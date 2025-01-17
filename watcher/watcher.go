package watcher

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/smartbch/moeingads/datatree"
	modbtypes "github.com/smartbch/moeingdb/types"
	evmtypes "github.com/smartbch/moeingevm/types"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/smartbch/smartbch/crosschain"
	cctypes "github.com/smartbch/smartbch/crosschain/types"
	"github.com/smartbch/smartbch/param"
	stakingtypes "github.com/smartbch/smartbch/staking/types"
	"github.com/smartbch/smartbch/watcher/types"
)

const (
	NumBlocksToClearMemory    = 1000
	WaitingBlockDelayTime     = 2
	waitingBlockDelayTime     = 2
	monitorInfoCleanThreshold = 5
)

var blockFinalizeNumber = int64(1) // 1 for test, 9 for product

type IContextGetter interface {
	GetRpcContext() *evmtypes.Context
}

// A watcher watches the new blocks generated on bitcoin cash's mainnet, and
// outputs epoch information through a channel
type Watcher struct {
	logger log.Logger

	rpcClient         types.RpcClient
	smartBchRpcClient types.RpcClient

	latestFinalizedHeight int64

	heightToFinalizedBlock map[int64]*types.BCHBlock

	catchupChan chan bool

	EpochChan chan *stakingtypes.Epoch
	// new monitor vote info always sent to app same time with epoch
	MonitorVoteChan     chan *cctypes.MonitorVoteInfo
	monitorVoteInfoList []*cctypes.MonitorVoteInfo

	voteInfoList []*types.VoteInfo

	numBlocksInEpoch   int64
	lastEpochEndHeight int64
	lastKnownEpochNum  int64

	waitingBlockDelayTime int
	parallelNum           int

	chainConfig *param.ChainConfig

	currentMainnetBlockTimestamp int64

	//executors
	CcContractExecutor *crosschain.CcContractExecutor
	txParser           types.CcTxParser

	contextGetter IContextGetter
}

func NewWatcher(logger log.Logger, historyDB modbtypes.DB, lastHeight, lastKnownEpochNum int64, chainConfig *param.ChainConfig) *Watcher {
	return &Watcher{
		logger: logger,

		rpcClient:         NewRpcClient(chainConfig.AppConfig.MainnetRPCUrl, chainConfig.AppConfig.MainnetRPCUsername, chainConfig.AppConfig.MainnetRPCPassword, "text/plain;", logger),
		smartBchRpcClient: NewRpcClient(chainConfig.AppConfig.SmartBchRPCUrl, "", "", "application/json", logger),

		lastEpochEndHeight:    lastHeight,
		latestFinalizedHeight: lastHeight,
		lastKnownEpochNum:     lastKnownEpochNum,

		catchupChan: make(chan bool, 1),

		heightToFinalizedBlock: make(map[int64]*types.BCHBlock),

		EpochChan:           make(chan *stakingtypes.Epoch, 10000),
		MonitorVoteChan:     make(chan *cctypes.MonitorVoteInfo, 5000),
		monitorVoteInfoList: make([]*cctypes.MonitorVoteInfo, 0, 10),

		voteInfoList: make([]*types.VoteInfo, 0, 10),

		numBlocksInEpoch:      param.StakingNumBlocksInEpoch,
		waitingBlockDelayTime: waitingBlockDelayTime,

		parallelNum: 10,
		chainConfig: chainConfig,
		// set big enough for single node startup when no BCH node connected. it will be updated when mainnet block finalize.
		currentMainnetBlockTimestamp: math.MaxInt64 - 14*24*3600,
		txParser: types.CcTxParser{
			DB: historyDB,
		},
	}
}

func (watcher *Watcher) SetRpcClient(client types.RpcClient) {
	watcher.rpcClient = client
}

func (watcher *Watcher) SetCCExecutor(exe *crosschain.CcContractExecutor) {
	watcher.CcContractExecutor = exe
}

func (watcher *Watcher) SetContextGetter(getter IContextGetter) {
	watcher.contextGetter = getter
}

func (watcher *Watcher) SetNumBlocksInEpoch(n int64) {
	watcher.numBlocksInEpoch = n
}

func (watcher *Watcher) SetWaitingBlockDelayTime(n int) {
	watcher.waitingBlockDelayTime = n
}

func (watcher *Watcher) WaitCatchup() {
	<-watcher.catchupChan
}

// The main function to do a watcher's job. It must be run as a goroutine
func (watcher *Watcher) Run() {
	if watcher.rpcClient == (*RpcClient)(nil) {
		watcher.catchupChan <- true // for ut
		return
	}
	watcher.speedup()
	if !param.IsAmber {
		go watcher.CollectCCTransferInfos()
	}
	watcher.fetchBlocks()
}

func (watcher *Watcher) fetchBlocks() {
	catchedUp := false
	latestMainnetHeight := watcher.rpcClient.GetLatestHeight(true)
	heightWanted := watcher.latestFinalizedHeight + 1
	// parallel fetch blocks when startup
	if heightWanted+blockFinalizeNumber+int64(watcher.parallelNum) <= latestMainnetHeight {
		watcher.logger.Debug("block parallel fetch info", "latestFinalizedHeight", watcher.latestFinalizedHeight, "latestMainnetHeight", latestMainnetHeight)
		watcher.parallelFetchBlocks(heightWanted, latestMainnetHeight-blockFinalizeNumber)
		heightWanted = watcher.latestFinalizedHeight + 1
	}
	// normal catchup
	for {
		latestMainnetHeight = watcher.rpcClient.GetLatestHeight(true)
		for heightWanted+blockFinalizeNumber <= latestMainnetHeight {
			watcher.addFinalizedBlock(watcher.rpcClient.GetBlockByHeight(heightWanted, true))
			heightWanted++
			latestMainnetHeight = watcher.rpcClient.GetLatestHeight(true)
		}
		if catchedUp {
			watcher.logger.Debug("waiting BCH mainnet", "height now is", latestMainnetHeight)
			watcher.suspended(time.Duration(watcher.waitingBlockDelayTime) * time.Second) //delay half of bch mainnet block intervals
		} else {
			watcher.logger.Debug("AlreadyCaughtUp")
			catchedUp = true
			close(watcher.catchupChan)
		}
	}
}

func (watcher *Watcher) parallelFetchBlocks(heightStart, heightEnd int64) {
	var blockSet = make([]*types.BCHBlock, heightEnd-heightStart+1)
	sharedIdx := int64(-1)
	datatree.ParallelRun(watcher.parallelNum, func(_ int) {
		for {
			index := atomic.AddInt64(&sharedIdx, 1)
			if heightStart+index > heightEnd {
				break
			}
			blockSet[index] = watcher.rpcClient.GetBlockByHeight(heightStart+index, true)
		}
	})
	for _, blk := range blockSet {
		watcher.addFinalizedBlock(blk)
	}
	watcher.logger.Debug("Get bch mainnet blocks parallel", "latestFinalizedHeight", watcher.latestFinalizedHeight)
}

func (watcher *Watcher) speedup() {
	if watcher.chainConfig.AppConfig.Speedup {
		start := uint64(watcher.lastKnownEpochNum) + 1
		for {
			infos := watcher.smartBchRpcClient.GetVoteInfoByEpochNumber(start, start+100)
			if len(infos) == 0 {
				break
			}
			watcher.voteInfoList = append(watcher.voteInfoList, infos...)
			for _, in := range infos {
				if in.Epoch.EndTime != 0 {
					watcher.EpochChan <- &in.Epoch
				}
				if !param.IsAmber && in.MonitorVote.EndTime != 0 {
					watcher.MonitorVoteChan <- &in.MonitorVote
				}
			}
			watcher.latestFinalizedHeight += int64(len(infos)) * watcher.numBlocksInEpoch
			start = start + uint64(len(infos))
		}
		watcher.lastEpochEndHeight = watcher.latestFinalizedHeight
		watcher.logger.Debug("After speedup", "latestFinalizedHeight", watcher.latestFinalizedHeight)
	}
}

func (watcher *Watcher) suspended(delayDuration time.Duration) {
	time.Sleep(delayDuration)
}

// Record new block and if the blocks for a new epoch is all ready, output the new epoch
func (watcher *Watcher) addFinalizedBlock(blk *types.BCHBlock) {
	watcher.heightToFinalizedBlock[blk.Height] = blk
	watcher.latestFinalizedHeight++
	watcher.currentMainnetBlockTimestamp = blk.Timestamp

	if watcher.latestFinalizedHeight-watcher.lastEpochEndHeight == watcher.numBlocksInEpoch {
		watcher.generateNewEpoch()
	}
}

// Generate a new block's information
func (watcher *Watcher) generateNewEpoch() {
	epoch := watcher.buildNewEpoch()
	watcher.logger.Debug("Generate new epoch", "epochNumber", epoch.Number, "startHeight", epoch.StartHeight)
	watcher.EpochChan <- epoch
	info := watcher.buildMonitorVoteInfo()
	if info != nil {
		watcher.MonitorVoteChan <- info
	}
	var voteInfo types.VoteInfo
	voteInfo.Epoch = *epoch
	if info != nil {
		voteInfo.MonitorVote = *info
	}
	watcher.voteInfoList = append(watcher.voteInfoList, &voteInfo)
	watcher.lastEpochEndHeight = watcher.latestFinalizedHeight
	watcher.ClearOldData()
}

func (watcher *Watcher) buildMonitorVoteInfo() *cctypes.MonitorVoteInfo {
	startHeight := watcher.lastEpochEndHeight + 1
	if startHeight < param.StartMainnetHeightForCC {
		return nil
	}
	var info cctypes.MonitorVoteInfo
	info.StartHeight = startHeight
	var monitorMapByPubkey = make(map[[33]byte]*cctypes.Nomination)

	for i := startHeight; i <= watcher.latestFinalizedHeight; i++ {
		blk, ok := watcher.heightToFinalizedBlock[i]
		if !ok {
			panic("Missing Block")
		}
		for _, ccNomination := range blk.CCNominations {
			if _, ok := monitorMapByPubkey[ccNomination.Pubkey]; !ok {
				monitorMapByPubkey[ccNomination.Pubkey] = &ccNomination
			}
			monitorMapByPubkey[ccNomination.Pubkey].NominatedCount += ccNomination.NominatedCount
		}
	}
	for _, v := range monitorMapByPubkey {
		info.Nominations = append(info.Nominations, v)
	}
	sortMonitorVoteNominations(info.Nominations)
	return &info
}

func sortMonitorVoteNominations(nominations []*cctypes.Nomination) {
	sort.Slice(nominations, func(i, j int) bool {
		return bytes.Compare(nominations[i].Pubkey[:], nominations[j].Pubkey[:]) < 0
	})
	sort.SliceStable(nominations, func(i, j int) bool {
		return nominations[i].NominatedCount > nominations[j].NominatedCount
	})
}

func (watcher *Watcher) buildNewEpoch() *stakingtypes.Epoch {
	epoch := &stakingtypes.Epoch{
		StartHeight: watcher.lastEpochEndHeight + 1,
		Nominations: make([]*stakingtypes.Nomination, 0, 10),
	}
	var valMapByPubkey = make(map[[32]byte]*stakingtypes.Nomination)
	for i := epoch.StartHeight; i <= watcher.latestFinalizedHeight; i++ {
		blk, ok := watcher.heightToFinalizedBlock[i]
		if !ok {
			panic("Missing Block")
		}
		//Please note that BCH's timestamp is not always linearly increasing
		if epoch.EndTime < blk.Timestamp {
			epoch.EndTime = blk.Timestamp
		}
		for _, nomination := range blk.Nominations {
			if _, ok := valMapByPubkey[nomination.Pubkey]; !ok {
				valMapByPubkey[nomination.Pubkey] = &nomination
			}
			valMapByPubkey[nomination.Pubkey].NominatedCount += nomination.NominatedCount
		}
	}
	for _, v := range valMapByPubkey {
		epoch.Nominations = append(epoch.Nominations, v)
	}
	sortEpochNominations(epoch)
	return epoch
}

func (watcher *Watcher) GetCurrEpoch() *stakingtypes.Epoch {
	return watcher.buildNewEpoch()
}
func (watcher *Watcher) GetEpochList() []*stakingtypes.Epoch {
	epochList := make([]*stakingtypes.Epoch, len(watcher.voteInfoList))
	for i, v := range watcher.voteInfoList {
		epochList[i] = stakingtypes.CopyEpoch(v.Epoch)
	}
	currEpoch := watcher.buildNewEpoch()
	return append(epochList, currEpoch)
}

func (watcher *Watcher) GetCurrMainnetBlockTimestamp() int64 {
	return watcher.currentMainnetBlockTimestamp
}

func (watcher *Watcher) GetLatestFinalizedHeight() int64 {
	return watcher.latestFinalizedHeight
}

func (watcher *Watcher) CheckSanity(skipCheck bool) {
	if !skipCheck {
		latestHeight := watcher.rpcClient.GetLatestHeight(false)
		if latestHeight <= 0 {
			panic("Watcher GetLatestHeight failed in Sanity Check")
		}
		blk := watcher.rpcClient.GetBlockByHeight(latestHeight, false)
		if blk == nil {
			panic("Watcher GetBlockByHeight failed in Sanity Check")
		}
	}
}

//sort by pubkey (small to big) first; then sort by nominationCount;
//so nominations sort by NominationCount, if count is equal, smaller pubkey stand front
func sortEpochNominations(epoch *stakingtypes.Epoch) {
	sort.Slice(epoch.Nominations, func(i, j int) bool {
		return bytes.Compare(epoch.Nominations[i].Pubkey[:], epoch.Nominations[j].Pubkey[:]) < 0
	})
	sort.SliceStable(epoch.Nominations, func(i, j int) bool {
		return epoch.Nominations[i].NominatedCount > epoch.Nominations[j].NominatedCount
	})
}

func (watcher *Watcher) ClearOldData() {
	vLen := len(watcher.voteInfoList)
	if vLen == 0 {
		return
	}
	height := watcher.voteInfoList[vLen-1].Epoch.StartHeight
	height -= 5 * watcher.numBlocksInEpoch
	if height <= 0 {
		return
	}
	for {
		_, ok := watcher.heightToFinalizedBlock[height]
		if !ok {
			break
		}
		delete(watcher.heightToFinalizedBlock, height)
		height--
	}
	if vLen > monitorInfoCleanThreshold /*param it*/ {
		watcher.voteInfoList = append([]*types.VoteInfo{}, watcher.voteInfoList[vLen-monitorInfoCleanThreshold:]...)
	}
}

func (watcher *Watcher) getUTXOCollectParam() *cctypes.UTXOCollectParam {
	ctx := watcher.contextGetter.GetRpcContext()
	defer ctx.Close(false)
	ccContext := crosschain.LoadCCContext(ctx)
	if ccContext == nil {
		return nil
	}
	return &cctypes.UTXOCollectParam{
		BeginHeight:            int64(ccContext.LastRescannedHeight),
		EndHeight:              int64(ccContext.RescanHeight),
		CurrentCovenantAddress: ccContext.CurrCovenantAddr,
		PrevCovenantAddress:    ccContext.LastCovenantAddr,
	}
}

func (watcher *Watcher) CollectCCTransferInfos() {
	var latestEndHeight int64
	var initCollect = true
	collectInterval := int64(1)
	for {
		time.Sleep(time.Duration(collectInterval) * time.Second)
		if watcher.latestFinalizedHeight < param.StartMainnetHeightForCC {
			continue
		}
		if watcher.CcContractExecutor == nil {
			continue
		}
		collectParam := watcher.getUTXOCollectParam()
		if collectParam == nil {
			continue
		}
		if collectParam.EndHeight == latestEndHeight || collectParam.BeginHeight == 0 {
			continue
		}
		watcher.CcContractExecutor.Lock.Lock()
		fmt.Printf("new collect round, beign:%d,end:%d\n", collectParam.BeginHeight, collectParam.EndHeight)
		latestEndHeight = collectParam.EndHeight
		var infos []*cctypes.CCTransferInfo
		blocks := watcher.getFinalizedBCHBlockInfos(collectParam.BeginHeight, collectParam.EndHeight)
		watcher.txParser.Refresh(collectParam.PrevCovenantAddress, collectParam.CurrentCovenantAddress)
		for _, bi := range blocks {
			infos = append(infos, watcher.txParser.GetCCUTXOTransferInfo(bi)...)
		}
		watcher.logger.Debug("collect cc infos", "BeginHeight", collectParam.BeginHeight, "EndHeight", collectParam.EndHeight, "length", len(infos))
		watcher.CcContractExecutor.Infos = infos
		watcher.CcContractExecutor.LastEndRescanBlock = uint64(latestEndHeight)
		watcher.CcContractExecutor.Lock.Unlock()
		if initCollect {
			close(watcher.CcContractExecutor.UTXOInitCollectDoneChan)
			initCollect = false
		}
	}
}

func (watcher *Watcher) getFinalizedBCHBlockInfos(startHeight, endHeight int64) (blocks []*types.BlockInfo) {
	if startHeight >= endHeight {
		watcher.logger.Debug("wrong startHeight and endHeight", "startHeight", startHeight, "endHeight", endHeight)
		return nil
	}
	latestHeight := watcher.rpcClient.GetLatestHeight(true)
	for latestHeight < endHeight+blockFinalizeNumber {
		time.Sleep(30 * time.Second)
		latestHeight = watcher.rpcClient.GetLatestHeight(true)
	}
	return watcher.getBCHBlockInfos(startHeight, endHeight)
}

// (startHeight, endHeight]
func (watcher *Watcher) getBCHBlockInfos(startHeight, endHeight int64) (blocks []*types.BlockInfo) {
	blocks = make([]*types.BlockInfo, endHeight-startHeight)
	sharedIdx := startHeight
	datatree.ParallelRun(10, func(_ int) {
		for {
			myIdx := atomic.AddInt64(&sharedIdx, 1)
			if myIdx > endHeight {
				break
			}
			blocks[myIdx-startHeight-1] = watcher.rpcClient.GetBlockInfoByHeight(myIdx, true)
		}
	})
	return
}
