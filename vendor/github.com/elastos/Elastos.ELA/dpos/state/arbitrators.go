// Copyright (c) 2017-2020 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//

package state

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/common/config"
	"github.com/elastos/Elastos.ELA/core/contract"
	"github.com/elastos/Elastos.ELA/core/contract/program"
	"github.com/elastos/Elastos.ELA/core/types"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/cr/state"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/events"
	"github.com/elastos/Elastos.ELA/utils"
)

type ChangeType byte

const (
	// MajoritySignRatioNumerator defines the ratio numerator to achieve
	// majority signatures.
	MajoritySignRatioNumerator = float64(2)

	// MajoritySignRatioDenominator defines the ratio denominator to achieve
	// majority signatures.
	MajoritySignRatioDenominator = float64(3)

	// MaxNormalInactiveChangesCount defines the max count arbitrators can
	// change when more than 1/3 arbiters don't sign cause to confirm fail
	MaxNormalInactiveChangesCount = 3

	// MaxSnapshotLength defines the max length the snapshot map should take
	MaxSnapshotLength = 20

	none         = ChangeType(0x00)
	updateNext   = ChangeType(0x01)
	normalChange = ChangeType(0x02)
)

var (
	ErrInsufficientProducer = errors.New("producers count less than min arbitrators count")
)

type ArbiterInfo struct {
	NodePublicKey   []byte
	IsNormal        bool
	IsCRMember      bool
	ClaimedDPOSNode bool
}

type arbitrators struct {
	*State
	*degradation
	chainParams      *config.Params
	crCommittee      *state.Committee
	bestHeight       func() uint32
	bestBlockHash    func() *common.Uint256
	getBlockByHeight func(uint32) (*types.Block, error)

	mtx       sync.Mutex
	started   bool
	dutyIndex int

	CurrentReward RewardData
	NextReward    RewardData

	currentArbitrators []ArbiterMember
	currentCandidates  []ArbiterMember
	nextArbitrators    []ArbiterMember
	nextCandidates     []ArbiterMember

	// current cr arbiters map
	currentCRCArbitersMap map[common.Uint168]ArbiterMember
	// next cr arbiters map
	nextCRCArbitersMap map[common.Uint168]ArbiterMember
	// next cr arbiters
	nextCRCArbiters []ArbiterMember

	crcChangedHeight           uint32
	accumulativeReward         common.Fixed64
	finalRoundChange           common.Fixed64
	clearingHeight             uint32
	arbitersRoundReward        map[common.Uint168]common.Fixed64
	illegalBlocksPayloadHashes map[common.Uint256]interface{}

	snapshots        map[uint32][]*CheckPoint
	snapshotKeysDesc []uint32

	forceChanged bool

	history *utils.History
}

func (a *arbitrators) Start() {
	a.mtx.Lock()
	a.started = true
	a.mtx.Unlock()
}

func (a *arbitrators) SetNeedRevertToDPOSTX(need bool) {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	a.NeedRevertToDPOSTX = need
}

func (a *arbitrators) IsInPOWMode() bool {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	return a.ConsensusAlgorithm == POW

}

func (a *arbitrators) GetRevertToPOWBlockHeight() uint32 {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	return a.RevertToPOWBlockHeight
}

func (a *arbitrators) RegisterFunction(bestHeight func() uint32,
	bestBlockHash func() *common.Uint256,
	getBlockByHeight func(uint32) (*types.Block, error),
	getTxReference func(tx *types.Transaction) (
		map[*types.Input]types.Output, error)) {
	a.bestHeight = bestHeight
	a.bestBlockHash = bestBlockHash
	a.getBlockByHeight = getBlockByHeight
	a.getTxReference = getTxReference
}

func (a *arbitrators) IsNeedNextTurnDPOSInfo() bool {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	return a.NeedNextTurnDPOSInfo
}

func (a *arbitrators) RecoverFromCheckPoints(point *CheckPoint) {
	a.mtx.Lock()
	a.recoverFromCheckPoints(point)
	a.mtx.Unlock()
}

func (a *arbitrators) recoverFromCheckPoints(point *CheckPoint) {
	a.dutyIndex = point.DutyIndex
	a.currentArbitrators = point.CurrentArbitrators
	a.currentCandidates = point.CurrentCandidates
	a.nextArbitrators = point.NextArbitrators
	a.nextCandidates = point.NextCandidates
	a.CurrentReward = point.CurrentReward
	a.NextReward = point.NextReward
	a.StateKeyFrame = &point.StateKeyFrame
	a.accumulativeReward = point.accumulativeReward
	a.finalRoundChange = point.finalRoundChange
	a.clearingHeight = point.clearingHeight
	a.arbitersRoundReward = point.arbitersRoundReward
	a.illegalBlocksPayloadHashes = point.illegalBlocksPayloadHashes

	a.crcChangedHeight = point.crcChangedHeight
	a.currentCRCArbitersMap = point.CurrentCRCArbitersMap
	a.nextCRCArbitersMap = point.NextCRCArbitersMap
	a.nextCRCArbiters = point.NextCRCArbiters
	a.forceChanged = point.forceChanged
}

func (a *arbitrators) ProcessBlock(block *types.Block, confirm *payload.Confirm) {
	a.State.ProcessBlock(block, confirm)
	a.IncreaseChainHeight(block)
}

func (a *arbitrators) CheckDPOSIllegalTx(block *types.Block) error {

	a.mtx.Lock()
	hashes := a.illegalBlocksPayloadHashes
	a.mtx.Unlock()

	if hashes == nil || len(hashes) == 0 {
		return nil
	}

	foundMap := make(map[common.Uint256]bool)
	for k := range hashes {
		foundMap[k] = false
	}

	for _, tx := range block.Transactions {
		if tx.IsIllegalBlockTx() {
			foundMap[tx.Payload.(*payload.DPOSIllegalBlocks).Hash()] = true
		}
	}

	for _, found := range foundMap {
		if !found {
			return errors.New("expect an illegal blocks transaction in this block")
		}
	}
	return nil
}

func (a *arbitrators) CheckRevertToDPOSTX(block *types.Block) error {
	a.mtx.Lock()
	needRevertToDPOSTX := a.NeedRevertToDPOSTX
	a.mtx.Unlock()

	var revertToDPOSTxCount uint32
	for _, tx := range block.Transactions {
		if tx.IsRevertToDPOS() {
			revertToDPOSTxCount++
		}
	}

	var needRevertToDPOSTXCount uint32
	if needRevertToDPOSTX {
		needRevertToDPOSTXCount = 1
	}

	if revertToDPOSTxCount != needRevertToDPOSTXCount {
		return fmt.Errorf("current block height %d, RevertToDPOSTX "+
			"transaction count should be %d, current block contains %d",
			block.Height, needRevertToDPOSTXCount, revertToDPOSTxCount)
	}

	return nil
}

func (a *arbitrators) CheckNextTurnDPOSInfoTx(block *types.Block) error {
	a.mtx.Lock()
	needNextTurnDposInfo := a.NeedNextTurnDPOSInfo
	a.mtx.Unlock()

	var nextTurnDPOSInfoTxCount uint32
	for _, tx := range block.Transactions {
		if tx.IsNextTurnDPOSInfoTx() {
			nextTurnDPOSInfoTxCount++
		}
	}

	var needNextTurnDPOSInfoCount uint32
	if needNextTurnDposInfo {
		needNextTurnDPOSInfoCount = 1
	}

	if nextTurnDPOSInfoTxCount != needNextTurnDPOSInfoCount {
		return fmt.Errorf("current block height %d, NextTurnDPOSInfo "+
			"transaction count should be %d, current block contains %d",
			block.Height, needNextTurnDPOSInfoCount, nextTurnDPOSInfoTxCount)
	}

	return nil
}

func (a *arbitrators) CheckCRCAppropriationTx(block *types.Block) error {
	a.mtx.Lock()
	needAppropriation := a.crCommittee.NeedAppropriation
	a.mtx.Unlock()

	var appropriationCount uint32
	for _, tx := range block.Transactions {
		if tx.IsCRCAppropriationTx() {
			appropriationCount++
		}
	}

	var needAppropriationCount uint32
	if needAppropriation {
		needAppropriationCount = 1
	}

	if appropriationCount != needAppropriationCount {
		return fmt.Errorf("current block height %d, appropriation "+
			"transaction count should be %d, current block contains %d",
			block.Height, needAppropriationCount, appropriationCount)
	}

	return nil
}

func (a *arbitrators) CheckCustomIDResultsTx(block *types.Block) error {
	a.mtx.Lock()
	needCustomProposalResult := a.crCommittee.NeedRecordProposalResult
	a.mtx.Unlock()

	var cidProposalResultCount uint32
	for _, tx := range block.Transactions {
		if tx.IsCustomIDResultTx() {
			cidProposalResultCount++
		}
	}

	var needCIDProposalResultCount uint32
	if needCustomProposalResult {
		needCIDProposalResultCount = 1
	}

	if cidProposalResultCount != needCIDProposalResultCount {
		return fmt.Errorf("current block height %d, custom ID result "+
			"transaction count should be %d, current block contains %d",
			block.Height, needCIDProposalResultCount, cidProposalResultCount)
	}

	return nil
}

func (a *arbitrators) ProcessSpecialTxPayload(p types.Payload,
	height uint32) error {
	switch obj := p.(type) {
	case *payload.DPOSIllegalBlocks:
		a.mtx.Lock()
		a.illegalBlocksPayloadHashes[obj.Hash()] = nil
		a.mtx.Unlock()
	case *payload.InactiveArbitrators:
		if !a.AddInactivePayload(obj) {
			log.Debug("[ProcessSpecialTxPayload] duplicated payload")
			return nil
		}
	default:
		return errors.New("[ProcessSpecialTxPayload] invalid payload type")
	}

	a.State.ProcessSpecialTxPayload(p, height)
	return a.ForceChange(height)
}

func (a *arbitrators) RollbackSeekTo(height uint32) {
	a.mtx.Lock()
	a.history.RollbackSeekTo(height)
	a.State.RollbackSeekTo(height)
	a.mtx.Unlock()
}

func (a *arbitrators) RollbackTo(height uint32) error {
	a.mtx.Lock()
	a.history.RollbackTo(height)
	a.degradation.RollbackTo(height)
	err := a.State.RollbackTo(height)
	a.mtx.Unlock()

	return err
}

func (a *arbitrators) GetDutyIndexByHeight(height uint32) (index int) {
	a.mtx.Lock()
	if height >= a.chainParams.DPOSNodeCrossChainHeight {
		if len(a.currentArbitrators) == 0 {
			index = 0
		} else {
			index = a.dutyIndex % len(a.currentArbitrators)
		}
	} else if height >= a.chainParams.CRClaimDPOSNodeStartHeight {
		if len(a.currentCRCArbitersMap) == 0 {
			index = 0
		} else {
			index = a.dutyIndex % len(a.currentCRCArbitersMap)
		}
	} else if height >= a.chainParams.CRCOnlyDPOSHeight-1 {
		if len(a.currentCRCArbitersMap) == 0 {
			index = 0
		} else {
			index = int(height-a.chainParams.CRCOnlyDPOSHeight+1) % len(a.currentCRCArbitersMap)
		}
	} else {
		if len(a.currentArbitrators) == 0 {
			index = 0
		} else {
			index = int(height) % len(a.currentArbitrators)
		}
	}
	a.mtx.Unlock()
	return index
}

func (a *arbitrators) GetDutyIndex() int {
	a.mtx.Lock()
	index := a.dutyIndex
	a.mtx.Unlock()

	return index
}

func (a *arbitrators) GetArbitersRoundReward() map[common.Uint168]common.Fixed64 {
	a.mtx.Lock()
	result := a.arbitersRoundReward
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetFinalRoundChange() common.Fixed64 {
	a.mtx.Lock()
	result := a.finalRoundChange
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetLastBlockTimestamp() uint32 {
	a.mtx.Lock()
	result := a.LastBlockTimestamp
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) ForceChange(height uint32) error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	return a.forceChange(height)
}

func (a *arbitrators) forceChange(height uint32) error {
	block, err := a.getBlockByHeight(height)
	if err != nil {
		block, err = a.getBlockByHeight(a.bestHeight())
		if err != nil {
			return err
		}
	}
	a.snapshot(height)

	if err := a.clearingDPOSReward(block, height, false); err != nil {
		return err
	}

	if err := a.updateNextArbitrators(height+1, height); err != nil {
		log.Info("force change failed at height:", height)
		return err
	}

	if err := a.changeCurrentArbitrators(height); err != nil {
		return err
	}

	if a.started {
		go events.Notify(events.ETDirectPeersChanged,
			a.getNeedConnectArbiters())
	}
	oriForceChanged := a.forceChanged
	a.history.Append(height, func() {
		a.forceChanged = true
	}, func() {
		a.forceChanged = oriForceChanged
	})
	a.history.Commit(height)

	a.dumpInfo(height)
	return nil
}

func (a *arbitrators) tryHandleError(height uint32, err error) error {
	if err == ErrInsufficientProducer {
		log.Warn("found error: ", err, ", degrade to CRC only state")
		a.TrySetUnderstaffed(height)
		return nil
	} else {
		return err
	}
}

func (a *arbitrators) normalChange(height uint32) error {
	if err := a.changeCurrentArbitrators(height); err != nil {
		log.Warn("[NormalChange] change current arbiters error: ", err)
		return err
	}
	if err := a.updateNextArbitrators(height+1, height); err != nil {
		log.Warn("[NormalChange] update next arbiters error: ", err)
		return err
	}
	return nil
}

func (a *arbitrators) notifyNextTurnDPOSInfoTx(blockHeight, versionHeight uint32, forceChange bool) {
	nextTurnDPOSInfoTx := a.createNextTurnDPOSInfoTransaction(blockHeight, forceChange)
	go events.Notify(events.ETAppendTxToTxPool, nextTurnDPOSInfoTx)
	return
}

func (a *arbitrators) IncreaseChainHeight(block *types.Block) {
	var notify = true
	var snapshotVotes = true
	a.mtx.Lock()

	var containsIllegalBlockEvidence bool
	for _, tx := range block.Transactions {
		if tx.IsIllegalBlockTx() {
			containsIllegalBlockEvidence = true
			break
		}
	}
	var forceChanged bool
	if containsIllegalBlockEvidence {
		if err := a.forceChange(block.Height); err != nil {
			log.Errorf("Found illegal blocks, ForceChange failed:%s", err)
			a.cleanArbitrators(block.Height)
			a.revertToPOWAtNextTurn(block.Height)
			log.Warn(fmt.Sprintf("force change fail at height: %d, error: %s",
				block.Height, err))
		}
		forceChanged = true
	} else {
		changeType, versionHeight := a.getChangeType(block.Height + 1)
		switch changeType {
		case updateNext:
			if err := a.updateNextArbitrators(versionHeight, block.Height); err != nil {
				a.revertToPOWAtNextTurn(block.Height)
				log.Warn(fmt.Sprintf("update next arbiters at height: %d, "+
					"error: %s, revert to POW mode", block.Height, err))
			}
		case normalChange:
			if err := a.clearingDPOSReward(block, block.Height, true); err != nil {
				panic(fmt.Sprintf("normal change fail when clear DPOS reward: "+
					" transaction, height: %d, error: %s", block.Height, err))
			}
			if err := a.normalChange(block.Height); err != nil {
				a.revertToPOWAtNextTurn(block.Height)
				log.Warn(fmt.Sprintf("normal change fail at height: %d, "+
					"error: %s， revert to POW mode", block.Height, err))
			}
		case none:
			a.accumulateReward(block)
			notify = false
			snapshotVotes = false
		}
	}

	oriIllegalBlocks := a.illegalBlocksPayloadHashes
	a.history.Append(block.Height, func() {
		a.illegalBlocksPayloadHashes = make(map[common.Uint256]interface{})
	}, func() {
		a.illegalBlocksPayloadHashes = oriIllegalBlocks
	})
	a.history.Commit(block.Height)
	bestHeight := a.bestHeight()
	if a.ConsensusAlgorithm != POW && block.Height >= bestHeight {
		if len(a.currentArbitrators) == 0 && (a.NoClaimDPOSNode || a.NoProducers) {
			a.createRevertToPOWTransaction(block.Height)
		}
	}
	if snapshotVotes {
		if err := a.snapshotVotesStates(block.Height); err != nil {
			panic(fmt.Sprintf("snap shot votes states error:%s", err))
		}
		a.history.Commit(block.Height)
	}
	if block.Height > bestHeight-MaxSnapshotLength {
		a.snapshot(block.Height)
	}
	if block.Height >= bestHeight && (a.NeedNextTurnDPOSInfo || forceChanged) {
		a.notifyNextTurnDPOSInfoTx(block.Height, block.Height+1, forceChanged)
	}
	a.mtx.Unlock()
	if a.started && notify {
		go events.Notify(events.ETDirectPeersChanged, a.GetNeedConnectArbiters())
	}
}

func (a *arbitrators) createRevertToPOWTransaction(blockHeight uint32) {
	var revertType payload.RevertType
	if a.NoClaimDPOSNode {
		revertType = payload.NoClaimDPOSNode
	} else {
		revertType = payload.NoProducers
	}
	revertToPOWPayload := payload.RevertToPOW{
		Type:          revertType,
		WorkingHeight: blockHeight + 1,
	}
	tx := &types.Transaction{
		Version:        types.TxVersion09,
		TxType:         types.RevertToPOW,
		PayloadVersion: payload.RevertToPOWVersion,
		Payload:        &revertToPOWPayload,
		Attributes:     []*types.Attribute{},
		Programs:       []*program.Program{},
		LockTime:       0,
	}
	go events.Notify(events.ETAppendTxToTxPoolWithoutRelay, tx)
}

func (a *arbitrators) revertToPOWAtNextTurn(height uint32) {
	oriNextArbitrators := a.nextArbitrators
	oriNextCandidates := a.nextCandidates
	oriNextCRCArbitersMap := a.nextCRCArbitersMap
	oriNextCRCArbiters := a.nextCRCArbiters
	oriNoProducers := a.NoProducers

	a.history.Append(height, func() {
		a.nextArbitrators = make([]ArbiterMember, 0)
		a.nextCandidates = make([]ArbiterMember, 0)
		a.nextCRCArbitersMap = make(map[common.Uint168]ArbiterMember)
		a.nextCRCArbiters = make([]ArbiterMember, 0)
		if a.ConsensusAlgorithm == DPOS {
			a.NoProducers = true
		}
	}, func() {
		a.nextArbitrators = oriNextArbitrators
		a.nextCandidates = oriNextCandidates
		a.nextCRCArbitersMap = oriNextCRCArbitersMap
		a.nextCRCArbiters = oriNextCRCArbiters
		a.NoProducers = oriNoProducers
	})
}

func (a *arbitrators) accumulateReward(block *types.Block) {
	if block.Height < a.chainParams.PublicDPOSHeight {
		oriDutyIndex := a.dutyIndex
		a.history.Append(block.Height, func() {
			a.dutyIndex = oriDutyIndex + 1
		}, func() {
			a.dutyIndex = oriDutyIndex
		})
		return
	}

	var accumulative common.Fixed64
	accumulative = a.accumulativeReward
	if block.Height < a.chainParams.CRVotingStartHeight || !a.forceChanged {
		dposReward := a.getBlockDPOSReward(block)
		accumulative += dposReward
	}

	oriAccumulativeReward := a.accumulativeReward
	oriArbitersRoundReward := a.arbitersRoundReward
	oriFinalRoundChange := a.finalRoundChange
	oriForceChanged := a.forceChanged
	oriDutyIndex := a.dutyIndex
	a.history.Append(block.Height, func() {
		a.accumulativeReward = accumulative
		a.arbitersRoundReward = nil
		a.finalRoundChange = 0
		a.forceChanged = false
		a.dutyIndex = oriDutyIndex + 1
	}, func() {
		a.accumulativeReward = oriAccumulativeReward
		a.arbitersRoundReward = oriArbitersRoundReward
		a.finalRoundChange = oriFinalRoundChange
		a.forceChanged = oriForceChanged
		a.dutyIndex = oriDutyIndex
	})

}

func (a *arbitrators) clearingDPOSReward(block *types.Block, historyHeight uint32,
	smoothClearing bool) (err error) {
	if block.Height < a.chainParams.PublicDPOSHeight ||
		block.Height == a.clearingHeight {
		return nil
	}

	dposReward := a.getBlockDPOSReward(block)
	accumulativeReward := a.accumulativeReward
	if smoothClearing {
		accumulativeReward += dposReward
		dposReward = 0
	}

	var change common.Fixed64
	var roundReward map[common.Uint168]common.Fixed64
	if roundReward, change, err = a.distributeDPOSReward(block.Height,
		accumulativeReward); err != nil {
		return
	}

	oriRoundReward := a.arbitersRoundReward
	oriAccumulativeReward := a.accumulativeReward
	oriClearingHeight := a.clearingHeight
	oriChange := a.finalRoundChange
	a.history.Append(historyHeight, func() {
		a.arbitersRoundReward = roundReward
		a.accumulativeReward = dposReward
		a.clearingHeight = block.Height
		a.finalRoundChange = change
	}, func() {
		a.arbitersRoundReward = oriRoundReward
		a.accumulativeReward = oriAccumulativeReward
		a.clearingHeight = oriClearingHeight
		a.finalRoundChange = oriChange
	})

	return nil
}

func (a *arbitrators) distributeDPOSReward(height uint32,
	reward common.Fixed64) (roundReward map[common.Uint168]common.Fixed64,
	change common.Fixed64, err error) {
	var realDPOSReward common.Fixed64
	if height >= a.chainParams.ChangeCommitteeNewCRHeight+2*uint32(len(a.currentArbitrators)) {
		roundReward, realDPOSReward, err = a.distributeWithNormalArbitratorsV3(height, reward)
	} else if height >= a.chainParams.CRClaimDPOSNodeStartHeight+2*uint32(len(a.currentArbitrators)) {
		roundReward, realDPOSReward, err = a.distributeWithNormalArbitratorsV2(height, reward)
	} else if height >= a.chainParams.CRCommitteeStartHeight+2*uint32(len(a.currentArbitrators)) {
		roundReward, realDPOSReward, err = a.distributeWithNormalArbitratorsV1(height, reward)
	} else {
		roundReward, realDPOSReward, err = a.distributeWithNormalArbitratorsV0(reward)
	}

	if err != nil {
		return nil, 0, err
	}

	change = reward - realDPOSReward
	if change < 0 {
		log.Error("reward:", reward, "realDPOSReward:", realDPOSReward, "height:", height,
			"b", a.chainParams.CRClaimDPOSNodeStartHeight+2*uint32(len(a.currentArbitrators)),
			"c", a.chainParams.CRCommitteeStartHeight+2*uint32(len(a.currentArbitrators)))
		return nil, 0, errors.New("real dpos reward more than reward limit")
	}

	return
}

func (a *arbitrators) distributeWithNormalArbitratorsV3(height uint32, reward common.Fixed64) (
	map[common.Uint168]common.Fixed64, common.Fixed64, error) {
	//if len(a.currentArbitrators) == 0 {
	//	return nil, 0, errors.New("not found arbiters when " +
	//		"distributeWithNormalArbitratorsV3")
	//}

	roundReward := map[common.Uint168]common.Fixed64{}
	totalBlockConfirmReward := float64(reward) * 0.25
	totalTopProducersReward := float64(reward) - totalBlockConfirmReward
	// Consider that there is no only CR consensus.
	arbitersCount := len(a.chainParams.CRCArbiters) + a.chainParams.GeneralArbiters
	individualBlockConfirmReward := common.Fixed64(
		math.Floor(totalBlockConfirmReward / float64(arbitersCount)))
	totalVotesInRound := a.CurrentReward.TotalVotesInRound
	if a.ConsensusAlgorithm == POW || len(a.currentArbitrators) == 0 ||
		len(a.chainParams.CRCArbiters) == len(a.currentArbitrators) {
		// if no normal DPOS node, need to destroy reward.
		roundReward[a.chainParams.DestroyELAAddress] = reward
		return roundReward, reward, nil
	}
	rewardPerVote := totalTopProducersReward / float64(totalVotesInRound)

	realDPOSReward := common.Fixed64(0)
	for _, arbiter := range a.currentArbitrators {
		ownerHash := arbiter.GetOwnerProgramHash()
		rewardHash := ownerHash
		var r common.Fixed64
		if arbiter.GetType() == CRC {
			r = individualBlockConfirmReward
			m, ok := arbiter.(*crcArbiter)
			if !ok || m.crMember.MemberState != state.MemberElected {
				rewardHash = a.chainParams.DestroyELAAddress
			} else if len(m.crMember.DPOSPublicKey) == 0 {
				nodePK := arbiter.GetNodePublicKey()
				ownerPK := a.getProducerKey(nodePK)
				opk, err := common.HexStringToBytes(ownerPK)
				if err != nil {
					panic("get owner public key err:" + err.Error())
				}
				programHash, err := contract.PublicKeyToStandardProgramHash(opk)
				if err != nil {
					panic("public key to standard program hash err:" + err.Error())
				}
				votes := a.CurrentReward.OwnerVotesInRound[*programHash]
				individualCRCProducerReward := common.Fixed64(math.Floor(float64(
					votes) * rewardPerVote))
				r = individualBlockConfirmReward + individualCRCProducerReward
				rewardHash = *programHash
			} else {
				pk := arbiter.GetOwnerPublicKey()
				programHash, err := contract.PublicKeyToStandardProgramHash(pk)
				if err != nil {
					rewardHash = a.chainParams.DestroyELAAddress
				} else {
					rewardHash = *programHash
				}
			}
		} else {
			votes := a.CurrentReward.OwnerVotesInRound[ownerHash]
			individualProducerReward := common.Fixed64(math.Floor(float64(
				votes) * rewardPerVote))
			r = individualBlockConfirmReward + individualProducerReward
		}
		roundReward[rewardHash] += r
		realDPOSReward += r
	}
	for _, candidate := range a.currentCandidates {
		ownerHash := candidate.GetOwnerProgramHash()
		votes := a.CurrentReward.OwnerVotesInRound[ownerHash]
		individualProducerReward := common.Fixed64(math.Floor(float64(
			votes) * rewardPerVote))
		roundReward[ownerHash] = individualProducerReward

		realDPOSReward += individualProducerReward
	}
	// Abnormal CR`s reward need to be destroyed.
	for i := len(a.currentArbitrators); i < arbitersCount; i++ {
		roundReward[a.chainParams.DestroyELAAddress] += individualBlockConfirmReward
	}
	return roundReward, realDPOSReward, nil
}

func (a *arbitrators) distributeWithNormalArbitratorsV2(height uint32, reward common.Fixed64) (
	map[common.Uint168]common.Fixed64, common.Fixed64, error) {
	//if len(a.currentArbitrators) == 0 {
	//	return nil, 0, errors.New("not found arbiters when " +
	//		"distributeWithNormalArbitratorsV2")
	//}

	roundReward := map[common.Uint168]common.Fixed64{}
	totalBlockConfirmReward := float64(reward) * 0.25
	totalTopProducersReward := float64(reward) - totalBlockConfirmReward
	// Consider that there is no only CR consensus.
	arbitersCount := len(a.chainParams.CRCArbiters) + a.chainParams.GeneralArbiters
	individualBlockConfirmReward := common.Fixed64(
		math.Floor(totalBlockConfirmReward / float64(arbitersCount)))
	totalVotesInRound := a.CurrentReward.TotalVotesInRound
	if len(a.currentArbitrators) == 0 ||
		len(a.chainParams.CRCArbiters) == len(a.currentArbitrators) {
		//if len(a.chainParams.CRCArbiters) == len(a.currentArbitrators) {
		// if no normal DPOS node, need to destroy reward.
		roundReward[a.chainParams.DestroyELAAddress] = reward
		return roundReward, reward, nil
	}
	rewardPerVote := totalTopProducersReward / float64(totalVotesInRound)

	realDPOSReward := common.Fixed64(0)
	for _, arbiter := range a.currentArbitrators {
		ownerHash := arbiter.GetOwnerProgramHash()
		rewardHash := ownerHash
		var r common.Fixed64
		if _, ok := a.currentCRCArbitersMap[ownerHash]; ok {
			r = individualBlockConfirmReward
			m, ok := arbiter.(*crcArbiter)
			if !ok || m.crMember.MemberState != state.MemberElected || len(m.crMember.DPOSPublicKey) == 0 {
				rewardHash = a.chainParams.DestroyELAAddress
			} else {
				pk := arbiter.GetOwnerPublicKey()
				programHash, err := contract.PublicKeyToStandardProgramHash(pk)
				if err != nil {
					rewardHash = a.chainParams.DestroyELAAddress
				} else {
					rewardHash = *programHash
				}
			}
		} else {
			votes := a.CurrentReward.OwnerVotesInRound[ownerHash]
			individualProducerReward := common.Fixed64(math.Floor(float64(
				votes) * rewardPerVote))
			r = individualBlockConfirmReward + individualProducerReward
		}
		roundReward[rewardHash] += r
		realDPOSReward += r
	}
	for _, candidate := range a.currentCandidates {
		ownerHash := candidate.GetOwnerProgramHash()
		votes := a.CurrentReward.OwnerVotesInRound[ownerHash]
		individualProducerReward := common.Fixed64(math.Floor(float64(
			votes) * rewardPerVote))
		roundReward[ownerHash] = individualProducerReward

		realDPOSReward += individualProducerReward
	}
	// Abnormal CR`s reward need to be destroyed.
	for i := len(a.currentArbitrators); i < arbitersCount; i++ {
		roundReward[a.chainParams.DestroyELAAddress] += individualBlockConfirmReward
	}
	return roundReward, realDPOSReward, nil
}

func (a *arbitrators) distributeWithNormalArbitratorsV1(height uint32, reward common.Fixed64) (
	map[common.Uint168]common.Fixed64, common.Fixed64, error) {
	if len(a.currentArbitrators) == 0 {
		return nil, 0, errors.New("not found arbiters when " +
			"distributeWithNormalArbitratorsV1")
	}

	roundReward := map[common.Uint168]common.Fixed64{}
	totalBlockConfirmReward := float64(reward) * 0.25
	totalTopProducersReward := float64(reward) - totalBlockConfirmReward
	individualBlockConfirmReward := common.Fixed64(
		math.Floor(totalBlockConfirmReward / float64(len(a.currentArbitrators))))
	totalVotesInRound := a.CurrentReward.TotalVotesInRound
	if len(a.chainParams.CRCArbiters) == len(a.currentArbitrators) {
		roundReward[a.chainParams.CRCAddress] = reward
		return roundReward, reward, nil
	}
	rewardPerVote := totalTopProducersReward / float64(totalVotesInRound)
	realDPOSReward := common.Fixed64(0)
	for _, arbiter := range a.currentArbitrators {
		ownerHash := arbiter.GetOwnerProgramHash()
		rewardHash := ownerHash
		var r common.Fixed64
		if _, ok := a.currentCRCArbitersMap[ownerHash]; ok {
			r = individualBlockConfirmReward
			m, ok := arbiter.(*crcArbiter)
			if !ok || m.crMember.MemberState != state.MemberElected {
				rewardHash = a.chainParams.DestroyELAAddress
			} else {
				pk := arbiter.GetOwnerPublicKey()
				programHash, err := contract.PublicKeyToStandardProgramHash(pk)
				if err != nil {
					rewardHash = a.chainParams.DestroyELAAddress
				} else {
					rewardHash = *programHash
				}
			}
		} else {
			votes := a.CurrentReward.OwnerVotesInRound[ownerHash]
			individualProducerReward := common.Fixed64(math.Floor(float64(
				votes) * rewardPerVote))
			r = individualBlockConfirmReward + individualProducerReward
		}
		roundReward[rewardHash] += r
		realDPOSReward += r
	}
	for _, candidate := range a.currentCandidates {
		ownerHash := candidate.GetOwnerProgramHash()
		votes := a.CurrentReward.OwnerVotesInRound[ownerHash]
		individualProducerReward := common.Fixed64(math.Floor(float64(
			votes) * rewardPerVote))
		roundReward[ownerHash] = individualProducerReward

		realDPOSReward += individualProducerReward
	}
	return roundReward, realDPOSReward, nil
}

func (a *arbitrators) GetNeedConnectArbiters() []peer.PID {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	return a.getNeedConnectArbiters()
}

func (a *arbitrators) getNeedConnectArbiters() []peer.PID {
	height := a.history.Height() + 1
	if height < a.chainParams.CRCOnlyDPOSHeight-a.chainParams.PreConnectOffset {
		return nil
	}

	pids := make(map[string]peer.PID)
	for _, p := range a.currentCRCArbitersMap {
		abt, ok := p.(*crcArbiter)
		if !ok || abt.crMember.MemberState != state.MemberElected {
			continue
		}
		var pid peer.PID
		copy(pid[:], p.GetNodePublicKey())
		pids[common.BytesToHexString(p.GetNodePublicKey())] = pid
	}

	for _, p := range a.nextCRCArbitersMap {
		abt, ok := p.(*crcArbiter)
		if !ok || abt.crMember.MemberState != state.MemberElected {
			continue
		}
		var pid peer.PID
		copy(pid[:], p.GetNodePublicKey())
		pids[common.BytesToHexString(p.GetNodePublicKey())] = pid
	}

	if height != a.chainParams.CRCOnlyDPOSHeight-
		a.chainParams.PreConnectOffset {
		for _, v := range a.currentArbitrators {
			key := common.BytesToHexString(v.GetNodePublicKey())
			var pid peer.PID
			copy(pid[:], v.GetNodePublicKey())
			pids[key] = pid
		}
	}

	for _, v := range a.nextArbitrators {
		key := common.BytesToHexString(v.GetNodePublicKey())
		var pid peer.PID
		copy(pid[:], v.GetNodePublicKey())
		pids[key] = pid
	}

	peers := make([]peer.PID, 0, len(pids))
	for _, pid := range pids {
		peers = append(peers, pid)
	}

	return peers
}

func (a *arbitrators) IsArbitrator(pk []byte) bool {
	arbitrators := a.GetArbitrators()

	for _, v := range arbitrators {
		if !v.IsNormal {
			continue
		}
		if bytes.Equal(pk, v.NodePublicKey) {
			return true
		}
	}
	return false
}

func (a *arbitrators) GetArbitrators() []*ArbiterInfo {
	a.mtx.Lock()
	result := a.getArbitrators()
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) getArbitrators() []*ArbiterInfo {
	result := make([]*ArbiterInfo, 0, len(a.currentArbitrators))
	for _, v := range a.currentArbitrators {
		isNormal := true
		isCRMember := false
		claimedDPOSNode := false
		abt, ok := v.(*crcArbiter)
		if ok {
			isCRMember = true
			if !abt.isNormal {
				isNormal = false
			}
			if len(abt.crMember.DPOSPublicKey) != 0 {
				claimedDPOSNode = true
			}
		}
		result = append(result, &ArbiterInfo{
			NodePublicKey:   v.GetNodePublicKey(),
			IsNormal:        isNormal,
			IsCRMember:      isCRMember,
			ClaimedDPOSNode: claimedDPOSNode,
		})
	}
	return result
}

func (a *arbitrators) GetCandidates() [][]byte {
	a.mtx.Lock()
	result := make([][]byte, 0, len(a.currentCandidates))
	for _, v := range a.currentCandidates {
		result = append(result, v.GetNodePublicKey())
	}
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetNextArbitrators() []*ArbiterInfo {
	a.mtx.Lock()
	result := make([]*ArbiterInfo, 0, len(a.nextArbitrators))
	for _, v := range a.nextArbitrators {
		isNormal := true
		isCRMember := false
		claimedDPOSNode := false
		abt, ok := v.(*crcArbiter)
		if ok {
			isCRMember = true
			if !abt.isNormal {
				isNormal = false
			}
			if len(abt.crMember.DPOSPublicKey) != 0 {
				claimedDPOSNode = true
			}
		}
		result = append(result, &ArbiterInfo{
			NodePublicKey:   v.GetNodePublicKey(),
			IsNormal:        isNormal,
			IsCRMember:      isCRMember,
			ClaimedDPOSNode: claimedDPOSNode,
		})
	}
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetNextCandidates() [][]byte {
	a.mtx.Lock()
	result := make([][]byte, 0, len(a.nextCandidates))
	for _, v := range a.nextCandidates {
		result = append(result, v.GetNodePublicKey())
	}
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetCRCArbiters() []*ArbiterInfo {
	a.mtx.Lock()
	result := a.getCRCArbiters()
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) getCRCArbiters() []*ArbiterInfo {
	result := make([]*ArbiterInfo, 0, len(a.currentCRCArbitersMap))
	for _, v := range a.currentCRCArbitersMap {
		isNormal := true
		isCRMember := false
		claimedDPOSNode := false
		abt, ok := v.(*crcArbiter)
		if ok {
			isCRMember = true
			if !abt.isNormal {
				isNormal = false
			}
			if len(abt.crMember.DPOSPublicKey) != 0 {
				claimedDPOSNode = true
			}
		}
		result = append(result, &ArbiterInfo{
			NodePublicKey:   v.GetNodePublicKey(),
			IsNormal:        isNormal,
			IsCRMember:      isCRMember,
			ClaimedDPOSNode: claimedDPOSNode,
		})
	}

	return result
}

func (a *arbitrators) GetNextCRCArbiters() [][]byte {
	a.mtx.Lock()
	result := make([][]byte, 0, len(a.nextCRCArbiters))
	for _, v := range a.nextCRCArbiters {
		if !v.IsNormal() {
			continue
		}
		result = append(result, v.GetNodePublicKey())
	}
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetCurrentRewardData() RewardData {
	a.mtx.Lock()
	result := a.CurrentReward
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) GetNextRewardData() RewardData {
	a.mtx.Lock()
	result := a.NextReward
	a.mtx.Unlock()

	return result
}

func (a *arbitrators) IsCRCArbitrator(pk []byte) bool {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	for _, v := range a.currentCRCArbitersMap {
		if bytes.Equal(v.GetNodePublicKey(), pk) {
			return true
		}
	}
	return false
}

func (a *arbitrators) isNextCRCArbitrator(pk []byte) bool {
	for _, v := range a.nextCRCArbitersMap {
		if bytes.Equal(v.GetNodePublicKey(), pk) {
			return true
		}
	}
	return false
}

func (a *arbitrators) IsNextCRCArbitrator(pk []byte) bool {
	for _, v := range a.nextCRCArbiters {
		if bytes.Equal(v.GetNodePublicKey(), pk) {
			return true
		}
	}
	return false
}

func (a *arbitrators) IsMemberElectedNextCRCArbitrator(pk []byte) bool {
	for _, v := range a.nextCRCArbiters {
		if bytes.Equal(v.GetNodePublicKey(), pk) && v.(*crcArbiter).crMember.MemberState == state.MemberElected {
			return true
		}
	}
	return false
}

func (a *arbitrators) IsActiveProducer(pk []byte) bool {
	return a.State.IsActiveProducer(pk)
}

func (a *arbitrators) IsDisabledProducer(pk []byte) bool {
	return a.State.IsInactiveProducer(pk) || a.State.IsIllegalProducer(pk) || a.State.IsCanceledProducer(pk)
}

func (a *arbitrators) GetConnectedProducer(publicKey []byte) ArbiterMember {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	for _, v := range a.currentCRCArbitersMap {
		if bytes.Equal(v.GetNodePublicKey(), publicKey) {
			return v
		}
	}

	for _, v := range a.nextCRCArbitersMap {
		if bytes.Equal(v.GetNodePublicKey(), publicKey) {
			return v
		}
	}

	findByPk := func(arbiters []ArbiterMember) ArbiterMember {
		for _, v := range arbiters {
			if bytes.Equal(v.GetNodePublicKey(), publicKey) {
				return v
			}
		}
		return nil
	}
	if ar := findByPk(a.currentArbitrators); ar != nil {
		return ar
	}
	if ar := findByPk(a.currentCandidates); ar != nil {
		return ar
	}
	if ar := findByPk(a.nextArbitrators); ar != nil {
		return ar
	}
	if ar := findByPk(a.nextCandidates); ar != nil {
		return ar
	}

	return nil
}

func (a *arbitrators) CRCProducerCount() int {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	return len(a.currentCRCArbitersMap)
}

func (a *arbitrators) getOnDutyArbitrator() []byte {
	return a.getNextOnDutyArbitratorV(a.bestHeight()+1, 0).GetNodePublicKey()
}

func (a *arbitrators) GetOnDutyArbitrator() []byte {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	arbiter := a.getNextOnDutyArbitratorV(a.bestHeight()+1, 0)
	if arbiter != nil && arbiter.IsNormal() {
		return arbiter.GetNodePublicKey()
	}
	return []byte{}
}

func (a *arbitrators) GetNextOnDutyArbitrator(offset uint32) []byte {
	arbiter := a.getNextOnDutyArbitratorV(a.bestHeight()+1, offset)
	if arbiter == nil {
		return []byte{}
	}
	return arbiter.GetNodePublicKey()
}

func (a *arbitrators) GetOnDutyCrossChainArbitrator() []byte {
	var arbiter []byte
	height := a.bestHeight()
	if height < a.chainParams.CRCOnlyDPOSHeight-1 {
		arbiter = a.GetOnDutyArbitrator()
	} else if height < a.chainParams.CRClaimDPOSNodeStartHeight {
		a.mtx.Lock()
		crcArbiters := a.getCRCArbiters()
		sort.Slice(crcArbiters, func(i, j int) bool {
			return bytes.Compare(crcArbiters[i].NodePublicKey, crcArbiters[j].NodePublicKey) < 0
		})
		ondutyIndex := int(height-a.chainParams.CRCOnlyDPOSHeight+1) % len(crcArbiters)
		arbiter = crcArbiters[ondutyIndex].NodePublicKey
		a.mtx.Unlock()
	} else if height < a.chainParams.DPOSNodeCrossChainHeight {
		a.mtx.Lock()
		crcArbiters := a.getCRCArbiters()
		sort.Slice(crcArbiters, func(i, j int) bool {
			return bytes.Compare(crcArbiters[i].NodePublicKey,
				crcArbiters[j].NodePublicKey) < 0
		})
		index := a.dutyIndex % len(a.currentCRCArbitersMap)
		if crcArbiters[index].IsNormal {
			arbiter = crcArbiters[index].NodePublicKey
		} else {
			arbiter = nil
		}
		a.mtx.Unlock()
	} else {
		a.mtx.Lock()
		if len(a.currentArbitrators) != 0 && a.currentArbitrators[a.dutyIndex].IsNormal() {
			arbiter = a.currentArbitrators[a.dutyIndex].GetNodePublicKey()
		} else {
			arbiter = nil
		}
		a.mtx.Unlock()
	}

	return arbiter
}

func (a *arbitrators) GetCrossChainArbiters() []*ArbiterInfo {
	bestHeight := a.bestHeight()
	if bestHeight < a.chainParams.CRCOnlyDPOSHeight-1 {
		return a.GetArbitrators()
	}
	if bestHeight < a.chainParams.DPOSNodeCrossChainHeight {
		return a.GetCRCArbiters()
	}

	return a.GetArbitrators()
}

func (a *arbitrators) GetCrossChainArbitersCount() int {
	if a.bestHeight() < a.chainParams.CRCOnlyDPOSHeight-1 {
		return len(a.chainParams.OriginArbiters)
	}

	return len(a.chainParams.CRCArbiters)
}

func (a *arbitrators) GetCrossChainArbitersMajorityCount() int {
	minSignCount := int(float64(a.GetCrossChainArbitersCount()) *
		MajoritySignRatioNumerator / MajoritySignRatioDenominator)
	return minSignCount
}

func (a *arbitrators) getNextOnDutyArbitratorV(height, offset uint32) ArbiterMember {
	// main version is >= H1
	if height >= a.chainParams.CRCOnlyDPOSHeight {
		arbitrators := a.currentArbitrators
		if len(arbitrators) == 0 {
			return nil
		}
		index := (a.dutyIndex + int(offset)) % len(arbitrators)
		arbiter := arbitrators[index]

		return arbiter
	}

	// old version
	return a.getNextOnDutyArbitratorV0(height, offset)
}

func (a *arbitrators) GetArbitersCount() int {
	a.mtx.Lock()
	result := len(a.currentArbitrators)
	if result == 0 {
		result = a.chainParams.GeneralArbiters + len(a.chainParams.CRCArbiters)
	}
	a.mtx.Unlock()
	return result
}

func (a *arbitrators) GetCRCArbitersCount() int {
	a.mtx.Lock()
	result := len(a.currentCRCArbitersMap)
	a.mtx.Unlock()
	return result
}

func (a *arbitrators) GetArbitersMajorityCount() int {
	a.mtx.Lock()
	var currentArbitratorsCount int
	if len(a.currentArbitrators) != 0 {
		currentArbitratorsCount = len(a.currentArbitrators)
	} else {
		currentArbitratorsCount = len(a.chainParams.CRCArbiters) + a.chainParams.GeneralArbiters
	}
	minSignCount := int(float64(currentArbitratorsCount) *
		MajoritySignRatioNumerator / MajoritySignRatioDenominator)
	a.mtx.Unlock()
	return minSignCount
}

func (a *arbitrators) HasArbitersMajorityCount(num int) bool {
	return num > a.GetArbitersMajorityCount()
}

func (a *arbitrators) HasArbitersMinorityCount(num int) bool {
	a.mtx.Lock()
	count := len(a.currentArbitrators)
	a.mtx.Unlock()
	return num >= count-a.GetArbitersMajorityCount()
}

func (a *arbitrators) getChangeType(height uint32) (ChangeType, uint32) {

	// special change points:
	//		H1 - PreConnectOffset -> 	[updateNext, H1]: update next arbiters and let CRC arbiters prepare to connect
	//		H1 -> 						[normalChange, H1]: should change to new election (that only have CRC arbiters)
	//		H2 - PreConnectOffset -> 	[updateNext, H2]: update next arbiters and let normal arbiters prepare to connect
	//		H2 -> 						[normalChange, H2]: should change to new election (arbiters will have both CRC and normal arbiters)
	if height == a.chainParams.CRCOnlyDPOSHeight-
		a.chainParams.PreConnectOffset {
		return updateNext, a.chainParams.CRCOnlyDPOSHeight
	} else if height == a.chainParams.CRCOnlyDPOSHeight {
		return normalChange, a.chainParams.CRCOnlyDPOSHeight
	} else if height == a.chainParams.PublicDPOSHeight-
		a.chainParams.PreConnectOffset {
		return updateNext, a.chainParams.PublicDPOSHeight
	} else if height == a.chainParams.PublicDPOSHeight {
		return normalChange, a.chainParams.PublicDPOSHeight
	}

	// main version >= H2
	if height > a.chainParams.PublicDPOSHeight &&
		a.dutyIndex == len(a.currentArbitrators)-1 {
		return normalChange, height
	}

	if height > a.chainParams.RevertToPOWStartHeight &&
		a.dutyIndex == len(a.chainParams.CRCArbiters)+a.chainParams.GeneralArbiters-1 {
		return normalChange, height
	}

	return none, height
}

func (a *arbitrators) cleanArbitrators(height uint32) {
	oriCurrentCRCArbitersMap := copyCRCArbitersMap(a.currentCRCArbitersMap)
	oriCurrentArbitrators := a.currentArbitrators
	oriCurrentCandidates := a.currentCandidates
	oriNextCRCArbitersMap := copyCRCArbitersMap(a.nextCRCArbitersMap)
	oriNextArbitrators := a.nextArbitrators
	oriNextCandidates := a.nextCandidates
	oriDutyIndex := a.dutyIndex
	a.history.Append(height, func() {
		a.currentCRCArbitersMap = make(map[common.Uint168]ArbiterMember)
		a.currentArbitrators = make([]ArbiterMember, 0)
		a.currentCandidates = make([]ArbiterMember, 0)
		a.nextCRCArbitersMap = make(map[common.Uint168]ArbiterMember)
		a.nextArbitrators = make([]ArbiterMember, 0)
		a.nextCandidates = make([]ArbiterMember, 0)
		a.dutyIndex = 0
	}, func() {
		a.currentCRCArbitersMap = oriCurrentCRCArbitersMap
		a.currentArbitrators = oriCurrentArbitrators
		a.currentCandidates = oriCurrentCandidates
		a.nextCRCArbitersMap = oriNextCRCArbitersMap
		a.nextArbitrators = oriNextArbitrators
		a.nextCandidates = oriNextCandidates
		a.dutyIndex = oriDutyIndex
	})
}

func (a *arbitrators) changeCurrentArbitrators(height uint32) error {
	oriCurrentCRCArbitersMap := copyCRCArbitersMap(a.currentCRCArbitersMap)
	oriCurrentArbitrators := a.currentArbitrators
	oriCurrentCandidates := a.currentCandidates
	oriCurrentReward := a.CurrentReward
	oriDutyIndex := a.dutyIndex
	a.history.Append(height, func() {
		sort.Slice(a.nextArbitrators, func(i, j int) bool {
			return bytes.Compare(a.nextArbitrators[i].GetNodePublicKey(),
				a.nextArbitrators[j].GetNodePublicKey()) < 0
		})
		a.currentCRCArbitersMap = copyCRCArbitersMap(a.nextCRCArbitersMap)
		a.currentArbitrators = a.nextArbitrators
		a.currentCandidates = a.nextCandidates
		a.CurrentReward = a.NextReward
		a.dutyIndex = 0
	}, func() {
		a.currentCRCArbitersMap = oriCurrentCRCArbitersMap
		a.currentArbitrators = oriCurrentArbitrators
		a.currentCandidates = oriCurrentCandidates
		a.CurrentReward = oriCurrentReward
		a.dutyIndex = oriDutyIndex
	})
	return nil
}

func (a *arbitrators) IsSameWithNextArbitrators() bool {

	if len(a.nextArbitrators) != len(a.currentArbitrators) {
		return false
	}
	for index, v := range a.currentArbitrators {
		if bytes.Equal(v.GetNodePublicKey(), a.nextArbitrators[index].GetNodePublicKey()) {
			return false
		}
	}
	return true
}

func (a *arbitrators) ConvertToArbitersStr(arbiters [][]byte) []string {
	var arbitersStr []string
	for _, v := range arbiters {
		arbitersStr = append(arbitersStr, common.BytesToHexString(v))
	}
	return arbitersStr
}

func (a *arbitrators) createNextTurnDPOSInfoTransaction(blockHeight uint32, forceChange bool) *types.Transaction {
	var nextTurnDPOSInfo payload.NextTurnDPOSInfo
	nextTurnDPOSInfo.CRPublicKeys = make([][]byte, 0)
	nextTurnDPOSInfo.DPOSPublicKeys = make([][]byte, 0)
	var workingHeight uint32
	if forceChange {
		workingHeight = blockHeight
	} else {
		workingHeight = blockHeight + uint32(a.chainParams.GeneralArbiters+len(a.chainParams.CRCArbiters))
	}
	nextTurnDPOSInfo.WorkingHeight = workingHeight
	for _, v := range a.nextArbitrators {
		if a.isNextCRCArbitrator(v.GetNodePublicKey()) {
			if abt, ok := v.(*crcArbiter); ok && abt.crMember.MemberState != state.MemberElected {
				nextTurnDPOSInfo.CRPublicKeys = append(nextTurnDPOSInfo.CRPublicKeys, []byte{})
			} else {
				nextTurnDPOSInfo.CRPublicKeys = append(nextTurnDPOSInfo.CRPublicKeys, v.GetNodePublicKey())
			}
		} else {
			nextTurnDPOSInfo.DPOSPublicKeys = append(nextTurnDPOSInfo.DPOSPublicKeys, v.GetNodePublicKey())
		}
	}

	log.Debugf("[createNextTurnDPOSInfoTransaction] CRPublicKeys %v, DPOSPublicKeys%v\n",
		a.ConvertToArbitersStr(nextTurnDPOSInfo.CRPublicKeys), a.ConvertToArbitersStr(nextTurnDPOSInfo.DPOSPublicKeys))

	return &types.Transaction{
		Version:    types.TxVersion09,
		TxType:     types.NextTurnDPOSInfo,
		Payload:    &nextTurnDPOSInfo,
		Attributes: []*types.Attribute{},
		Programs:   []*program.Program{},
		LockTime:   0,
	}
}

func (a *arbitrators) updateNextTurnInfo(height uint32, producers []ArbiterMember, unclaimed int) {
	nextCRCArbiters := a.nextArbitrators
	a.nextArbitrators = append(a.nextArbitrators, producers...)
	sort.Slice(a.nextArbitrators, func(i, j int) bool {
		return bytes.Compare(a.nextArbitrators[i].GetNodePublicKey(), a.nextArbitrators[j].GetNodePublicKey()) < 0
	})
	if height >= a.chainParams.CRClaimDPOSNodeStartHeight {
		//need sent a NextTurnDPOSInfo tx into mempool
		sort.Slice(nextCRCArbiters, func(i, j int) bool {
			return bytes.Compare(nextCRCArbiters[i].GetNodePublicKey(), nextCRCArbiters[j].GetNodePublicKey()) < 0
		})
		a.nextCRCArbiters = copyByteList(nextCRCArbiters)
	}
}

func (a *arbitrators) getProducers(count int, height uint32) ([]ArbiterMember, error) {
	return a.GetNormalArbitratorsDesc(height, count,
		a.getSortedProducers(), 0)
}

func (a *arbitrators) getSortedProducers() []*Producer {
	votedProducers := a.State.GetVotedProducers()
	sort.Slice(votedProducers, func(i, j int) bool {
		if votedProducers[i].votes == votedProducers[j].votes {
			return bytes.Compare(votedProducers[i].info.NodePublicKey,
				votedProducers[j].NodePublicKey()) < 0
		}
		return votedProducers[i].Votes() > votedProducers[j].Votes()
	})

	return votedProducers
}

func (a *arbitrators) getSortedProducersWithRandom(height uint32, unclaimedCount int) ([]*Producer, error) {
	votedProducers := a.getSortedProducers()
	if height < a.chainParams.NoCRCDPOSNodeHeight {
		return votedProducers, nil
	}

	// if the last random producer is not found or the poll ranked in the top
	// 23(may be 35) or the state is not active, need to get a candidate as
	// DPOS node at random.
	if a.LastRandomCandidateHeight != 0 &&
		height-a.LastRandomCandidateHeight < a.chainParams.RandomCandidatePeriod {
		for i, p := range votedProducers {
			if common.BytesToHexString(p.info.OwnerPublicKey) == a.LastRandomCandidateOwner {
				if i < unclaimedCount+a.chainParams.GeneralArbiters-1 || p.state != Active {
					// need get again at random.
					break
				}
				normalCount := a.chainParams.GeneralArbiters - 1
				selectedCandidateIndex := i

				newProducers := make([]*Producer, 0, len(votedProducers))
				newProducers = append(newProducers, votedProducers[:unclaimedCount+normalCount]...)
				newProducers = append(newProducers, p)
				newProducers = append(newProducers, votedProducers[unclaimedCount+normalCount:selectedCandidateIndex]...)
				newProducers = append(newProducers, votedProducers[selectedCandidateIndex+1:]...)
				log.Info("no need random producer, current random producer:",
					p.info.NickName, "current height:", height)
				return newProducers, nil
			}
		}
	}

	candidateIndex, err := a.getCandidateIndexAtRandom(height, unclaimedCount, len(votedProducers))
	if err != nil {
		return nil, err
	}

	normalCount := a.chainParams.GeneralArbiters - 1
	selectedCandidateIndex := unclaimedCount + normalCount + candidateIndex
	candidateProducer := votedProducers[selectedCandidateIndex]

	// todo need to use history?
	a.LastRandomCandidateHeight = height
	a.LastRandomCandidateOwner = common.BytesToHexString(candidateProducer.info.OwnerPublicKey)

	newProducers := make([]*Producer, 0, len(votedProducers))
	newProducers = append(newProducers, votedProducers[:unclaimedCount+normalCount]...)
	newProducers = append(newProducers, candidateProducer)
	newProducers = append(newProducers, votedProducers[unclaimedCount+normalCount:selectedCandidateIndex]...)
	newProducers = append(newProducers, votedProducers[selectedCandidateIndex+1:]...)

	log.Info("random producer, current random producer:",
		candidateProducer.info.NickName, "current height:", height)
	return newProducers, nil
}

func (a *arbitrators) getCandidateIndexAtRandom(height uint32, unclaimedCount, votedProducersCount int) (int, error) {
	block, _ := a.getBlockByHeight(height - 1)
	if block == nil {
		return 0, errors.New("block is not found")
	}
	var x = make([]byte, 8)
	blockHash := block.Hash()
	copy(x, blockHash[24:])
	seed, _, ok := readi64(x)
	if !ok {
		return 0, errors.New("invalid block hash")
	}
	rand.Seed(seed)
	normalCount := a.chainParams.GeneralArbiters - 1
	count := votedProducersCount - unclaimedCount - normalCount
	if count < 1 {
		return 0, errors.New("producers is not enough")
	}
	candidatesCount := minInt(count, a.chainParams.CandidateArbiters+1)
	return rand.Intn(candidatesCount), nil
}

func (a *arbitrators) updateNextArbitrators(versionHeight, height uint32) error {

	if height >= a.chainParams.CRClaimDPOSNodeStartHeight {
		oriNeedNextTurnDPOSInfo := a.NeedNextTurnDPOSInfo
		a.history.Append(height, func() {
			a.NeedNextTurnDPOSInfo = true
		}, func() {
			a.NeedNextTurnDPOSInfo = oriNeedNextTurnDPOSInfo
		})
	}

	_, recover := a.InactiveModeSwitch(versionHeight, a.IsAbleToRecoverFromInactiveMode)
	if recover {
		a.LeaveEmergency(a.history, height)
	} else {
		a.TryLeaveUnderStaffed(a.IsAbleToRecoverFromUnderstaffedState)
	}
	unclaimed, err := a.resetNextArbiterByCRC(versionHeight, height)
	if err != nil {
		return err
	}

	if !a.IsInactiveMode() && !a.IsUnderstaffedMode() {

		count := a.chainParams.GeneralArbiters
		votedProducers, err := a.getSortedProducersWithRandom(height, unclaimed)
		if err != nil {
			return err
		}
		producers, err := a.GetNormalArbitratorsDesc(versionHeight, count,
			votedProducers, unclaimed)
		if err != nil {
			if height > a.chainParams.ChangeCommitteeNewCRHeight {
				return err
			}
			if err := a.tryHandleError(versionHeight, err); err != nil {
				return err
			}
			oriNextCandidates := a.nextCandidates
			oriNextArbitrators := a.nextArbitrators
			oriNextCRCArbiters := a.nextCRCArbiters
			a.history.Append(height, func() {
				a.nextCandidates = make([]ArbiterMember, 0)
				a.updateNextTurnInfo(height, producers, unclaimed)
			}, func() {
				a.nextCandidates = oriNextCandidates
				a.nextArbitrators = oriNextArbitrators
				a.nextCRCArbiters = oriNextCRCArbiters
			})
		} else {
			if height >= a.chainParams.NoCRCDPOSNodeHeight {
				count := len(a.chainParams.CRCArbiters) + a.chainParams.GeneralArbiters
				var newSelected bool
				for _, p := range votedProducers {
					producer := p
					ownerPK := common.BytesToHexString(producer.info.OwnerPublicKey)
					if ownerPK == a.LastRandomCandidateOwner &&
						height-a.LastRandomCandidateHeight == uint32(count) {
						newSelected = true
					}
				}
				if newSelected {
					for _, p := range votedProducers {
						producer := p
						if producer.selected {
							a.history.Append(height, func() {
								producer.selected = false
							}, func() {
								producer.selected = true
							})
						}
						ownerPK := common.BytesToHexString(producer.info.OwnerPublicKey)
						oriRandomInactiveCount := producer.randomCandidateInactiveCount
						if ownerPK == a.LastRandomCandidateOwner {
							a.history.Append(height, func() {
								producer.selected = true
								producer.randomCandidateInactiveCount = 0
							}, func() {
								producer.selected = false
								producer.randomCandidateInactiveCount = oriRandomInactiveCount
							})
						}
					}
				}
			}
			oriNextArbitrators := a.nextArbitrators
			oriNextCRCArbiters := a.nextCRCArbiters
			a.history.Append(height, func() {
				a.updateNextTurnInfo(height, producers, unclaimed)
			}, func() {
				// next arbitrators will rollback in resetNextArbiterByCRC
				a.nextArbitrators = oriNextArbitrators
				a.nextCRCArbiters = oriNextCRCArbiters
			})

			candidates, err := a.GetCandidatesDesc(versionHeight, count+unclaimed,
				votedProducers)
			if err != nil {
				return err
			}
			oriNextCandidates := a.nextCandidates
			a.history.Append(height, func() {
				a.nextCandidates = candidates
			}, func() {
				a.nextCandidates = oriNextCandidates
			})
		}
	} else {
		oriNextCandidates := a.nextCandidates
		oriNextArbitrators := a.nextArbitrators
		oriNextCRCArbiters := a.nextCRCArbiters
		a.history.Append(height, func() {
			a.nextCandidates = make([]ArbiterMember, 0)
			a.updateNextTurnInfo(height, nil, unclaimed)
		}, func() {
			a.nextCandidates = oriNextCandidates
			a.nextArbitrators = oriNextArbitrators
			a.nextCRCArbiters = oriNextCRCArbiters
		})
	}
	return nil
}

func (a *arbitrators) resetNextArbiterByCRC(versionHeight uint32, height uint32) (int, error) {
	var unclaimed int
	var needReset bool
	crcArbiters := map[common.Uint168]ArbiterMember{}
	if a.crCommittee != nil && a.crCommittee.IsInElectionPeriod() {
		if versionHeight >= a.chainParams.CRClaimDPOSNodeStartHeight {
			var err error
			if versionHeight < a.chainParams.ChangeCommitteeNewCRHeight {
				if crcArbiters, err = a.getCRCArbitersV1(height); err != nil {
					return unclaimed, err
				}
			} else {
				if crcArbiters, unclaimed, err = a.getCRCArbitersV2(height); err != nil {
					return unclaimed, err
				}
			}
		} else {
			var err error
			if crcArbiters, err = a.getCRCArbitersV0(); err != nil {
				return unclaimed, err
			}
		}
		needReset = true
	} else if versionHeight >= a.chainParams.ChangeCommitteeNewCRHeight {
		votedProducers := a.State.GetVotedProducers()

		if len(votedProducers) < len(a.chainParams.CRCArbiters) {
			return unclaimed, errors.New("votedProducers less than CRCArbiters")
		}

		sort.Slice(votedProducers, func(i, j int) bool {
			if votedProducers[i].votes == votedProducers[j].votes {
				return bytes.Compare(votedProducers[i].info.NodePublicKey,
					votedProducers[j].NodePublicKey()) < 0
			}
			return votedProducers[i].Votes() > votedProducers[j].Votes()
		})

		for i := 0; i < len(a.chainParams.CRCArbiters); i++ {
			producer := votedProducers[i]
			ar, err := NewDPoSArbiter(producer)
			if err != nil {
				return unclaimed, err
			}
			crcArbiters[ar.GetOwnerProgramHash()] = ar
		}
		unclaimed = len(a.chainParams.CRCArbiters)
		needReset = true

	} else if versionHeight >= a.chainParams.CRCommitteeStartHeight {
		for _, pk := range a.chainParams.CRCArbiters {
			pubKey, err := hex.DecodeString(pk)
			if err != nil {
				return unclaimed, err
			}
			producer := &Producer{ // here need crc NODE public key
				info: payload.ProducerInfo{
					OwnerPublicKey: pubKey,
					NodePublicKey:  pubKey,
				},
				activateRequestHeight: math.MaxUint32,
			}
			ar, err := NewDPoSArbiter(producer)
			if err != nil {
				return unclaimed, err
			}
			crcArbiters[ar.GetOwnerProgramHash()] = ar
		}
		needReset = true
	}

	if needReset {
		oriNextArbitersMap := a.nextCRCArbitersMap
		oriCRCChangedHeight := a.crcChangedHeight
		a.history.Append(height, func() {
			a.nextCRCArbitersMap = crcArbiters
			a.crcChangedHeight = a.crCommittee.LastCommitteeHeight
		}, func() {
			a.nextCRCArbitersMap = oriNextArbitersMap
			a.crcChangedHeight = oriCRCChangedHeight
		})
	}

	oriNextArbiters := a.nextArbitrators
	a.history.Append(height, func() {
		a.nextArbitrators = make([]ArbiterMember, 0)
		for _, v := range a.nextCRCArbitersMap {
			a.nextArbitrators = append(a.nextArbitrators, v)
		}
	}, func() {
		a.nextArbitrators = oriNextArbiters
	})

	return unclaimed, nil
}

func (a *arbitrators) getCRCArbitersV2(height uint32) (map[common.Uint168]ArbiterMember, int, error) {
	crMembers := a.crCommittee.GetAllMembersCopy()
	if len(crMembers) != len(a.chainParams.CRCArbiters) {
		return nil, 0, errors.New("CRC members count mismatch with CRC arbiters")
	}

	// get public key map
	crPublicKeysMap := make(map[string]struct{})
	for _, cr := range crMembers {
		if len(cr.DPOSPublicKey) != 0 {
			crPublicKeysMap[common.BytesToHexString(cr.DPOSPublicKey)] = struct{}{}
		}
	}
	arbitersPublicKeysMap := make(map[string]struct{})
	for _, ar := range a.chainParams.CRCArbiters {
		arbitersPublicKeysMap[ar] = struct{}{}
	}

	// get unclaimed arbiter keys list
	unclaimedArbiterKeys := make([]string, 0)
	for k, _ := range arbitersPublicKeysMap {
		if _, ok := crPublicKeysMap[k]; !ok {
			unclaimedArbiterKeys = append(unclaimedArbiterKeys, k)
		}
	}
	sort.Slice(unclaimedArbiterKeys, func(i, j int) bool {
		return strings.Compare(unclaimedArbiterKeys[i], unclaimedArbiterKeys[j]) < 0
	})
	producers, err := a.getProducers(int(a.chainParams.CRMemberCount), height)
	if err != nil {
		return nil, 0, err
	}
	var unclaimedCount int
	crcArbiters := map[common.Uint168]ArbiterMember{}
	claimHeight := a.chainParams.CRClaimDPOSNodeStartHeight
	for _, cr := range crMembers {
		var pk []byte
		if len(cr.DPOSPublicKey) == 0 {
			if cr.MemberState != state.MemberElected {
				var err error
				pk, err = common.HexStringToBytes(unclaimedArbiterKeys[0])
				if err != nil {
					return nil, 0, err
				}
				unclaimedArbiterKeys = unclaimedArbiterKeys[1:]
			} else {
				pk = producers[unclaimedCount].GetNodePublicKey()
				unclaimedCount++
			}
		} else {
			pk = cr.DPOSPublicKey
		}
		crPublicKey := cr.Info.Code[1 : len(cr.Info.Code)-1]
		isNormal := true
		if height >= claimHeight && cr.MemberState != state.MemberElected {
			isNormal = false
		}
		ar, err := NewCRCArbiter(pk, crPublicKey, cr, isNormal)
		if err != nil {
			return nil, 0, err
		}
		crcArbiters[ar.GetOwnerProgramHash()] = ar
	}

	return crcArbiters, unclaimedCount, nil
}

func (a *arbitrators) getCRCArbitersV1(height uint32) (map[common.Uint168]ArbiterMember, error) {
	crMembers := a.crCommittee.GetAllMembersCopy()
	if len(crMembers) != len(a.chainParams.CRCArbiters) {
		return nil, errors.New("CRC members count mismatch with CRC arbiters")
	}

	// get public key map
	crPublicKeysMap := make(map[string]struct{})
	for _, cr := range crMembers {
		if len(cr.DPOSPublicKey) != 0 {
			crPublicKeysMap[common.BytesToHexString(cr.DPOSPublicKey)] = struct{}{}
		}
	}
	arbitersPublicKeysMap := make(map[string]struct{})
	for _, ar := range a.chainParams.CRCArbiters {
		arbitersPublicKeysMap[ar] = struct{}{}
	}

	// get unclaimed arbiter keys list
	unclaimedArbiterKeys := make([]string, 0)
	for k, _ := range arbitersPublicKeysMap {
		if _, ok := crPublicKeysMap[k]; !ok {
			unclaimedArbiterKeys = append(unclaimedArbiterKeys, k)
		}
	}
	sort.Slice(unclaimedArbiterKeys, func(i, j int) bool {
		return strings.Compare(unclaimedArbiterKeys[i], unclaimedArbiterKeys[j]) < 0
	})
	crcArbiters := map[common.Uint168]ArbiterMember{}
	claimHeight := a.chainParams.CRClaimDPOSNodeStartHeight
	for _, cr := range crMembers {
		var pk []byte
		if len(cr.DPOSPublicKey) == 0 {
			var err error
			pk, err = common.HexStringToBytes(unclaimedArbiterKeys[0])
			if err != nil {
				return nil, err
			}
			unclaimedArbiterKeys = unclaimedArbiterKeys[1:]
		} else {
			pk = cr.DPOSPublicKey
		}
		crPublicKey := cr.Info.Code[1 : len(cr.Info.Code)-1]
		isNormal := true
		if height >= claimHeight && cr.MemberState != state.MemberElected {
			isNormal = false
		}
		ar, err := NewCRCArbiter(pk, crPublicKey, cr, isNormal)
		if err != nil {
			return nil, err
		}
		crcArbiters[ar.GetOwnerProgramHash()] = ar
	}

	return crcArbiters, nil
}

func (a *arbitrators) getCRCArbitersV0() (map[common.Uint168]ArbiterMember, error) {
	crMembers := a.crCommittee.GetAllMembersCopy()
	if len(crMembers) != len(a.chainParams.CRCArbiters) {
		return nil, errors.New("CRC members count mismatch with CRC arbiters")
	}

	crcArbiters := map[common.Uint168]ArbiterMember{}
	for i, v := range a.chainParams.CRCArbiters {
		pk, err := common.HexStringToBytes(v)
		if err != nil {
			return nil, err
		}
		ar, err := NewCRCArbiter(pk, pk, crMembers[i], true)
		if err != nil {
			return nil, err
		}
		crcArbiters[ar.GetOwnerProgramHash()] = ar
	}

	return crcArbiters, nil
}

func (a *arbitrators) GetCandidatesDesc(height uint32, startIndex int,
	producers []*Producer) ([]ArbiterMember, error) {
	// main version >= H2
	if height >= a.chainParams.PublicDPOSHeight {
		if len(producers) < startIndex {
			return make([]ArbiterMember, 0), nil
		}

		result := make([]ArbiterMember, 0)
		for i := startIndex; i < len(producers) && i < startIndex+a.
			chainParams.CandidateArbiters; i++ {
			ar, err := NewDPoSArbiter(producers[i])
			if err != nil {
				return nil, err
			}
			result = append(result, ar)
		}
		return result, nil
	}

	// old version [0, H2)
	return nil, nil
}

func (a *arbitrators) GetNormalArbitratorsDesc(height uint32,
	arbitratorsCount int, producers []*Producer, start int) ([]ArbiterMember, error) {

	// main version >= H2
	if height >= a.chainParams.PublicDPOSHeight {
		return a.getNormalArbitratorsDescV2(arbitratorsCount, producers, start)
	}

	// version [H1, H2)
	if height >= a.chainParams.CRCOnlyDPOSHeight {
		return a.getNormalArbitratorsDescV1()
	}

	// version [0, H1)
	return a.getNormalArbitratorsDescV0()
}

func (a *arbitrators) snapshotVotesStates(height uint32) error {
	var nextReward RewardData
	recordVotes := func(nodePublicKey []byte) error {
		producer := a.GetProducer(nodePublicKey)
		if producer == nil {
			return errors.New("get producer by node public key failed")
		}
		programHash, err := contract.PublicKeyToStandardProgramHash(
			producer.OwnerPublicKey())
		if err != nil {
			return err
		}
		nextReward.OwnerVotesInRound[*programHash] = producer.Votes()
		nextReward.TotalVotesInRound += producer.Votes()
		return nil
	}

	nextReward.OwnerVotesInRound = make(map[common.Uint168]common.Fixed64, 0)
	nextReward.TotalVotesInRound = 0
	for _, ar := range a.nextArbitrators {
		if height > a.chainParams.ChangeCommitteeNewCRHeight {
			if ar.GetType() == CRC && (!ar.IsNormal() ||
				(len(ar.(*crcArbiter).crMember.DPOSPublicKey) != 0 && ar.IsNormal())) {
				continue
			}
			if err := recordVotes(ar.GetNodePublicKey()); err != nil {
				return err
			}
		} else {
			if !a.isNextCRCArbitrator(ar.GetNodePublicKey()) {
				if err := recordVotes(ar.GetNodePublicKey()); err != nil {
					return err
				}
			}
		}
	}

	for _, ar := range a.nextCandidates {
		if a.isNextCRCArbitrator(ar.GetNodePublicKey()) {
			continue
		}
		producer := a.GetProducer(ar.GetNodePublicKey())
		if producer == nil {
			return errors.New("get producer by node public key failed")
		}
		programHash, err := contract.PublicKeyToStandardProgramHash(producer.OwnerPublicKey())
		if err != nil {
			return err
		}
		nextReward.OwnerVotesInRound[*programHash] = producer.Votes()
		nextReward.TotalVotesInRound += producer.Votes()
	}

	oriNextReward := a.NextReward
	a.history.Append(height, func() {
		a.NextReward = nextReward
	}, func() {
		a.NextReward = oriNextReward
	})
	return nil
}

func (a *arbitrators) DumpInfo(height uint32) {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	a.dumpInfo(height)
}

func (a *arbitrators) dumpInfo(height uint32) {
	var printer func(string, ...interface{})
	changeType, _ := a.getChangeType(height + 1)
	switch changeType {
	case updateNext:
		fallthrough
	case normalChange:
		printer = log.Infof
	case none:
		printer = log.Debugf
	}

	var crInfo string
	crParams := make([]interface{}, 0)
	if len(a.currentArbitrators) != 0 {
		crInfo, crParams = getArbitersInfoWithOnduty("CURRENT ARBITERS",
			a.currentArbitrators, a.dutyIndex, a.getOnDutyArbitrator())
	} else {
		crInfo, crParams = getArbitersInfoWithoutOnduty("CURRENT ARBITERS", a.currentArbitrators)
	}
	nrInfo, nrParams := getArbitersInfoWithoutOnduty("NEXT ARBITERS", a.nextArbitrators)
	ccInfo, ccParams := getArbitersInfoWithoutOnduty("CURRENT CANDIDATES", a.currentCandidates)
	ncInfo, ncParams := getArbitersInfoWithoutOnduty("NEXT CANDIDATES", a.nextCandidates)
	printer(crInfo+nrInfo+ccInfo+ncInfo, append(append(append(crParams, nrParams...), ccParams...), ncParams...)...)
}

func (a *arbitrators) getBlockDPOSReward(block *types.Block) common.Fixed64 {
	totalTxFx := common.Fixed64(0)
	for _, tx := range block.Transactions {
		totalTxFx += tx.Fee
	}

	return common.Fixed64(math.Ceil(float64(totalTxFx+
		a.chainParams.GetBlockReward(block.Height)) * 0.35))
}

func (a *arbitrators) newCheckPoint(height uint32) *CheckPoint {
	point := &CheckPoint{
		Height:                     height,
		DutyIndex:                  a.dutyIndex,
		CurrentCandidates:          make([]ArbiterMember, 0),
		NextArbitrators:            make([]ArbiterMember, 0),
		NextCandidates:             make([]ArbiterMember, 0),
		CurrentReward:              *NewRewardData(),
		NextReward:                 *NewRewardData(),
		CurrentCRCArbitersMap:      make(map[common.Uint168]ArbiterMember),
		NextCRCArbitersMap:         make(map[common.Uint168]ArbiterMember),
		NextCRCArbiters:            make([]ArbiterMember, 0),
		crcChangedHeight:           a.crcChangedHeight,
		accumulativeReward:         a.accumulativeReward,
		finalRoundChange:           a.finalRoundChange,
		clearingHeight:             a.clearingHeight,
		forceChanged:               a.forceChanged,
		arbitersRoundReward:        make(map[common.Uint168]common.Fixed64),
		illegalBlocksPayloadHashes: make(map[common.Uint256]interface{}),
		CurrentArbitrators:         a.currentArbitrators,
		StateKeyFrame:              *a.State.snapshot(),
	}
	point.CurrentArbitrators = copyByteList(a.currentArbitrators)
	point.CurrentCandidates = copyByteList(a.currentCandidates)
	point.NextArbitrators = copyByteList(a.nextArbitrators)
	point.NextCandidates = copyByteList(a.nextCandidates)
	point.CurrentReward = *copyReward(&a.CurrentReward)
	point.NextReward = *copyReward(&a.NextReward)
	point.NextCRCArbitersMap = copyCRCArbitersMap(a.nextCRCArbitersMap)
	point.CurrentCRCArbitersMap = copyCRCArbitersMap(a.currentCRCArbitersMap)
	point.NextCRCArbiters = copyByteList(a.nextCRCArbiters)

	for k, v := range a.arbitersRoundReward {
		point.arbitersRoundReward[k] = v
	}
	for k := range a.illegalBlocksPayloadHashes {
		point.illegalBlocksPayloadHashes[k] = nil
	}

	return point
}
func (a *arbitrators) Snapshot() *CheckPoint {
	return a.newCheckPoint(0)
}

func (a *arbitrators) snapshot(height uint32) {
	var frames []*CheckPoint
	if v, ok := a.snapshots[height]; ok {
		frames = v
	} else {
		// remove the oldest keys if snapshot capacity is over
		if len(a.snapshotKeysDesc) >= MaxSnapshotLength {
			for i := MaxSnapshotLength - 1; i < len(a.snapshotKeysDesc); i++ {
				delete(a.snapshots, a.snapshotKeysDesc[i])
			}
			a.snapshotKeysDesc = a.snapshotKeysDesc[0 : MaxSnapshotLength-1]
		}

		a.snapshotKeysDesc = append(a.snapshotKeysDesc, height)
		sort.Slice(a.snapshotKeysDesc, func(i, j int) bool {
			return a.snapshotKeysDesc[i] > a.snapshotKeysDesc[j]
		})
	}
	checkpoint := a.newCheckPoint(height)
	frames = append(frames, checkpoint)
	a.snapshots[height] = frames
}

func (a *arbitrators) GetSnapshot(height uint32) []*CheckPoint {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	if height > a.bestHeight() {
		return []*CheckPoint{
			{
				CurrentArbitrators: a.currentArbitrators,
			},
		}
	} else {
		return a.getSnapshot(height)
	}
}

func (a *arbitrators) getSnapshot(height uint32) []*CheckPoint {
	result := make([]*CheckPoint, 0)
	if height >= a.snapshotKeysDesc[len(a.snapshotKeysDesc)-1] {
		// if height is in range of snapshotKeysDesc, get the key with the same
		// election as height
		key := a.snapshotKeysDesc[0]
		for i := 1; i < len(a.snapshotKeysDesc); i++ {
			if height >= a.snapshotKeysDesc[i] &&
				height < a.snapshotKeysDesc[i-1] {
				key = a.snapshotKeysDesc[i]
			}
		}

		return a.snapshots[key]
	}
	return result
}

func getArbitersInfoWithOnduty(title string, arbiters []ArbiterMember,
	dutyIndex int, ondutyArbiter []byte) (string, []interface{}) {
	info := "\n" + title + "\nDUTYINDEX: %d\n%5s %66s %6s \n----- " +
		strings.Repeat("-", 66) + " ------\n"
	params := make([]interface{}, 0)
	params = append(params, (dutyIndex+1)%len(arbiters))
	params = append(params, "INDEX", "PUBLICKEY", "ONDUTY")
	for i, arbiter := range arbiters {
		info += "%-5d %-66s %6t\n"
		var publicKey string
		if arbiter.IsNormal() {
			publicKey = common.BytesToHexString(arbiter.GetNodePublicKey())
		}
		params = append(params, i+1, publicKey, bytes.Equal(
			arbiter.GetNodePublicKey(), ondutyArbiter))
	}
	info += "----- " + strings.Repeat("-", 66) + " ------"
	return info, params
}

func getArbitersInfoWithoutOnduty(title string,
	arbiters []ArbiterMember) (string, []interface{}) {

	info := "\n" + title + "\n%5s %66s\n----- " + strings.Repeat("-", 66) + "\n"
	params := make([]interface{}, 0)
	params = append(params, "INDEX", "PUBLICKEY")
	for i, arbiter := range arbiters {
		info += "%-5d %-66s\n"
		var publicKey string
		if arbiter.IsNormal() {
			publicKey = common.BytesToHexString(arbiter.GetNodePublicKey())
		}
		params = append(params, i+1, publicKey)
	}
	info += "----- " + strings.Repeat("-", 66)
	return info, params
}

func (a *arbitrators) initArbitrators(chainParams *config.Params) error {
	originArbiters := make([]ArbiterMember, len(chainParams.OriginArbiters))
	for i, arbiter := range chainParams.OriginArbiters {
		b, err := common.HexStringToBytes(arbiter)
		if err != nil {
			return err
		}
		ar, err := NewOriginArbiter(b)
		if err != nil {
			return err
		}

		originArbiters[i] = ar
	}

	crcArbiters := make(map[common.Uint168]ArbiterMember)
	for _, pk := range chainParams.CRCArbiters {
		pubKey, err := hex.DecodeString(pk)
		if err != nil {
			return err
		}
		producer := &Producer{ // here need crc NODE public key
			info: payload.ProducerInfo{
				OwnerPublicKey: pubKey,
				NodePublicKey:  pubKey,
			},
			activateRequestHeight: math.MaxUint32,
		}
		ar, err := NewDPoSArbiter(producer)
		if err != nil {
			return err
		}
		crcArbiters[ar.GetOwnerProgramHash()] = ar
	}

	a.currentArbitrators = originArbiters
	a.nextArbitrators = originArbiters
	a.nextCRCArbitersMap = crcArbiters
	a.currentCRCArbitersMap = crcArbiters
	a.CurrentReward = RewardData{
		OwnerVotesInRound: make(map[common.Uint168]common.Fixed64),
		TotalVotesInRound: 0,
	}
	a.NextReward = RewardData{
		OwnerVotesInRound: make(map[common.Uint168]common.Fixed64),
		TotalVotesInRound: 0,
	}
	return nil
}

func NewArbitrators(chainParams *config.Params, committee *state.Committee,
	getProducerDepositAmount func(common.Uint168) (common.Fixed64, error),
	tryUpdateCRMemberInactivity func(did common.Uint168, needReset bool, height uint32),
	tryRevertCRMemberInactivityfunc func(did common.Uint168, oriState state.MemberState, oriInactiveCount uint32, height uint32),
	tryUpdateCRMemberIllegal func(did common.Uint168, height uint32),
	tryRevertCRMemberIllegal func(did common.Uint168, oriState state.MemberState, height uint32)) (
	*arbitrators, error) {
	a := &arbitrators{
		chainParams:                chainParams,
		crCommittee:                committee,
		nextCandidates:             make([]ArbiterMember, 0),
		accumulativeReward:         common.Fixed64(0),
		finalRoundChange:           common.Fixed64(0),
		arbitersRoundReward:        nil,
		illegalBlocksPayloadHashes: make(map[common.Uint256]interface{}),
		snapshots:                  make(map[uint32][]*CheckPoint),
		snapshotKeysDesc:           make([]uint32, 0),
		crcChangedHeight:           0,
		degradation: &degradation{
			inactiveTxs:       make(map[common.Uint256]interface{}),
			inactivateHeight:  0,
			understaffedSince: 0,
			state:             DSNormal,
		},
		history: utils.NewHistory(maxHistoryCapacity),
	}
	if err := a.initArbitrators(chainParams); err != nil {
		return nil, err
	}
	a.State = NewState(chainParams, a.GetArbitrators, a.crCommittee.GetAllMembers,
		a.crCommittee.IsInElectionPeriod,
		getProducerDepositAmount, tryUpdateCRMemberInactivity, tryRevertCRMemberInactivityfunc,
		tryUpdateCRMemberIllegal, tryRevertCRMemberIllegal)

	chainParams.CkpManager.Register(NewCheckpoint(a))
	return a, nil
}
