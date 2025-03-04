package miner

import (
	"bytes"
	"encoding/binary"
	"fmt"
	miner7 "github.com/filecoin-project/specs-actors/v7/actors/builtin/miner"
	"math"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/exitcode"
	rtt "github.com/filecoin-project/go-state-types/rt"
	miner0 "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	miner2 "github.com/filecoin-project/specs-actors/v2/actors/builtin/miner"
	miner3 "github.com/filecoin-project/specs-actors/v3/actors/builtin/miner"
	miner5 "github.com/filecoin-project/specs-actors/v5/actors/builtin/miner"
	cid "github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/v8/actors/builtin"
	"github.com/filecoin-project/specs-actors/v8/actors/builtin/market"
	"github.com/filecoin-project/specs-actors/v8/actors/builtin/power"
	"github.com/filecoin-project/specs-actors/v8/actors/builtin/reward"
	"github.com/filecoin-project/specs-actors/v8/actors/runtime"
	"github.com/filecoin-project/specs-actors/v8/actors/runtime/proof"
	. "github.com/filecoin-project/specs-actors/v8/actors/util"
	"github.com/filecoin-project/specs-actors/v8/actors/util/adt"
	"github.com/filecoin-project/specs-actors/v8/actors/util/smoothing"
)

type Runtime = runtime.Runtime

const (
	// The first 1000 actor-specific codes are left open for user error, i.e. things that might
	// actually happen without programming error in the actor code.
	//ErrToBeDetermined = exitcode.FirstActorSpecificExitCode + iota

	// The following errors are particular cases of illegal state.
	// They're not expected to ever happen, but if they do, distinguished codes can help us
	// diagnose the problem.
	ErrBalanceInvariantBroken = 1000
)

type Actor struct{}

func (a Actor) Exports() []interface{} {
	return []interface{}{
		builtin.MethodConstructor: a.Constructor,
		2:                         a.ControlAddresses,
		3:                         a.ChangeWorkerAddress,
		4:                         a.ChangePeerID,
		5:                         a.SubmitWindowedPoSt,
		6:                         a.PreCommitSector,
		7:                         a.ProveCommitSector,
		8:                         a.ExtendSectorExpiration,
		9:                         a.TerminateSectors,
		10:                        a.DeclareFaults,
		11:                        a.DeclareFaultsRecovered,
		12:                        a.OnDeferredCronEvent,
		13:                        a.CheckSectorProven,
		14:                        a.ApplyRewards,
		15:                        a.ReportConsensusFault,
		16:                        a.WithdrawBalance,
		17:                        a.ConfirmSectorProofsValid,
		18:                        a.ChangeMultiaddrs,
		19:                        a.CompactPartitions,
		20:                        a.CompactSectorNumbers,
		21:                        a.ConfirmUpdateWorkerKey,
		22:                        a.RepayDebt,
		23:                        a.ChangeOwnerAddress,
		24:                        a.DisputeWindowedPoSt,
		25:                        a.PreCommitSectorBatch,
		26:                        a.ProveCommitAggregate,
		27:                        a.ProveReplicaUpdates,
	}
}

func (a Actor) Code() cid.Cid {
	return builtin.StorageMinerActorCodeID
}

func (a Actor) State() cbor.Er {
	return new(State)
}

var _ runtime.VMActor = Actor{}

/////////////////
// Constructor //
/////////////////

// Storage miner actors are created exclusively by the storage power actor. In order to break a circular dependency
// between the two, the construction parameters are defined in the power actor.
type ConstructorParams = power.MinerConstructorParams

func (a Actor) Constructor(rt Runtime, params *ConstructorParams) *abi.EmptyValue {
	rt.ValidateImmediateCallerIs(builtin.InitActorAddr)

	checkControlAddresses(rt, params.ControlAddrs)
	checkPeerInfo(rt, params.PeerId, params.Multiaddrs)

	if !CanWindowPoStProof(params.WindowPoStProofType) {
		rt.Abortf(exitcode.ErrIllegalArgument, "proof type %d not allowed for new miner actors", params.WindowPoStProofType)
	}

	owner := resolveControlAddress(rt, params.OwnerAddr)
	worker := resolveWorkerAddress(rt, params.WorkerAddr)
	controlAddrs := make([]addr.Address, 0, len(params.ControlAddrs))
	for _, ca := range params.ControlAddrs {
		resolved := resolveControlAddress(rt, ca)
		controlAddrs = append(controlAddrs, resolved)
	}

	currEpoch := rt.CurrEpoch()
	offset, err := assignProvingPeriodOffset(rt.Receiver(), currEpoch, rt.HashBlake2b)
	builtin.RequireNoErr(rt, err, exitcode.ErrSerialization, "failed to assign proving period offset")
	periodStart := currentProvingPeriodStart(currEpoch, offset)
	builtin.RequireState(rt, periodStart <= currEpoch, "computed proving period start %d after current epoch %d", periodStart, currEpoch)
	deadlineIndex := currentDeadlineIndex(currEpoch, periodStart)
	builtin.RequireState(rt, deadlineIndex < WPoStPeriodDeadlines, "computed proving deadline index %d invalid", deadlineIndex)

	info, err := ConstructMinerInfo(owner, worker, controlAddrs, params.PeerId, params.Multiaddrs, params.WindowPoStProofType)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to construct initial miner info")
	infoCid := rt.StorePut(info)

	store := adt.AsStore(rt)
	state, err := ConstructState(store, infoCid, periodStart, deadlineIndex)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to construct state")
	rt.StateCreate(state)

	return nil
}

/////////////
// Control //
/////////////

// type GetControlAddressesReturn struct {
// 	Owner        addr.Address
// 	Worker       addr.Address
// 	ControlAddrs []addr.Address
// }
type GetControlAddressesReturn = miner2.GetControlAddressesReturn

func (a Actor) ControlAddresses(rt Runtime, _ *abi.EmptyValue) *GetControlAddressesReturn {
	rt.ValidateImmediateCallerAcceptAny()
	var st State
	rt.StateReadonly(&st)
	info := getMinerInfo(rt, &st)
	return &GetControlAddressesReturn{
		Owner:        info.Owner,
		Worker:       info.Worker,
		ControlAddrs: info.ControlAddresses,
	}
}

//type ChangeWorkerAddressParams struct {
//	NewWorker       addr.Address
//	NewControlAddrs []addr.Address
//}
type ChangeWorkerAddressParams = miner0.ChangeWorkerAddressParams

// ChangeWorkerAddress will ALWAYS overwrite the existing control addresses with the control addresses passed in the params.
// If a nil addresses slice is passed, the control addresses will be cleared.
// A worker change will be scheduled if the worker passed in the params is different from the existing worker.
func (a Actor) ChangeWorkerAddress(rt Runtime, params *ChangeWorkerAddressParams) *abi.EmptyValue {
	checkControlAddresses(rt, params.NewControlAddrs)

	newWorker := resolveWorkerAddress(rt, params.NewWorker)

	var controlAddrs []addr.Address
	for _, ca := range params.NewControlAddrs {
		resolved := resolveControlAddress(rt, ca)
		controlAddrs = append(controlAddrs, resolved)
	}

	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		// Only the Owner is allowed to change the newWorker and control addresses.
		rt.ValidateImmediateCallerIs(info.Owner)

		// save the new control addresses
		info.ControlAddresses = controlAddrs

		// save newWorker addr key change request
		if newWorker != info.Worker && info.PendingWorkerKey == nil {
			info.PendingWorkerKey = &WorkerKeyChange{
				NewWorker:   newWorker,
				EffectiveAt: rt.CurrEpoch() + WorkerKeyChangeDelay,
			}
		}

		err := st.SaveInfo(adt.AsStore(rt), info)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not save miner info")
	})

	return nil
}

// Triggers a worker address change if a change has been requested and its effective epoch has arrived.
func (a Actor) ConfirmUpdateWorkerKey(rt Runtime, params *abi.EmptyValue) *abi.EmptyValue {
	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		// Only the Owner is allowed to change the newWorker.
		rt.ValidateImmediateCallerIs(info.Owner)

		processPendingWorker(info, rt, &st)
	})

	return nil
}

// Proposes or confirms a change of owner address.
// If invoked by the current owner, proposes a new owner address for confirmation. If the proposed address is the
// current owner address, revokes any existing proposal.
// If invoked by the previously proposed address, with the same proposal, changes the current owner address to be
// that proposed address.
func (a Actor) ChangeOwnerAddress(rt Runtime, newAddress *addr.Address) *abi.EmptyValue {
	if newAddress.Empty() {
		rt.Abortf(exitcode.ErrIllegalArgument, "empty address")
	}
	if newAddress.Protocol() != addr.ID {
		rt.Abortf(exitcode.ErrIllegalArgument, "owner address must be an ID address")
	}
	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)
		if rt.Caller() == info.Owner || info.PendingOwnerAddress == nil {
			// Propose new address.
			rt.ValidateImmediateCallerIs(info.Owner)
			info.PendingOwnerAddress = newAddress
		} else { // info.PendingOwnerAddress != nil
			// Confirm the proposal.
			// This validates that the operator can in fact use the proposed new address to sign messages.
			rt.ValidateImmediateCallerIs(*info.PendingOwnerAddress)
			if *newAddress != *info.PendingOwnerAddress {
				rt.Abortf(exitcode.ErrIllegalArgument, "expected confirmation of %v, got %v",
					info.PendingOwnerAddress, newAddress)
			}
			info.Owner = *info.PendingOwnerAddress
		}

		// Clear any resulting no-op change.
		if info.PendingOwnerAddress != nil && *info.PendingOwnerAddress == info.Owner {
			info.PendingOwnerAddress = nil
		}

		err := st.SaveInfo(adt.AsStore(rt), info)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save miner info")
	})
	return nil
}

//type ChangePeerIDParams struct {
//	NewID abi.PeerID
//}
type ChangePeerIDParams = miner0.ChangePeerIDParams

func (a Actor) ChangePeerID(rt Runtime, params *ChangePeerIDParams) *abi.EmptyValue {
	checkPeerInfo(rt, params.NewID, nil)

	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		info.PeerId = params.NewID
		err := st.SaveInfo(adt.AsStore(rt), info)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not save miner info")
	})
	return nil
}

//type ChangeMultiaddrsParams struct {
//	NewMultiaddrs []abi.Multiaddrs
//}
type ChangeMultiaddrsParams = miner0.ChangeMultiaddrsParams

func (a Actor) ChangeMultiaddrs(rt Runtime, params *ChangeMultiaddrsParams) *abi.EmptyValue {
	checkPeerInfo(rt, nil, params.NewMultiaddrs)

	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		info.Multiaddrs = params.NewMultiaddrs
		err := st.SaveInfo(adt.AsStore(rt), info)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not save miner info")
	})
	return nil
}

//////////////////
// WindowedPoSt //
//////////////////

//type PoStPartition struct {
//	// Partitions are numbered per-deadline, from zero.
//	Index uint64
//	// Sectors skipped while proving that weren't already declared faulty
//	Skipped bitfield.BitField
//}
type PoStPartition = miner0.PoStPartition

// Information submitted by a miner to provide a Window PoSt.
//type SubmitWindowedPoStParams struct {
//	// The deadline index which the submission targets.
//	Deadline uint64
//	// The partitions being proven.
//	Partitions []PoStPartition
//	// Array of proofs, one per distinct registered proof type present in the sectors being proven.
//	// In the usual case of a single proof type, this array will always have a single element (independent of number of partitions).
//	Proofs []proof.PoStProof
//	// The epoch at which these proofs is being committed to a particular chain.
//	// NOTE: This field should be removed in the future. See
//	// https://github.com/filecoin-project/specs-actors/issues/1094
//	ChainCommitEpoch abi.ChainEpoch
//	// The ticket randomness on the chain at the chain commit epoch.
//	ChainCommitRand abi.Randomness
//}
type SubmitWindowedPoStParams = miner0.SubmitWindowedPoStParams

// Invoked by miner's worker address to submit their fallback post
func (a Actor) SubmitWindowedPoSt(rt Runtime, params *SubmitWindowedPoStParams) *abi.EmptyValue {
	currEpoch := rt.CurrEpoch()
	store := adt.AsStore(rt)
	var st State

	// Verify that the miner has passed exactly 1 proof.
	if len(params.Proofs) != 1 {
		rt.Abortf(exitcode.ErrIllegalArgument, "expected exactly one proof, got %d", len(params.Proofs))
	}

	if !CanWindowPoStProof(params.Proofs[0].PoStProof) {
		rt.Abortf(exitcode.ErrIllegalArgument, "proof type %d not allowed", params.Proofs[0].PoStProof)
	}

	if params.Deadline >= WPoStPeriodDeadlines {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid deadline %d of %d", params.Deadline, WPoStPeriodDeadlines)
	}
	// Technically, ChainCommitRand should be _exactly_ 32 bytes. However:
	// 1. It's convenient to allow smaller slices when testing.
	// 2. Nothing bad will happen if the caller provides too little randomness.
	if len(params.ChainCommitRand) > abi.RandomnessLength {
		rt.Abortf(exitcode.ErrIllegalArgument, "expected at most %d bytes of randomness, got %d", abi.RandomnessLength, len(params.ChainCommitRand))
	}

	var postResult *PoStResult
	var info *MinerInfo
	rt.StateTransaction(&st, func() {
		info = getMinerInfo(rt, &st)
		maxProofSize, err := info.WindowPoStProofType.ProofSize()
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to determine max window post proof size")

		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		// Make sure the miner is using the correct proof type.
		if params.Proofs[0].PoStProof != info.WindowPoStProofType {
			rt.Abortf(exitcode.ErrIllegalArgument, "expected proof of type %d, got proof of type %d", info.WindowPoStProofType, params.Proofs[0])
		}

		// Make sure the proof size doesn't exceed the max. We could probably check for an exact match, but this is safer.
		if maxSize := maxProofSize * uint64(len(params.Partitions)); uint64(len(params.Proofs[0].ProofBytes)) > maxSize {
			rt.Abortf(exitcode.ErrIllegalArgument, "expected proof to be smaller than %d bytes", maxSize)
		}

		// Validate that the miner didn't try to prove too many partitions at once.
		submissionPartitionLimit := loadPartitionsSectorsMax(info.WindowPoStPartitionSectors)
		if uint64(len(params.Partitions)) > submissionPartitionLimit {
			rt.Abortf(exitcode.ErrIllegalArgument, "too many partitions %d, limit %d", len(params.Partitions), submissionPartitionLimit)
		}

		currDeadline := st.DeadlineInfo(currEpoch)
		// Check that the miner state indicates that the current proving deadline has started.
		// This should only fail if the cron actor wasn't invoked, and matters only in case that it hasn't been
		// invoked for a whole proving period, and hence the missed PoSt submissions from the prior occurrence
		// of this deadline haven't been processed yet.
		if !currDeadline.IsOpen() {
			rt.Abortf(exitcode.ErrIllegalState, "proving period %d not yet open at %d", currDeadline.PeriodStart, currEpoch)
		}

		// The miner may only submit a proof for the current deadline.
		if params.Deadline != currDeadline.Index {
			rt.Abortf(exitcode.ErrIllegalArgument, "invalid deadline %d at epoch %d, expected %d",
				params.Deadline, currEpoch, currDeadline.Index)
		}

		// Verify that the PoSt was committed to the chain at most WPoStChallengeLookback+WPoStChallengeWindow in the past.
		if params.ChainCommitEpoch < currDeadline.Challenge {
			rt.Abortf(exitcode.ErrIllegalArgument, "expected chain commit epoch %d to be after %d", params.ChainCommitEpoch, currDeadline.Challenge)
		}
		if params.ChainCommitEpoch >= currEpoch {
			rt.Abortf(exitcode.ErrIllegalArgument, "chain commit epoch %d must be less than the current epoch %d", params.ChainCommitEpoch, currEpoch)
		}
		// Verify the chain commit randomness.
		commRand := rt.GetRandomnessFromTickets(crypto.DomainSeparationTag_PoStChainCommit, params.ChainCommitEpoch, nil)
		if !bytes.Equal(commRand, params.ChainCommitRand) {
			rt.Abortf(exitcode.ErrIllegalArgument, "post commit randomness mismatched")
		}

		sectors, err := LoadSectors(store, st.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors")

		deadlines, err := st.LoadDeadlines(adt.AsStore(rt))
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		deadline, err := deadlines.LoadDeadline(store, params.Deadline)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", params.Deadline)

		// Record proven sectors/partitions, returning updates to power and the final set of sectors
		// proven/skipped.
		//
		// NOTE: This function does not actually check the proofs but does assume that they're correct. Instead,
		// it snapshots the deadline's state and the submitted proofs at the end of the challenge window and
		// allows third-parties to dispute these proofs.
		//
		// While we could perform _all_ operations at the end of challenge window, we do as we can here to avoid
		// overloading cron.
		faultExpiration := currDeadline.Last() + FaultMaxAge
		postResult, err = deadline.RecordProvenSectors(store, sectors, info.SectorSize, QuantSpecForDeadline(currDeadline), faultExpiration, params.Partitions)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to process post submission for deadline %d", params.Deadline)

		// Make sure we actually proved something.

		provenSectors, err := bitfield.SubtractBitField(postResult.Sectors, postResult.IgnoredSectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to determine proven sectors for deadline %d", params.Deadline)

		noSectors, err := provenSectors.IsEmpty()
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to determine if any sectors were proven", params.Deadline)
		if noSectors {
			// Abort verification if all sectors are (now) faults. There's nothing to prove.
			// It's not rational for a miner to submit a Window PoSt marking *all* non-faulty sectors as skipped,
			// since that will just cause them to pay a penalty at deadline end that would otherwise be zero
			// if they had *not* declared them.
			rt.Abortf(exitcode.ErrIllegalArgument, "cannot prove partitions with no active sectors")
		}

		// If we're not recovering power, record the proof for optimistic verification.
		if postResult.RecoveredPower.IsZero() {
			err = deadline.RecordPoStProofs(store, postResult.Partitions, params.Proofs)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to record proof for optimistic verification", params.Deadline)
		} else {
			// otherwise, check the proof
			sectorInfos, err := sectors.LoadForProof(postResult.Sectors, postResult.IgnoredSectors)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors for post verification")

			err = verifyWindowedPost(rt, currDeadline.Challenge, sectorInfos, params.Proofs)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "window post failed")
		}

		err = deadlines.UpdateDeadline(store, params.Deadline, deadline)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to update deadline %d", params.Deadline)

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
	})

	// Restore power for recovered sectors. Remove power for new faults.
	// NOTE: It would be permissible to delay the power loss until the deadline closes, but that would require
	// additional accounting state.
	// https://github.com/filecoin-project/specs-actors/issues/414
	requestUpdatePower(rt, postResult.PowerDelta)

	rt.StateReadonly(&st)
	err := st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	return nil
}

// type DisputeWindowedPoStParams struct {
// 		Deadline  uint64
// 		PoStIndex uint64 // only one is allowed at a time to avoid loading too many sector infos.
// }
type DisputeWindowedPoStParams = miner3.DisputeWindowedPoStParams

func (a Actor) DisputeWindowedPoSt(rt Runtime, params *DisputeWindowedPoStParams) *abi.EmptyValue {
	rt.ValidateImmediateCallerType(builtin.CallerTypesSignable...)
	reporter := rt.Caller()

	if params.Deadline >= WPoStPeriodDeadlines {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid deadline %d of %d", params.Deadline, WPoStPeriodDeadlines)
	}

	currEpoch := rt.CurrEpoch()

	// Note: these are going to be slightly inaccurate as time
	// will have moved on from when the post was actually
	// submitted.
	//
	// However, these are estimates _anyways_.
	epochReward := requestCurrentEpochBlockReward(rt)
	pwrTotal := requestCurrentTotalPower(rt)

	toBurn := abi.NewTokenAmount(0)
	toReward := abi.NewTokenAmount(0)
	pledgeDelta := abi.NewTokenAmount(0)
	powerDelta := NewPowerPairZero()
	var st State
	rt.StateTransaction(&st, func() {
		dlInfo := st.DeadlineInfo(currEpoch)
		if !deadlineAvailableForOptimisticPoStDispute(dlInfo.PeriodStart, params.Deadline, currEpoch) {
			rt.Abortf(exitcode.ErrForbidden, "can only dispute window posts during the dispute window (%d epochs after the challenge window closes)", WPoStDisputeWindow)
		}

		info := getMinerInfo(rt, &st)
		penalisedPower := NewPowerPairZero()
		store := adt.AsStore(rt)

		// Check proof
		{
			// Find the proving period start for the deadline in question.
			ppStart := dlInfo.PeriodStart
			if dlInfo.Index < params.Deadline {
				ppStart -= WPoStProvingPeriod
			}
			targetDeadline := NewDeadlineInfo(ppStart, params.Deadline, currEpoch)

			// Load the target deadline.
			deadlinesCurrent, err := st.LoadDeadlines(store)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

			dlCurrent, err := deadlinesCurrent.LoadDeadline(store, params.Deadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline")

			// Take the post from the snapshot for dispute.
			// This operation REMOVES the PoSt from the snapshot so
			// it can't be disputed again. If this method fails,
			// this operation must be rolled back.
			partitions, proofs, err := dlCurrent.TakePoStProofs(store, params.PoStIndex)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load proof for dispute")

			// Load the partition info we need for the dispute.
			disputeInfo, err := dlCurrent.LoadPartitionsForDispute(store, partitions)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load partition info for dispute")
			// This includes power that is no longer active (e.g., due to sector terminations).
			// It must only be used for penalty calculations, not power adjustments.
			penalisedPower = disputeInfo.DisputedPower

			// Load sectors for the dispute.
			sectors, err := LoadSectors(store, dlCurrent.SectorsSnapshot)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors snapshot array")

			sectorInfos, err := sectors.LoadForProof(disputeInfo.AllSectorNos, disputeInfo.IgnoredSectorNos)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors to dispute window post")

			// Check proof, we fail if validation succeeds.
			err = verifyWindowedPost(rt, targetDeadline.Challenge, sectorInfos, proofs)
			if err == nil {
				rt.Abortf(exitcode.ErrIllegalArgument, "failed to dispute valid post")
				return
			}
			rt.Log(rtt.INFO, "successfully disputed: %s", err)

			// Ok, now we record faults. This always works because
			// we don't allow compaction/moving sectors during the
			// challenge window.
			//
			// However, some of these sectors may have been
			// terminated. That's fine, we'll skip them.
			faultExpirationEpoch := targetDeadline.Last() + FaultMaxAge
			powerDelta, err = dlCurrent.RecordFaults(store, sectors, info.SectorSize, QuantSpecForDeadline(targetDeadline), faultExpirationEpoch, disputeInfo.DisputedSectors)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to declare faults")

			err = deadlinesCurrent.UpdateDeadline(store, params.Deadline, dlCurrent)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to update deadline %d", params.Deadline)
			err = st.SaveDeadlines(store, deadlinesCurrent)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
		}

		// Penalties.
		{
			// Calculate the base penalty.
			penaltyBase := PledgePenaltyForInvalidWindowPoSt(
				epochReward.ThisEpochRewardSmoothed,
				pwrTotal.QualityAdjPowerSmoothed,
				penalisedPower.QA,
			)

			// Calculate the target reward.
			rewardTarget := RewardForDisputedWindowPoSt(info.WindowPoStProofType, penalisedPower)

			// Compute the target penalty by adding the
			// base penalty to the target reward. We don't
			// take reward out of the penalty as the miner
			// could end up receiving a substantial
			// portion of their fee back as a reward.
			penaltyTarget := big.Add(penaltyBase, rewardTarget)

			err := st.ApplyPenalty(penaltyTarget)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")
			penaltyFromVesting, penaltyFromBalance, err := st.RepayPartialDebtInPriorityOrder(store, currEpoch, rt.CurrentBalance())
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to pay debt")
			toBurn = big.Add(penaltyFromVesting, penaltyFromBalance)

			// Now, move as much of the target reward as
			// we can from the burn to the reward.
			toReward = big.Min(toBurn, rewardTarget)
			toBurn = big.Sub(toBurn, toReward)

			pledgeDelta = penaltyFromVesting.Neg()
		}
	})

	requestUpdatePower(rt, powerDelta)

	if !toReward.IsZero() {
		// Try to send the reward to the reporter.
		code := rt.Send(reporter, builtin.MethodSend, nil, toReward, &builtin.Discard{})

		// If we fail, log and burn the reward to make sure the balances remain correct.
		if !code.IsSuccess() {
			rt.Log(rtt.ERROR, "failed to send reward")
			toBurn = big.Add(toBurn, toReward)
		}
	}
	burnFunds(rt, toBurn, BurnMethodDisputeWindowedPoSt)
	notifyPledgeChanged(rt, pledgeDelta)
	rt.StateReadonly(&st)

	err := st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")
	return nil
}

///////////////////////
// Sector Commitment //
///////////////////////

//type SectorPreCommitInfo struct {
//	SealProof       abi.RegisteredSealProof
//	SectorNumber    abi.SectorNumber
//	SealedCID       cid.Cid `checked:"true"` // CommR
//	SealRandEpoch   abi.ChainEpoch
//	DealIDs         []abi.DealID
//	Expiration      abi.ChainEpoch
//	ReplaceCapacity bool                    // Must be false since v7
//	ReplaceSectorDeadline  uint64           // Unused since v7
//	ReplaceSectorPartition uint64           // Unused since v7
//	ReplaceSectorNumber    abi.SectorNumber // Unused since v7
//}
type PreCommitSectorParams = miner0.SectorPreCommitInfo

// Pledges to seal and commit a single sector.
// See PreCommitSectorBatch for details.
// This method may be deprecated and removed in the future.
func (a Actor) PreCommitSector(rt Runtime, params *PreCommitSectorParams) *abi.EmptyValue {
	// This is a direct method call to self, not a message send.
	batchParams := &PreCommitSectorBatchParams{Sectors: []miner0.SectorPreCommitInfo{*params}}
	a.PreCommitSectorBatch(rt, batchParams)
	return nil
}

//type PreCommitSectorBatchParams struct {
//	Sectors []miner0.SectorPreCommitInfo
//}
type PreCommitSectorBatchParams = miner5.PreCommitSectorBatchParams

// Pledges the miner to seal and commit some new sectors.
// The caller specifies sector numbers, sealed sector data CIDs, seal randomness epoch, expiration, and the IDs
// of any storage deals contained in the sector data. The storage deal proposals must be already submitted
// to the storage market actor.
// This method calculates the sector's power, locks a pre-commit deposit for the sector, stores information about the
// sector in state and waits for it to be proven or expire.
func (a Actor) PreCommitSectorBatch(rt Runtime, params *PreCommitSectorBatchParams) *abi.EmptyValue {
	currEpoch := rt.CurrEpoch()
	if len(params.Sectors) == 0 {
		rt.Abortf(exitcode.ErrIllegalArgument, "batch empty")
	} else if len(params.Sectors) > PreCommitSectorBatchMaxSize {
		rt.Abortf(exitcode.ErrIllegalArgument, "batch of %d too large, max %d", len(params.Sectors), PreCommitSectorBatchMaxSize)
	}

	// Check per-sector preconditions before opening state transaction or sending other messages.
	challengeEarliest := currEpoch - MaxPreCommitRandomnessLookback
	sectorsDeals := make([]market.SectorDeals, len(params.Sectors))
	sectorNumbers := bitfield.New()
	for i, precommit := range params.Sectors {
		// Bitfied.IsSet() is fast when there are only locally-set values.
		set, err := sectorNumbers.IsSet(uint64(precommit.SectorNumber))
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "error checking sector number")
		if set {
			rt.Abortf(exitcode.ErrIllegalArgument, "duplicate sector number %d", precommit.SectorNumber)
		}
		sectorNumbers.Set(uint64(precommit.SectorNumber))

		if !CanPreCommitSealProof(precommit.SealProof) {
			rt.Abortf(exitcode.ErrIllegalArgument, "unsupported seal proof type %v", precommit.SealProof)
		}
		if precommit.SectorNumber > abi.MaxSectorNumber {
			rt.Abortf(exitcode.ErrIllegalArgument, "sector number %d out of range 0..(2^63-1)", precommit.SectorNumber)
		}
		if !precommit.SealedCID.Defined() {
			rt.Abortf(exitcode.ErrIllegalArgument, "sealed CID undefined")
		}
		if precommit.SealedCID.Prefix() != SealedCIDPrefix {
			rt.Abortf(exitcode.ErrIllegalArgument, "sealed CID had wrong prefix")
		}
		if precommit.SealRandEpoch >= currEpoch {
			rt.Abortf(exitcode.ErrIllegalArgument, "seal challenge epoch %v must be before now %v", precommit.SealRandEpoch, rt.CurrEpoch())
		}
		if precommit.SealRandEpoch < challengeEarliest {
			rt.Abortf(exitcode.ErrIllegalArgument, "seal challenge epoch %v too old, must be after %v", precommit.SealRandEpoch, challengeEarliest)
		}

		// Require sector lifetime meets minimum by assuming activation happens at last epoch permitted for seal proof.
		// This could make sector maximum lifetime validation more lenient if the maximum sector limit isn't hit first.
		maxActivation := currEpoch + MaxProveCommitDuration[precommit.SealProof]
		validateExpiration(rt, maxActivation, precommit.Expiration, precommit.SealProof)

		if precommit.ReplaceCapacity {
			rt.Abortf(exitcode.SysErrForbidden, "cc upgrade through precommit discontinued, use lightweight cc upgrade instead")
		}

		sectorsDeals[i] = market.SectorDeals{
			SectorExpiry: precommit.Expiration,
			DealIDs:      precommit.DealIDs,
		}
	}

	// gather information from other actors
	rewardStats := requestCurrentEpochBlockReward(rt)
	pwrTotal := requestCurrentTotalPower(rt)
	dealWeights := requestDealWeights(rt, sectorsDeals)

	if len(dealWeights.Sectors) != len(params.Sectors) {
		rt.Abortf(exitcode.ErrIllegalState, "deal weight request returned %d records, expected %d",
			len(dealWeights.Sectors), len(params.Sectors))
	}

	store := adt.AsStore(rt)
	var st State
	var err error
	feeToBurn := abi.NewTokenAmount(0)
	var needsCron bool
	rt.StateTransaction(&st, func() {
		// Aggregate fee applies only when batching.
		if len(params.Sectors) > 1 {
			aggregateFee := AggregatePreCommitNetworkFee(len(params.Sectors), rt.BaseFee())
			// AggregateFee applied to fee debt to consolidate burn with outstanding debts
			err := st.ApplyPenalty(aggregateFee)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")
		}

		// available balance already accounts for fee debt so it is correct to call
		// this before RepayDebts. We would have to
		// subtract fee debt explicitly if we called this after.
		availableBalance, err := st.GetAvailableBalance(rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to calculate available balance")
		feeToBurn = RepayDebtsOrAbort(rt, &st)

		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		if ConsensusFaultActive(info, currEpoch) {
			rt.Abortf(exitcode.ErrForbidden, "pre-commit not allowed during active consensus fault")
		}

		chainInfos := make([]*SectorPreCommitOnChainInfo, len(params.Sectors))
		totalDepositRequired := big.Zero()
		cleanUpEvents := map[abi.ChainEpoch][]uint64{}
		dealCountMax := SectorDealsMax(info.SectorSize)
		for i, precommit := range params.Sectors {
			// Sector must have the same Window PoSt proof type as the miner's recorded seal type.
			sectorWPoStProof, err := precommit.SealProof.RegisteredWindowPoStProof()
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "failed to lookup Window PoSt proof type for sector seal proof %d", precommit.SealProof)
			if sectorWPoStProof != info.WindowPoStProofType {
				rt.Abortf(exitcode.ErrIllegalArgument, "sector Window PoSt proof type %d must match miner Window PoSt proof type %d (seal proof type %d)",
					sectorWPoStProof, info.WindowPoStProofType, precommit.SealProof)
			}

			if uint64(len(precommit.DealIDs)) > dealCountMax {
				rt.Abortf(exitcode.ErrIllegalArgument, "too many deals for sector %d > %d", len(precommit.DealIDs), dealCountMax)
			}

			// Ensure total deal space does not exceed sector size.
			dealWeight := dealWeights.Sectors[i]
			if dealWeight.DealSpace > uint64(info.SectorSize) {
				rt.Abortf(exitcode.ErrIllegalArgument, "deals too large to fit in sector %d > %d", dealWeight.DealSpace, info.SectorSize)
			}

			// Estimate the sector weight using the current epoch as an estimate for activation,
			// and compute the pre-commit deposit using that weight.
			// The sector's power will be recalculated when it's proven.
			duration := precommit.Expiration - currEpoch
			sectorWeight := QAPowerForWeight(info.SectorSize, duration, dealWeight.DealWeight, dealWeight.VerifiedDealWeight)
			depositReq := PreCommitDepositForPower(rewardStats.ThisEpochRewardSmoothed, pwrTotal.QualityAdjPowerSmoothed, sectorWeight)

			// Build on-chain record.
			chainInfos[i] = &SectorPreCommitOnChainInfo{
				Info:               SectorPreCommitInfo(precommit),
				PreCommitDeposit:   depositReq,
				PreCommitEpoch:     currEpoch,
				DealWeight:         dealWeight.DealWeight,
				VerifiedDealWeight: dealWeight.VerifiedDealWeight,
			}
			totalDepositRequired = big.Add(totalDepositRequired, depositReq)

			// Calculate pre-commit cleanup
			msd, ok := MaxProveCommitDuration[precommit.SealProof]
			if !ok {
				rt.Abortf(exitcode.ErrIllegalArgument, "no max seal duration set for proof type: %d", precommit.SealProof)
			}
			// PreCommitCleanUpDelay > 0 here is critical for the batch verification of proofs. Without it, if a proof arrived exactly on the
			// due epoch, ProveCommitSector would accept it, then the expiry event would remove it, and then
			// ConfirmSectorProofsValid would fail to find it.
			cleanUpBound := currEpoch + msd + ExpiredPreCommitCleanUpDelay
			cleanUpEvents[cleanUpBound] = append(cleanUpEvents[cleanUpBound], uint64(precommit.SectorNumber))
		}

		// Batch update actor state.
		if availableBalance.LessThan(totalDepositRequired) {
			rt.Abortf(exitcode.ErrInsufficientFunds, "insufficient funds %v for pre-commit deposit: %v", availableBalance, totalDepositRequired)
		}
		err = st.AddPreCommitDeposit(totalDepositRequired)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add pre-commit deposit %v", totalDepositRequired)

		err = st.AllocateSectorNumbers(store, sectorNumbers, DenyCollisions)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to allocate sector ids %v", sectorNumbers)

		err = st.PutPrecommittedSectors(store, chainInfos...)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to write pre-committed sectors")

		err = st.AddPreCommitCleanUps(store, cleanUpEvents)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add pre-commit expiry to queue")

		// Activate miner cron
		needsCron = !st.DeadlineCronActive
		st.DeadlineCronActive = true
	})

	burnFunds(rt, feeToBurn, BurnMethodPreCommitSectorBatch)
	rt.StateReadonly(&st)
	err = st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")
	if needsCron {
		newDlInfo := st.DeadlineInfo(currEpoch)
		enrollCronEvent(rt, newDlInfo.Last(), &CronEventPayload{
			EventType: CronEventProvingDeadline,
		})
	}

	return nil
}

//type ProveCommitAggregateParams struct {
//	SectorNumbers  bitfield.BitField
//	AggregateProof []byte
//}
type ProveCommitAggregateParams = miner5.ProveCommitAggregateParams

// Checks state of the corresponding sector pre-commitments and verifies aggregate proof of replication
// of these sectors. If valid, the sectors' deals are activated, sectors are assigned a deadline and charged pledge
// and precommit state is removed.
func (a Actor) ProveCommitAggregate(rt Runtime, params *ProveCommitAggregateParams) *abi.EmptyValue {
	aggSectorsCount, err := params.SectorNumbers.Count()
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to count aggregated sectors")
	if aggSectorsCount > MaxAggregatedSectors {
		rt.Abortf(exitcode.ErrIllegalArgument, "too many sectors addressed, addressed %d want <= %d", aggSectorsCount, MaxAggregatedSectors)
	} else if aggSectorsCount < MinAggregatedSectors {
		rt.Abortf(exitcode.ErrIllegalArgument, "too few sectors addressed, addressed %d want >= %d", aggSectorsCount, MinAggregatedSectors)
	}

	if uint64(len(params.AggregateProof)) > MaxAggregateProofSize {
		rt.Abortf(exitcode.ErrIllegalArgument, "sector prove-commit proof of size %d exceeds max size of %d",
			len(params.AggregateProof), MaxAggregateProofSize)
	}

	store := adt.AsStore(rt)
	var st State
	rt.StateReadonly(&st)

	info := getMinerInfo(rt, &st)
	rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

	precommits, err := st.GetAllPrecommittedSectors(store, params.SectorNumbers)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to get precommits")

	// compute data commitments and validate each precommit
	computeDataCommitmentsInputs := make([]*market.SectorDataSpec, len(precommits))
	precommitsToConfirm := []*SectorPreCommitOnChainInfo{}
	for i, precommit := range precommits {
		msd, ok := MaxProveCommitDuration[precommit.Info.SealProof]
		if !ok {
			rt.Abortf(exitcode.ErrIllegalState, "no max seal duration for proof type: %d", precommit.Info.SealProof)
		}
		proveCommitDue := precommit.PreCommitEpoch + msd
		if rt.CurrEpoch() > proveCommitDue {
			rt.Log(rtt.WARN, "skipping commitment for sector %d, too late at %d, due %d", precommit.Info.SectorNumber, rt.CurrEpoch(), proveCommitDue)
		} else {
			precommitsToConfirm = append(precommitsToConfirm, precommit)
		}
		// All sealProof types should match
		if i >= 1 {
			prevSealProof := precommits[i-1].Info.SealProof
			builtin.RequireState(rt, prevSealProof == precommit.Info.SealProof, "aggregate contains mismatched seal proofs %d and %d", prevSealProof, precommit.Info.SealProof)
		}

		computeDataCommitmentsInputs[i] = &market.SectorDataSpec{
			SectorType: precommit.Info.SealProof,
			DealIDs:    precommit.Info.DealIDs,
		}
	}

	// compute shared verification inputs
	commDs := requestUnsealedSectorCIDs(rt, computeDataCommitmentsInputs...)
	svis := make([]proof.AggregateSealVerifyInfo, 0)
	receiver := rt.Receiver()
	minerActorID, err := addr.IDFromAddress(receiver)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "runtime provided non-ID receiver address %s", receiver)
	buf := new(bytes.Buffer)
	err = receiver.MarshalCBOR(buf)
	receiverBytes := buf.Bytes()
	builtin.RequireNoErr(rt, err, exitcode.ErrSerialization, "failed to marshal address for seal verification challenge")

	for i, precommit := range precommits {
		interactiveEpoch := precommit.PreCommitEpoch + PreCommitChallengeDelay
		if rt.CurrEpoch() <= interactiveEpoch {
			rt.Abortf(exitcode.ErrForbidden, "too early to prove sector %d", precommit.Info.SectorNumber)
		}

		svInfoRandomness := rt.GetRandomnessFromTickets(crypto.DomainSeparationTag_SealRandomness, precommit.Info.SealRandEpoch, receiverBytes)
		svInfoInteractiveRandomness := rt.GetRandomnessFromBeacon(crypto.DomainSeparationTag_InteractiveSealChallengeSeed, interactiveEpoch, receiverBytes)
		svi := proof.AggregateSealVerifyInfo{
			Number:                precommit.Info.SectorNumber,
			InteractiveRandomness: abi.InteractiveSealRandomness(svInfoInteractiveRandomness),
			Randomness:            abi.SealRandomness(svInfoRandomness),
			SealedCID:             precommit.Info.SealedCID,
			UnsealedCID:           commDs[i],
		}
		svis = append(svis, svi)
	}

	builtin.RequireState(rt, len(precommits) > 0, "bitfield non-empty but zero precommits read from state")
	sealProof := precommits[0].Info.SealProof
	err = rt.VerifyAggregateSeals(
		proof.AggregateSealVerifyProofAndInfos{
			Infos:          svis,
			Proof:          params.AggregateProof,
			Miner:          abi.ActorID(minerActorID),
			SealProof:      sealProof,
			AggregateProof: abi.RegisteredAggregationProof_SnarkPackV1,
		})
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "aggregate seal verify failed")

	rew := requestCurrentEpochBlockReward(rt)
	pwr := requestCurrentTotalPower(rt)

	confirmSectorProofsValid(rt, precommitsToConfirm, rew.ThisEpochBaselinePower, rew.ThisEpochRewardSmoothed, pwr.QualityAdjPowerSmoothed)

	// Compute and burn the aggregate network fee. We need to re-load the state as
	// confirmSectorProofsValid can change it.
	rt.StateReadonly(&st)
	aggregateFee := AggregateProveCommitNetworkFee(len(precommitsToConfirm), rt.BaseFee())
	unlockedBalance, err := st.GetUnlockedBalance(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to determine unlocked balance")
	if unlockedBalance.LessThan(aggregateFee) {
		rt.Abortf(exitcode.ErrInsufficientFunds,
			"remaining unlocked funds after prove-commit (%s) are insufficient to pay aggregation fee of %s",
			unlockedBalance, aggregateFee,
		)
	}
	burnFunds(rt, aggregateFee, BurnMethodProveCommitAggregate)

	err = st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	return nil
}

//type ProveCommitSectorParams struct {
//	SectorNumber abi.SectorNumber
//	ReplicaProof        []byte
//}
type ProveCommitSectorParams = miner0.ProveCommitSectorParams

// Checks state of the corresponding sector pre-commitment, then schedules the proof to be verified in bulk
// by the power actor.
// If valid, the power actor will call ConfirmSectorProofsValid at the end of the same epoch as this message.
func (a Actor) ProveCommitSector(rt Runtime, params *ProveCommitSectorParams) *abi.EmptyValue {
	rt.ValidateImmediateCallerAcceptAny()

	if params.SectorNumber > abi.MaxSectorNumber {
		rt.Abortf(exitcode.ErrIllegalArgument, "sector number greater than maximum")
	}

	store := adt.AsStore(rt)
	sectorNo := params.SectorNumber

	var st State
	rt.StateReadonly(&st)

	precommit, found, err := st.GetPrecommittedSector(store, sectorNo)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load pre-committed sector %v", sectorNo)
	if !found {
		rt.Abortf(exitcode.ErrNotFound, "no pre-committed sector %v", sectorNo)
	}

	maxProofSize, err := precommit.Info.SealProof.ProofSize()
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to determine max proof size for sector %v", sectorNo)
	if uint64(len(params.Proof)) > maxProofSize {
		rt.Abortf(exitcode.ErrIllegalArgument, "sector prove-commit proof of size %d exceeds max size of %d",
			len(params.Proof), maxProofSize)
	}

	msd, ok := MaxProveCommitDuration[precommit.Info.SealProof]
	if !ok {
		rt.Abortf(exitcode.ErrIllegalState, "no max seal duration for proof type: %d", precommit.Info.SealProof)
	}
	proveCommitDue := precommit.PreCommitEpoch + msd
	if rt.CurrEpoch() > proveCommitDue {
		rt.Abortf(exitcode.ErrIllegalArgument, "commitment proof for %d too late at %d, due %d", sectorNo, rt.CurrEpoch(), proveCommitDue)
	}

	svi := getVerifyInfo(rt, &SealVerifyStuff{
		SealedCID:           precommit.Info.SealedCID,
		InteractiveEpoch:    precommit.PreCommitEpoch + PreCommitChallengeDelay,
		SealRandEpoch:       precommit.Info.SealRandEpoch,
		Proof:               params.Proof,
		DealIDs:             precommit.Info.DealIDs,
		SectorNumber:        precommit.Info.SectorNumber,
		RegisteredSealProof: precommit.Info.SealProof,
	})

	code := rt.Send(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.SubmitPoRepForBulkVerify,
		svi,
		abi.NewTokenAmount(0),
		&builtin.Discard{},
	)
	builtin.RequireSuccess(rt, code, "failed to submit proof for bulk verification")
	return nil
}

func (a Actor) ConfirmSectorProofsValid(rt Runtime, params *builtin.ConfirmSectorProofsParams) *abi.EmptyValue {
	rt.ValidateImmediateCallerIs(builtin.StoragePowerActorAddr)

	// This should be enforced by the power actor. We log here just in case
	// something goes wrong.
	if len(params.Sectors) > power.MaxMinerProveCommitsPerEpoch {
		rt.Log(rtt.WARN, "confirmed more prove commits in an epoch than permitted: %d > %d",
			len(params.Sectors), power.MaxMinerProveCommitsPerEpoch,
		)
	}

	var st State
	rt.StateReadonly(&st)
	store := adt.AsStore(rt)

	// This skips missing pre-commits.
	precommittedSectors, err := st.FindPrecommittedSectors(store, params.Sectors...)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load pre-committed sectors")

	confirmSectorProofsValid(rt, precommittedSectors, params.RewardBaselinePower, params.RewardSmoothed, params.QualityAdjPowerSmoothed)

	return nil
}

func confirmSectorProofsValid(rt Runtime, preCommits []*SectorPreCommitOnChainInfo, thisEpochBaselinePower big.Int,
	thisEpochRewardSmoothed smoothing.FilterEstimate, qualityAdjPowerSmoothed smoothing.FilterEstimate) {

	circulatingSupply := rt.TotalFilCircSupply()

	// 1. Activate deals, skipping pre-commits with invalid deals.
	//    - calls the market actor.
	// 2. Add new sectors.
	//    - loads and saves sectors.
	//    - loads and saves deadlines/partitions
	//
	// Ideally, we'd combine some of these operations, but at least we have
	// a constant number of them.

	activation := rt.CurrEpoch()
	// Pre-commits for new sectors.
	var validPreCommits []*SectorPreCommitOnChainInfo
	for _, precommit := range preCommits {
		if len(precommit.Info.DealIDs) > 0 {
			// Check (and activate) storage deals associated to sector. Abort if checks failed.
			// TODO: we should batch these calls...
			// https://github.com/filecoin-project/specs-actors/issues/474
			code := rt.Send(
				builtin.StorageMarketActorAddr,
				builtin.MethodsMarket.ActivateDeals,
				&market.ActivateDealsParams{
					DealIDs:      precommit.Info.DealIDs,
					SectorExpiry: precommit.Info.Expiration,
				},
				abi.NewTokenAmount(0),
				&builtin.Discard{},
			)

			if code != exitcode.Ok {
				rt.Log(rtt.INFO, "failed to activate deals on sector %d, dropping from prove commit set", precommit.Info.SectorNumber)
				continue
			}
		}

		validPreCommits = append(validPreCommits, precommit)
	}

	// When all prove commits have failed abort early
	if len(validPreCommits) == 0 {
		rt.Abortf(exitcode.ErrIllegalArgument, "all prove commits failed to validate")
	}

	totalPledge := big.Zero()
	depositToUnlock := big.Zero()
	newSectors := make([]*SectorOnChainInfo, 0)
	newlyVested := big.Zero()
	var st State
	store := adt.AsStore(rt)
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		newSectorNos := make([]abi.SectorNumber, 0, len(validPreCommits))
		for _, precommit := range validPreCommits {
			// compute initial pledge
			duration := precommit.Info.Expiration - activation
			// This should have been caught in precommit, but don't let other sectors fail because of it.
			if duration < MinSectorExpiration {
				rt.Log(rtt.WARN, "precommit %d has lifetime %d less than minimum. ignoring", precommit.Info.SectorNumber, duration, MinSectorExpiration)
				continue
			}
			pwr := QAPowerForWeight(info.SectorSize, duration, precommit.DealWeight, precommit.VerifiedDealWeight)

			dayReward := ExpectedRewardForPower(thisEpochRewardSmoothed, qualityAdjPowerSmoothed, pwr, builtin.EpochsInDay)
			// The storage pledge is recorded for use in computing the penalty if this sector is terminated
			// before its declared expiration.
			// It's not capped to 1 FIL, so can exceed the actual initial pledge requirement.
			storagePledge := ExpectedRewardForPower(thisEpochRewardSmoothed, qualityAdjPowerSmoothed, pwr, InitialPledgeProjectionPeriod)
			initialPledge := InitialPledgeForPower(pwr, thisEpochBaselinePower, thisEpochRewardSmoothed,
				qualityAdjPowerSmoothed, circulatingSupply)

			newSectorInfo := SectorOnChainInfo{
				SectorNumber:          precommit.Info.SectorNumber,
				SealProof:             precommit.Info.SealProof,
				SealedCID:             precommit.Info.SealedCID,
				DealIDs:               precommit.Info.DealIDs,
				Expiration:            precommit.Info.Expiration,
				Activation:            activation,
				DealWeight:            precommit.DealWeight,
				VerifiedDealWeight:    precommit.VerifiedDealWeight,
				InitialPledge:         initialPledge,
				ExpectedDayReward:     dayReward,
				ExpectedStoragePledge: storagePledge,
				ReplacedSectorAge:     0,          // The replacement mechanism is disabled since v7
				ReplacedDayReward:     big.Zero(), // The replacement mechanism is disabled since v7
			}

			depositToUnlock = big.Add(depositToUnlock, precommit.PreCommitDeposit)
			newSectors = append(newSectors, &newSectorInfo)
			newSectorNos = append(newSectorNos, newSectorInfo.SectorNumber)
			totalPledge = big.Add(totalPledge, initialPledge)
		}

		err := st.PutSectors(store, newSectors...)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to put new sectors")

		err = st.DeletePrecommittedSectors(store, newSectorNos...)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to delete precommited sectors")

		err = st.AssignSectorsToDeadlines(store, rt.CurrEpoch(), newSectors, info.WindowPoStPartitionSectors, info.SectorSize)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to assign new sectors to deadlines")

		// Unlock deposit for successful proofs, make it available for lock-up as initial pledge.
		err = st.AddPreCommitDeposit(depositToUnlock.Neg())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add pre-commit deposit %v", depositToUnlock.Neg())

		unlockedBalance, err := st.GetUnlockedBalance(rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to calculate unlocked balance")
		if unlockedBalance.LessThan(totalPledge) {
			rt.Abortf(exitcode.ErrInsufficientFunds, "insufficient funds for aggregate initial pledge requirement %s, available: %s", totalPledge, unlockedBalance)
		}

		err = st.AddInitialPledge(totalPledge)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add initial pledge %v", totalPledge)
		err = st.CheckBalanceInvariants(rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")
	})

	// Request pledge update for activated sector.
	notifyPledgeChanged(rt, big.Sub(totalPledge, newlyVested))
}

//type CheckSectorProvenParams struct {
//	SectorNumber abi.SectorNumber
//}
type CheckSectorProvenParams = miner0.CheckSectorProvenParams

func (a Actor) CheckSectorProven(rt Runtime, params *CheckSectorProvenParams) *abi.EmptyValue {
	rt.ValidateImmediateCallerAcceptAny()

	if params.SectorNumber > abi.MaxSectorNumber {
		rt.Abortf(exitcode.ErrIllegalArgument, "sector number out of range")
	}

	var st State
	rt.StateReadonly(&st)
	store := adt.AsStore(rt)
	sectorNo := params.SectorNumber

	if _, found, err := st.GetSector(store, sectorNo); err != nil {
		rt.Abortf(exitcode.ErrIllegalState, "failed to load proven sector %v", sectorNo)
	} else if !found {
		rt.Abortf(exitcode.ErrNotFound, "sector %v not proven", sectorNo)
	}
	return nil
}

/////////////////////////
// Sector Modification //
/////////////////////////

//type ExtendSectorExpirationParams struct {
//	Extensions []ExpirationExtension
//}
type ExtendSectorExpirationParams = miner0.ExtendSectorExpirationParams

//type ExpirationExtension struct {
//	Deadline      uint64
//	Partition     uint64
//	Sectors       bitfield.BitField
//	NewExpiration abi.ChainEpoch
//}
type ExpirationExtension = miner0.ExpirationExtension

// Changes the expiration epoch for a sector to a new, later one.
// The sector must not be terminated or faulty.
// The sector's power is recomputed for the new expiration.
func (a Actor) ExtendSectorExpiration(rt Runtime, params *ExtendSectorExpirationParams) *abi.EmptyValue {
	if uint64(len(params.Extensions)) > DeclarationsMax {
		rt.Abortf(exitcode.ErrIllegalArgument, "too many declarations %d, max %d", len(params.Extensions), DeclarationsMax)
	}

	// limit the number of sectors declared at once
	// https://github.com/filecoin-project/specs-actors/issues/416
	var sectorCount uint64
	for _, decl := range params.Extensions {
		if decl.Deadline >= WPoStPeriodDeadlines {
			rt.Abortf(exitcode.ErrIllegalArgument, "deadline %d not in range 0..%d", decl.Deadline, WPoStPeriodDeadlines)
		}
		count, err := decl.Sectors.Count()
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument,
			"failed to count sectors for deadline %d, partition %d",
			decl.Deadline, decl.Partition,
		)
		if sectorCount > math.MaxUint64-count {
			rt.Abortf(exitcode.ErrIllegalArgument, "sector bitfield integer overflow")
		}
		sectorCount += count
	}
	if sectorCount > AddressedSectorsMax {
		rt.Abortf(exitcode.ErrIllegalArgument,
			"too many sectors for declaration %d, max %d",
			sectorCount, AddressedSectorsMax,
		)
	}

	currEpoch := rt.CurrEpoch()

	powerDelta := NewPowerPairZero()
	pledgeDelta := big.Zero()
	store := adt.AsStore(rt)
	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		deadlines, err := st.LoadDeadlines(adt.AsStore(rt))
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		// Group declarations by deadline, and remember iteration order.
		// This should be merged with the iteration outside the state transaction.
		declsByDeadline := map[uint64][]*ExpirationExtension{}
		var deadlinesToLoad []uint64
		for i := range params.Extensions {
			// Take a pointer to the value inside the slice, don't
			// take a reference to the temporary loop variable as it
			// will be overwritten every iteration.
			decl := &params.Extensions[i]
			if _, ok := declsByDeadline[decl.Deadline]; !ok {
				deadlinesToLoad = append(deadlinesToLoad, decl.Deadline)
			}
			declsByDeadline[decl.Deadline] = append(declsByDeadline[decl.Deadline], decl)
		}

		sectors, err := LoadSectors(store, st.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors array")

		for _, dlIdx := range deadlinesToLoad {
			deadline, err := deadlines.LoadDeadline(store, dlIdx)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", dlIdx)

			partitions, err := deadline.PartitionsArray(store)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load partitions for deadline %d", dlIdx)

			quant := st.QuantSpecForDeadline(dlIdx)

			// Group modified partitions by epoch to which they are extended. Duplicates are ok.
			partitionsByNewEpoch := map[abi.ChainEpoch][]uint64{}
			// Remember iteration order of epochs.
			var epochsToReschedule []abi.ChainEpoch

			for _, decl := range declsByDeadline[dlIdx] {
				var partition Partition
				found, err := partitions.Get(decl.Partition, &partition)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %v partition %v", dlIdx, decl.Partition)
				if !found {
					rt.Abortf(exitcode.ErrNotFound, "no such deadline %v partition %v", dlIdx, decl.Partition)
				}

				oldSectors, err := sectors.Load(decl.Sectors)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors in deadline %v partition %v", dlIdx, decl.Partition)
				newSectors := make([]*SectorOnChainInfo, len(oldSectors))
				for i, sector := range oldSectors {
					if !CanExtendSealProofType(sector.SealProof) {
						rt.Abortf(exitcode.ErrForbidden, "cannot extend expiration for sector %v with unsupported seal type %v",
							sector.SectorNumber, sector.SealProof)
					}
					// This can happen if the sector should have already expired, but hasn't
					// because the end of its deadline hasn't passed yet.
					if sector.Expiration < currEpoch {
						rt.Abortf(exitcode.ErrForbidden, "cannot extend expiration for expired sector %v, expired at %d, now %d",
							sector.SectorNumber,
							sector.Expiration,
							currEpoch,
						)
					}
					if decl.NewExpiration < sector.Expiration {
						rt.Abortf(exitcode.ErrIllegalArgument, "cannot reduce sector %v's expiration to %d from %d",
							sector.SectorNumber, decl.NewExpiration, sector.Expiration)
					}
					validateExpiration(rt, sector.Activation, decl.NewExpiration, sector.SealProof)

					// Remove "spent" deal weights
					newDealWeight := big.Div(
						big.Mul(sector.DealWeight, big.NewInt(int64(sector.Expiration-currEpoch))),
						big.NewInt(int64(sector.Expiration-sector.Activation)),
					)
					newVerifiedDealWeight := big.Div(
						big.Mul(sector.VerifiedDealWeight, big.NewInt(int64(sector.Expiration-currEpoch))),
						big.NewInt(int64(sector.Expiration-sector.Activation)),
					)

					newSector := *sector
					newSector.Expiration = decl.NewExpiration
					newSector.DealWeight = newDealWeight
					newSector.VerifiedDealWeight = newVerifiedDealWeight

					newSectors[i] = &newSector
				}

				// Overwrite sector infos.
				err = sectors.Store(newSectors...)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to update sectors %v", decl.Sectors)

				// Remove old sectors from partition and assign new sectors.
				partitionPowerDelta, partitionPledgeDelta, err := partition.ReplaceSectors(store, oldSectors, newSectors, info.SectorSize, quant)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to replace sector expirations at deadline %v partition %v", dlIdx, decl.Partition)

				powerDelta = powerDelta.Add(partitionPowerDelta)
				pledgeDelta = big.Add(pledgeDelta, partitionPledgeDelta) // expected to be zero, see note below.

				err = partitions.Set(decl.Partition, &partition)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadline %v partition %v", dlIdx, decl.Partition)

				// Record the new partition expiration epoch for setting outside this loop over declarations.
				prevEpochPartitions, ok := partitionsByNewEpoch[decl.NewExpiration]
				partitionsByNewEpoch[decl.NewExpiration] = append(prevEpochPartitions, decl.Partition)
				if !ok {
					epochsToReschedule = append(epochsToReschedule, decl.NewExpiration)
				}
			}

			deadline.Partitions, err = partitions.Root()
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save partitions for deadline %d", dlIdx)

			// Record partitions in deadline expiration queue
			for _, epoch := range epochsToReschedule {
				pIdxs := partitionsByNewEpoch[epoch]
				err := deadline.AddExpirationPartitions(store, epoch, pIdxs, quant)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add expiration partitions to deadline %v epoch %v: %v",
					dlIdx, epoch, pIdxs)
			}

			err = deadlines.UpdateDeadline(store, dlIdx, deadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadline %d", dlIdx)
		}

		st.Sectors, err = sectors.Root()
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save sectors")

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
	})

	requestUpdatePower(rt, powerDelta)
	// Note: the pledge delta is expected to be zero, since pledge is not re-calculated for the extension.
	// But in case that ever changes, we can do the right thing here.
	notifyPledgeChanged(rt, pledgeDelta)
	return nil
}

//type TerminateSectorsParams struct {
//	Terminations []TerminationDeclaration
//}
type TerminateSectorsParams = miner0.TerminateSectorsParams

//type TerminationDeclaration struct {
//	Deadline  uint64
//	Partition uint64
//	Sectors   bitfield.BitField
//}
type TerminationDeclaration = miner0.TerminationDeclaration

//type TerminateSectorsReturn struct {
//	// Set to true if all early termination work has been completed. When
//	// false, the miner may choose to repeatedly invoke TerminateSectors
//	// with no new sectors to process the remainder of the pending
//	// terminations. While pending terminations are outstanding, the miner
//	// will not be able to withdraw funds.
//	Done bool
//}
type TerminateSectorsReturn = miner0.TerminateSectorsReturn

// Marks some sectors as terminated at the present epoch, earlier than their
// scheduled termination, and adds these sectors to the early termination queue.
// This method then processes up to AddressedSectorsMax sectors and
// AddressedPartitionsMax partitions from the early termination queue,
// terminating deals, paying fines, and returning pledge collateral. While
// sectors remain in this queue:
//
//  1. The miner will be unable to withdraw funds.
//  2. The chain will process up to AddressedSectorsMax sectors and
//     AddressedPartitionsMax per epoch until the queue is empty.
//
// The sectors are immediately ignored for Window PoSt proofs, and should be
// masked in the same way as faulty sectors. A miner may not terminate sectors in the
// current deadline or the next deadline to be proven.
//
// This function may be invoked with no new sectors to explicitly process the
// next batch of sectors.
func (a Actor) TerminateSectors(rt Runtime, params *TerminateSectorsParams) *TerminateSectorsReturn {
	// Note: this cannot terminate pre-committed but un-proven sectors.
	// They must be allowed to expire (and deposit burnt).

	if len(params.Terminations) > DeclarationsMax {
		rt.Abortf(exitcode.ErrIllegalArgument,
			"too many declarations when terminating sectors: %d > %d",
			len(params.Terminations), DeclarationsMax,
		)
	}

	toProcess := make(DeadlineSectorMap)
	for _, term := range params.Terminations {
		err := toProcess.Add(term.Deadline, term.Partition, term.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument,
			"failed to process deadline %d, partition %d", term.Deadline, term.Partition,
		)
	}
	err := toProcess.Check(AddressedPartitionsMax, AddressedSectorsMax)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "cannot process requested parameters")

	var hadEarlyTerminations bool
	var st State
	store := adt.AsStore(rt)
	currEpoch := rt.CurrEpoch()
	powerDelta := NewPowerPairZero()
	rt.StateTransaction(&st, func() {
		hadEarlyTerminations = havePendingEarlyTerminations(rt, &st)

		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		deadlines, err := st.LoadDeadlines(adt.AsStore(rt))
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		// We're only reading the sectors, so there's no need to save this back.
		// However, we still want to avoid re-loading this array per-partition.
		sectors, err := LoadSectors(store, st.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors")

		err = toProcess.ForEach(func(dlIdx uint64, partitionSectors PartitionSectorMap) error {
			// If the deadline is the current or next deadline to prove, don't allow terminating sectors.
			// We assume that deadlines are immutable when being proven.
			if !deadlineIsMutable(st.CurrentProvingPeriodStart(currEpoch), dlIdx, currEpoch) {
				rt.Abortf(exitcode.ErrIllegalArgument, "cannot terminate sectors in immutable deadline %d", dlIdx)
			}

			quant := st.QuantSpecForDeadline(dlIdx)

			deadline, err := deadlines.LoadDeadline(store, dlIdx)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", dlIdx)

			removedPower, err := deadline.TerminateSectors(store, sectors, currEpoch, partitionSectors, info.SectorSize, quant)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to terminate sectors in deadline %d", dlIdx)

			st.EarlyTerminations.Set(dlIdx)

			powerDelta = powerDelta.Sub(removedPower)

			err = deadlines.UpdateDeadline(store, dlIdx, deadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to update deadline %d", dlIdx)

			return nil
		})
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to walk sectors")

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
	})

	epochReward := requestCurrentEpochBlockReward(rt)
	pwrTotal := requestCurrentTotalPower(rt)

	// Now, try to process these sectors.
	more := processEarlyTerminations(rt, epochReward.ThisEpochRewardSmoothed, pwrTotal.QualityAdjPowerSmoothed)
	if more && !hadEarlyTerminations {
		// We have remaining terminations, and we didn't _previously_
		// have early terminations to process, schedule a cron job.
		// NOTE: This isn't quite correct. If we repeatedly fill, empty,
		// fill, and empty, the queue, we'll keep scheduling new cron
		// jobs. However, in practice, that shouldn't be all that bad.
		scheduleEarlyTerminationWork(rt)
	}

	rt.StateReadonly(&st)
	err = st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	requestUpdatePower(rt, powerDelta)
	return &TerminateSectorsReturn{Done: !more}
}

////////////
// Faults //
////////////

//type DeclareFaultsParams struct {
//	Faults []FaultDeclaration
//}
type DeclareFaultsParams = miner0.DeclareFaultsParams

//type FaultDeclaration struct {
//	// The deadline to which the faulty sectors are assigned, in range [0..WPoStPeriodDeadlines)
//	Deadline uint64
//	// Partition index within the deadline containing the faulty sectors.
//	Partition uint64
//	// Sectors in the partition being declared faulty.
//	Sectors bitfield.BitField
//}
type FaultDeclaration = miner0.FaultDeclaration

func (a Actor) DeclareFaults(rt Runtime, params *DeclareFaultsParams) *abi.EmptyValue {
	if len(params.Faults) > DeclarationsMax {
		rt.Abortf(exitcode.ErrIllegalArgument,
			"too many fault declarations for a single message: %d > %d",
			len(params.Faults), DeclarationsMax,
		)
	}

	toProcess := make(DeadlineSectorMap)
	for _, term := range params.Faults {
		err := toProcess.Add(term.Deadline, term.Partition, term.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument,
			"failed to process deadline %d, partition %d", term.Deadline, term.Partition,
		)
	}
	err := toProcess.Check(AddressedPartitionsMax, AddressedSectorsMax)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "cannot process requested parameters")

	store := adt.AsStore(rt)
	var st State
	powerDelta := NewPowerPairZero()
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		deadlines, err := st.LoadDeadlines(store)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		sectors, err := LoadSectors(store, st.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors array")

		currEpoch := rt.CurrEpoch()
		err = toProcess.ForEach(func(dlIdx uint64, pm PartitionSectorMap) error {
			targetDeadline, err := declarationDeadlineInfo(st.CurrentProvingPeriodStart(currEpoch), dlIdx, currEpoch)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "invalid fault declaration deadline %d", dlIdx)

			err = validateFRDeclarationDeadline(targetDeadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "failed fault declaration at deadline %d", dlIdx)

			deadline, err := deadlines.LoadDeadline(store, dlIdx)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", dlIdx)

			faultExpirationEpoch := targetDeadline.Last() + FaultMaxAge
			deadlinePowerDelta, err := deadline.RecordFaults(store, sectors, info.SectorSize, QuantSpecForDeadline(targetDeadline), faultExpirationEpoch, pm)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to declare faults for deadline %d", dlIdx)

			err = deadlines.UpdateDeadline(store, dlIdx, deadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to store deadline %d partitions", dlIdx)

			powerDelta = powerDelta.Add(deadlinePowerDelta)
			return nil
		})
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to iterate deadlines")

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
	})

	// Remove power for new faulty sectors.
	// NOTE: It would be permissible to delay the power loss until the deadline closes, but that would require
	// additional accounting state.
	// https://github.com/filecoin-project/specs-actors/issues/414
	requestUpdatePower(rt, powerDelta)

	// Payment of penalty for declared faults is deferred to the deadline cron.
	return nil
}

//type DeclareFaultsRecoveredParams struct {
//	Recoveries []RecoveryDeclaration
//}
type DeclareFaultsRecoveredParams = miner0.DeclareFaultsRecoveredParams

//type RecoveryDeclaration struct {
//	// The deadline to which the recovered sectors are assigned, in range [0..WPoStPeriodDeadlines)
//	Deadline uint64
//	// Partition index within the deadline containing the recovered sectors.
//	Partition uint64
//	// Sectors in the partition being declared recovered.
//	Sectors bitfield.BitField
//}
type RecoveryDeclaration = miner0.RecoveryDeclaration

func (a Actor) DeclareFaultsRecovered(rt Runtime, params *DeclareFaultsRecoveredParams) *abi.EmptyValue {
	if len(params.Recoveries) > DeclarationsMax {
		rt.Abortf(exitcode.ErrIllegalArgument,
			"too many recovery declarations for a single message: %d > %d",
			len(params.Recoveries), DeclarationsMax,
		)
	}

	toProcess := make(DeadlineSectorMap)
	for _, term := range params.Recoveries {
		err := toProcess.Add(term.Deadline, term.Partition, term.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument,
			"failed to process deadline %d, partition %d", term.Deadline, term.Partition,
		)
	}
	err := toProcess.Check(AddressedPartitionsMax, AddressedSectorsMax)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "cannot process requested parameters")

	store := adt.AsStore(rt)
	var st State
	feeToBurn := abi.NewTokenAmount(0)
	rt.StateTransaction(&st, func() {
		// Verify unlocked funds cover both InitialPledgeRequirement and FeeDebt
		// and repay fee debt now.
		feeToBurn = RepayDebtsOrAbort(rt, &st)

		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)
		if ConsensusFaultActive(info, rt.CurrEpoch()) {
			rt.Abortf(exitcode.ErrForbidden, "recovery not allowed during active consensus fault")
		}

		deadlines, err := st.LoadDeadlines(adt.AsStore(rt))
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		sectors, err := LoadSectors(store, st.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors array")

		currEpoch := rt.CurrEpoch()
		err = toProcess.ForEach(func(dlIdx uint64, pm PartitionSectorMap) error {
			targetDeadline, err := declarationDeadlineInfo(st.CurrentProvingPeriodStart(currEpoch), dlIdx, currEpoch)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "invalid recovery declaration deadline %d", dlIdx)
			err = validateFRDeclarationDeadline(targetDeadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "failed recovery declaration at deadline %d", dlIdx)

			deadline, err := deadlines.LoadDeadline(store, dlIdx)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", dlIdx)

			err = deadline.DeclareFaultsRecovered(store, sectors, info.SectorSize, pm)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to declare recoveries for deadline %d", dlIdx)

			err = deadlines.UpdateDeadline(store, dlIdx, deadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to store deadline %d", dlIdx)
			return nil
		})
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to walk sectors")

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
	})

	burnFunds(rt, feeToBurn, BurnMethodDeclareFaultsRecovered)
	rt.StateReadonly(&st)
	err = st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	// Power is not restored yet, but when the recovered sectors are successfully PoSted.
	return nil
}

/////////////////
// Maintenance //
/////////////////

//type CompactPartitionsParams struct {
//	Deadline   uint64
//	Partitions bitfield.BitField
//}
type CompactPartitionsParams = miner0.CompactPartitionsParams

// Compacts a number of partitions at one deadline by removing terminated sectors, re-ordering the remaining sectors,
// and assigning them to new partitions so as to completely fill all but one partition with live sectors.
// The addressed partitions are removed from the deadline, and new ones appended.
// The final partition in the deadline is always included in the compaction, whether or not explicitly requested.
// Removed sectors are removed from state entirely.
// May not be invoked if the deadline has any un-processed early terminations.
func (a Actor) CompactPartitions(rt Runtime, params *CompactPartitionsParams) *abi.EmptyValue {
	if params.Deadline >= WPoStPeriodDeadlines {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid deadline %v", params.Deadline)
	}

	partitionCount, err := params.Partitions.Count()
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "failed to parse partitions bitfield")

	store := adt.AsStore(rt)
	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		if !deadlineAvailableForCompaction(st.CurrentProvingPeriodStart(rt.CurrEpoch()), params.Deadline, rt.CurrEpoch()) {
			rt.Abortf(exitcode.ErrForbidden,
				"cannot compact deadline %d during its challenge window, or the prior challenge window, or before %d epochs have passed since its last challenge window ended", params.Deadline, WPoStDisputeWindow)
		}

		submissionPartitionLimit := loadPartitionsSectorsMax(info.WindowPoStPartitionSectors)
		if partitionCount > submissionPartitionLimit {
			rt.Abortf(exitcode.ErrIllegalArgument, "too many partitions %d, limit %d", partitionCount, submissionPartitionLimit)
		}

		quant := st.QuantSpecForDeadline(params.Deadline)

		deadlines, err := st.LoadDeadlines(store)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		deadline, err := deadlines.LoadDeadline(store, params.Deadline)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", params.Deadline)

		live, dead, removedPower, err := deadline.RemovePartitions(store, params.Partitions, quant)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to remove partitions from deadline %d", params.Deadline)

		err = st.DeleteSectors(store, dead)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to delete dead sectors")

		sectors, err := st.LoadSectorInfos(store, live)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load moved sectors")

		proven := true
		addedPower, err := deadline.AddSectors(store, info.WindowPoStPartitionSectors, proven, sectors, info.SectorSize, quant)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add back moved sectors")

		if !removedPower.Equals(addedPower) {
			rt.Abortf(exitcode.ErrIllegalState, "power changed when compacting partitions: was %v, is now %v", removedPower, addedPower)
		}
		err = deadlines.UpdateDeadline(store, params.Deadline, deadline)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to update deadline %d", params.Deadline)

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")
	})
	return nil
}

//type CompactSectorNumbersParams struct {
//	MaskSectorNumbers bitfield.BitField
//}
type CompactSectorNumbersParams = miner0.CompactSectorNumbersParams

// Compacts sector number allocations to reduce the size of the allocated sector
// number bitfield.
//
// When allocating sector numbers sequentially, or in sequential groups, this
// bitfield should remain fairly small. However, if the bitfield grows large
// enough such that PreCommitSector fails (or becomes expensive), this method
// can be called to mask out (throw away) entire ranges of unused sector IDs.
// For example, if sectors 1-99 and 101-200 have been allocated, sector number
// 99 can be masked out to collapse these two ranges into one.
func (a Actor) CompactSectorNumbers(rt Runtime, params *CompactSectorNumbersParams) *abi.EmptyValue {
	lastSectorNo, err := params.MaskSectorNumbers.Last()
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "invalid mask bitfield")
	if lastSectorNo > abi.MaxSectorNumber {
		rt.Abortf(exitcode.ErrIllegalArgument, "masked sector number %d exceeded max sector number", lastSectorNo)
	}

	store := adt.AsStore(rt)
	var st State
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		err := st.AllocateSectorNumbers(store, params.MaskSectorNumbers, AllowCollisions)

		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to mask sector numbers")
	})
	return nil
}

///////////////////////
// Pledge Collateral //
///////////////////////

// Locks up some amount of the miner's unlocked balance (including funds received alongside the invoking message).
func (a Actor) ApplyRewards(rt Runtime, params *builtin.ApplyRewardParams) *abi.EmptyValue {
	if params.Reward.Sign() < 0 {
		rt.Abortf(exitcode.ErrIllegalArgument, "cannot lock up a negative amount of funds")
	}
	if params.Penalty.Sign() < 0 {
		rt.Abortf(exitcode.ErrIllegalArgument, "cannot penalize a negative amount of funds")
	}

	var st State
	pledgeDeltaTotal := big.Zero()
	toBurn := big.Zero()
	rt.StateTransaction(&st, func() {
		var err error
		store := adt.AsStore(rt)
		rt.ValidateImmediateCallerIs(builtin.RewardActorAddr)

		rewardToLock, lockedRewardVestingSpec := LockedRewardFromReward(params.Reward)

		// This ensures the miner has sufficient funds to lock up amountToLock.
		// This should always be true if reward actor sends reward funds with the message.
		unlockedBalance, err := st.GetUnlockedBalance(rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to calculate unlocked balance")
		if unlockedBalance.LessThan(rewardToLock) {
			rt.Abortf(exitcode.ErrInsufficientFunds, "insufficient funds to lock, available: %v, requested: %v", unlockedBalance, rewardToLock)
		}

		newlyVested, err := st.AddLockedFunds(store, rt.CurrEpoch(), rewardToLock, lockedRewardVestingSpec)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to lock funds in vesting table")
		pledgeDeltaTotal = big.Sub(pledgeDeltaTotal, newlyVested)
		pledgeDeltaTotal = big.Add(pledgeDeltaTotal, rewardToLock)

		// If the miner incurred block mining penalties charge these to miner's fee debt
		err = st.ApplyPenalty(params.Penalty)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")
		// Attempt to repay all fee debt in this call. In most cases the miner will have enough
		// funds in the *reward alone* to cover the penalty. In the rare case a miner incurs more
		// penalty than it can pay for with reward and existing funds, it will go into fee debt.
		penaltyFromVesting, penaltyFromBalance, err := st.RepayPartialDebtInPriorityOrder(store, rt.CurrEpoch(), rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to repay penalty")
		pledgeDeltaTotal = big.Sub(pledgeDeltaTotal, penaltyFromVesting)
		toBurn = big.Add(penaltyFromVesting, penaltyFromBalance)
	})

	notifyPledgeChanged(rt, pledgeDeltaTotal)
	burnFunds(rt, toBurn, BurnMethodApplyRewards)
	rt.StateReadonly(&st)
	err := st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	return nil
}

//type ReportConsensusFaultParams struct {
//	BlockHeader1     []byte
//	BlockHeader2     []byte
//	BlockHeaderExtra []byte
//}
type ReportConsensusFaultParams = miner0.ReportConsensusFaultParams

func (a Actor) ReportConsensusFault(rt Runtime, params *ReportConsensusFaultParams) *abi.EmptyValue {
	// Note: only the first report of any fault is processed because it sets the
	// ConsensusFaultElapsed state variable to an epoch after the fault, and reports prior to
	// that epoch are no longer valid.
	rt.ValidateImmediateCallerType(builtin.CallerTypesSignable...)
	reporter := rt.Caller()

	fault, err := rt.VerifyConsensusFault(params.BlockHeader1, params.BlockHeader2, params.BlockHeaderExtra)
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalArgument, "fault not verified: %s", err)
	}
	if fault.Target != rt.Receiver() {
		rt.Abortf(exitcode.ErrIllegalArgument, "fault by %v reported to miner %v", fault.Target, rt.Receiver())
	}

	// Elapsed since the fault (i.e. since the higher of the two blocks)
	currEpoch := rt.CurrEpoch()
	faultAge := currEpoch - fault.Epoch
	if faultAge <= 0 {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid fault epoch %v ahead of current %v", fault.Epoch, currEpoch)
	}

	// Penalize miner consensus fault fee
	// Give a portion of this to the reporter as reward
	var st State
	rewardStats := requestCurrentEpochBlockReward(rt)
	// The policy amounts we should burn and send to reporter
	// These may differ from actual funds send when miner goes into fee debt
	thisEpochReward := smoothing.Estimate(&rewardStats.ThisEpochRewardSmoothed)
	faultPenalty := ConsensusFaultPenalty(thisEpochReward)
	slasherReward := RewardForConsensusSlashReport(thisEpochReward)
	pledgeDelta := big.Zero()

	// The amounts actually sent to burnt funds and reporter
	burnAmount := big.Zero()
	rewardAmount := big.Zero()
	rt.StateTransaction(&st, func() {
		info := getMinerInfo(rt, &st)

		// verify miner hasn't already been faulted
		if fault.Epoch < info.ConsensusFaultElapsed {
			rt.Abortf(exitcode.ErrForbidden, "fault epoch %d is too old, last exclusion period ended at %d", fault.Epoch, info.ConsensusFaultElapsed)
		}

		err := st.ApplyPenalty(faultPenalty)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")

		// Pay penalty
		penaltyFromVesting, penaltyFromBalance, err := st.RepayPartialDebtInPriorityOrder(adt.AsStore(rt), currEpoch, rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to pay fees")
		// Burn the amount actually payable. Any difference in this and faultPenalty already recorded as FeeDebt
		burnAmount = big.Add(penaltyFromVesting, penaltyFromBalance)
		pledgeDelta = big.Add(pledgeDelta, penaltyFromVesting.Neg())

		// clamp reward at funds burnt
		rewardAmount = big.Min(burnAmount, slasherReward)
		// reduce burnAmount by rewardAmount
		burnAmount = big.Sub(burnAmount, rewardAmount)
		info.ConsensusFaultElapsed = currEpoch + ConsensusFaultIneligibilityDuration
		err = st.SaveInfo(adt.AsStore(rt), info)
		builtin.RequireNoErr(rt, err, exitcode.ErrSerialization, "failed to save miner info")
	})
	code := rt.Send(reporter, builtin.MethodSend, nil, rewardAmount, &builtin.Discard{})
	if !code.IsSuccess() {
		rt.Log(rtt.ERROR, "failed to send reward")
	}
	burnFunds(rt, burnAmount, BurnMethodReportConsensusFault)
	notifyPledgeChanged(rt, pledgeDelta)

	rt.StateReadonly(&st)
	err = st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	return nil
}

//type WithdrawBalanceParams struct {
//	AmountRequested abi.TokenAmount
//}
type WithdrawBalanceParams = miner0.WithdrawBalanceParams

// Attempt to withdraw the specified amount from the miner's available balance.
// Only owner key has permission to withdraw.
// If less than the specified amount is available, yields the entire available balance.
// Returns the amount withdrawn.
func (a Actor) WithdrawBalance(rt Runtime, params *WithdrawBalanceParams) *abi.TokenAmount {
	var st State
	if params.AmountRequested.LessThan(big.Zero()) {
		rt.Abortf(exitcode.ErrIllegalArgument, "negative fund requested for withdrawal: %s", params.AmountRequested)
	}
	var info *MinerInfo
	newlyVested := big.Zero()
	feeToBurn := big.Zero()
	availableBalance := big.Zero()
	rt.StateTransaction(&st, func() {
		var err error
		info = getMinerInfo(rt, &st)
		// Only the owner is allowed to withdraw the balance as it belongs to/is controlled by the owner
		// and not the worker.
		rt.ValidateImmediateCallerIs(info.Owner)

		// Ensure we don't have any pending terminations.
		if count, err := st.EarlyTerminations.Count(); err != nil {
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to count early terminations")
		} else if count > 0 {
			rt.Abortf(exitcode.ErrForbidden,
				"cannot withdraw funds while %d deadlines have terminated sectors with outstanding fees",
				count,
			)
		}

		// Unlock vested funds so we can spend them.
		newlyVested, err = st.UnlockVestedFunds(adt.AsStore(rt), rt.CurrEpoch())
		if err != nil {
			rt.Abortf(exitcode.ErrIllegalState, "failed to vest fund: %v", err)
		}
		// available balance already accounts for fee debt so it is correct to call
		// this before RepayDebts. We would have to
		// subtract fee debt explicitly if we called this after.
		availableBalance, err = st.GetAvailableBalance(rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to calculate available balance")

		// Verify unlocked funds cover both InitialPledgeRequirement and FeeDebt
		// and repay fee debt now.
		feeToBurn = RepayDebtsOrAbort(rt, &st)
	})

	amountWithdrawn := big.Min(availableBalance, params.AmountRequested)
	builtin.RequireState(rt, amountWithdrawn.GreaterThanEqual(big.Zero()), "negative amount to withdraw: %v", amountWithdrawn)
	builtin.RequireState(rt, amountWithdrawn.LessThanEqual(availableBalance), "amount to withdraw %v < available %v", amountWithdrawn, availableBalance)

	if amountWithdrawn.GreaterThan(abi.NewTokenAmount(0)) {
		code := rt.Send(info.Owner, builtin.MethodSend, nil, amountWithdrawn, &builtin.Discard{})
		builtin.RequireSuccess(rt, code, "failed to withdraw balance")
	}

	burnFunds(rt, feeToBurn, BurnMethodWithdrawBalance)

	pledgeDelta := newlyVested.Neg()
	notifyPledgeChanged(rt, pledgeDelta)

	err := st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	return &amountWithdrawn
}

func (a Actor) RepayDebt(rt Runtime, _ *abi.EmptyValue) *abi.EmptyValue {
	var st State
	var fromVesting, fromBalance abi.TokenAmount
	rt.StateTransaction(&st, func() {
		var err error
		info := getMinerInfo(rt, &st)
		rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

		// Repay as much fee debt as possible.
		fromVesting, fromBalance, err = st.RepayPartialDebtInPriorityOrder(adt.AsStore(rt), rt.CurrEpoch(), rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to unlock fee debt")
	})

	notifyPledgeChanged(rt, fromVesting.Neg())
	burnFunds(rt, big.Sum(fromVesting, fromBalance), BurnMethodRepayDebt)
	err := st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")

	return nil
}

type ReplicaUpdate = miner7.ReplicaUpdate

type ProveReplicaUpdatesParams = miner7.ProveReplicaUpdatesParams

func (a Actor) ProveReplicaUpdates(rt Runtime, params *ProveReplicaUpdatesParams) *bitfield.BitField {
	// Validate inputs

	builtin.RequireParam(rt, len(params.Updates) <= ProveReplicaUpdatesMaxSize, "too many updates (%d > %d)", len(params.Updates), ProveReplicaUpdatesMaxSize)

	store := adt.AsStore(rt)
	var stReadOnly State
	rt.StateReadonly(&stReadOnly)
	info := getMinerInfo(rt, &stReadOnly)

	rt.ValidateImmediateCallerIs(append(info.ControlAddresses, info.Owner, info.Worker)...)

	sectors, err := LoadSectors(store, stReadOnly.Sectors)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors array")

	powerDelta := NewPowerPairZero()
	pledgeDelta := big.Zero()

	type updateAndSectorInfo struct {
		update     *ReplicaUpdate
		sectorInfo *SectorOnChainInfo
	}

	var sectorsDeals []market.SectorDeals
	var sectorsDataSpec []*market.SectorDataSpec
	var validatedUpdates []*updateAndSectorInfo
	sectorNumbers := bitfield.New()
	for i := range params.Updates {
		update := params.Updates[i]
		// Bitfied.IsSet() is fast when there are only locally-set values.
		set, err := sectorNumbers.IsSet(uint64(update.SectorID))
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "error checking sector number")
		if set {
			rt.Log(rtt.INFO, "duplicate sector being updated %d, skipping", update.SectorID)
			continue
		}

		sectorNumbers.Set(uint64(update.SectorID))

		if len(update.ReplicaProof) > 4096 {
			rt.Log(rtt.INFO, "update proof is too large (%d), skipping sector %d", len(update.ReplicaProof), update.SectorID)
			continue
		}

		if len(update.Deals) <= 0 {
			rt.Log(rtt.INFO, "must have deals to update, skipping sector %d", update.SectorID)
			continue
		}

		if uint64(len(update.Deals)) > SectorDealsMax(info.SectorSize) {
			rt.Log(rtt.INFO, "more deals than policy allows, skipping sector %d", update.SectorID)
			continue
		}

		if update.Deadline >= WPoStPeriodDeadlines {
			rt.Log(rtt.INFO, "deadline %d not in range 0..%d, skipping sector %d", update.Deadline, WPoStPeriodDeadlines, update.SectorID)
			continue
		}

		if !update.NewSealedSectorCID.Defined() {
			rt.Log(rtt.INFO, "new sealed CID undefined, skipping sector %d", update.SectorID)
			continue
		}

		if update.NewSealedSectorCID.Prefix() != SealedCIDPrefix {
			rt.Log(rtt.INFO, "new sealed CID had wrong prefix %s, skipping sector %d", update.NewSealedSectorCID, update.SectorID)
			continue
		}

		// If the deadline is the current or next deadline to prove, don't allow updating sectors.
		// We assume that deadlines are immutable when being proven.
		if !deadlineIsMutable(stReadOnly.CurrentProvingPeriodStart(rt.CurrEpoch()), update.Deadline, rt.CurrEpoch()) {
			rt.Log(rtt.INFO, "cannot upgrade sectors in immutable deadline %d, skipping sector %d", update.Deadline, update.SectorID)
			continue
		}

		healthy, err := stReadOnly.CheckSectorActive(store, update.Deadline, update.Partition, update.SectorID, true)

		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "error checking sector health")

		if !healthy {
			rt.Log(rtt.INFO, "sector isn't healthy, skipping sector %d", update.SectorID)
			continue
		}

		sectorInfo, err := sectors.MustGet(update.SectorID)
		if err != nil {
			rt.Log(rtt.INFO, "failed to get sector, skipping sector %d", update.SectorID)
			continue
		}

		if len(sectorInfo.DealIDs) != 0 {
			rt.Log(rtt.INFO, "cannot update sector with deals, skipping sector %d", update.SectorID)
			continue
		}

		code := rt.Send(
			builtin.StorageMarketActorAddr,
			builtin.MethodsMarket.ActivateDeals,
			&market.ActivateDealsParams{
				DealIDs:      update.Deals,
				SectorExpiry: sectorInfo.Expiration,
			},
			abi.NewTokenAmount(0),
			&builtin.Discard{},
		)

		if code != exitcode.Ok {
			rt.Log(rtt.INFO, "failed to activate deals, skipping sector %d", update.SectorID)
			continue
		}

		validatedUpdates = append(validatedUpdates, &updateAndSectorInfo{
			update:     &update,
			sectorInfo: sectorInfo,
		})

		sectorsDeals = append(sectorsDeals, market.SectorDeals{DealIDs: update.Deals, SectorExpiry: sectorInfo.Expiration})
		sectorsDataSpec = append(sectorsDataSpec, &market.SectorDataSpec{
			SectorType: sectorInfo.SealProof,
			DealIDs:    update.Deals,
		})
	}

	builtin.RequireParam(rt, len(validatedUpdates) > 0, "no valid updates")

	// Errors past this point cause the ProveReplicaUpdates call to fail (no more skipping sectors)

	dealWeights := requestDealWeights(rt, sectorsDeals)
	builtin.RequirePredicate(rt, len(dealWeights.Sectors) == len(validatedUpdates), exitcode.ErrIllegalState,
		"deal weight request returned %d records, expected %d", len(dealWeights.Sectors), len(validatedUpdates))

	unsealedSectorCIDs := requestUnsealedSectorCIDs(rt, sectorsDataSpec...)
	builtin.RequirePredicate(rt, len(unsealedSectorCIDs) == len(validatedUpdates), exitcode.ErrIllegalState,
		"unsealed sector cid request returned %d records, expected %d", len(unsealedSectorCIDs), len(validatedUpdates))

	type updateWithDetails struct {
		update            *ReplicaUpdate
		sectorInfo        *SectorOnChainInfo
		dealWeight        market.SectorWeights
		unsealedSectorCID cid.Cid
	}

	// Group declarations by deadline
	declsByDeadline := map[uint64][]*updateWithDetails{}
	var deadlinesToLoad []uint64
	for i, updateWithSectorInfo := range validatedUpdates {
		if _, ok := declsByDeadline[updateWithSectorInfo.update.Deadline]; !ok {
			deadlinesToLoad = append(deadlinesToLoad, updateWithSectorInfo.update.Deadline)
		}
		declsByDeadline[updateWithSectorInfo.update.Deadline] = append(declsByDeadline[updateWithSectorInfo.update.Deadline], &updateWithDetails{
			update:            updateWithSectorInfo.update,
			sectorInfo:        updateWithSectorInfo.sectorInfo,
			dealWeight:        dealWeights.Sectors[i],
			unsealedSectorCID: unsealedSectorCIDs[i],
		})
	}

	rewRet := requestCurrentEpochBlockReward(rt)
	powRet := requestCurrentTotalPower(rt)

	succeededSectors := bitfield.New()
	var st State
	rt.StateTransaction(&st, func() {
		deadlines, err := st.LoadDeadlines(store)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadlines")

		newSectors := make([]*SectorOnChainInfo, len(validatedUpdates))
		for _, dlIdx := range deadlinesToLoad {
			deadline, err := deadlines.LoadDeadline(store, dlIdx)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %d", dlIdx)

			partitions, err := deadline.PartitionsArray(store)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load partitions for deadline %d", dlIdx)

			quant := st.QuantSpecForDeadline(dlIdx)

			for i, updateWithDetails := range declsByDeadline[dlIdx] {
				updateProofType, err := updateWithDetails.sectorInfo.SealProof.RegisteredUpdateProof()
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "couldn't load update proof type")
				builtin.RequirePredicate(rt, updateWithDetails.update.UpdateProofType == updateProofType, exitcode.ErrIllegalArgument, "unsupported update proof type %d", updateWithDetails.update.UpdateProofType)

				err = rt.VerifyReplicaUpdate(
					proof.ReplicaUpdateInfo{
						UpdateProofType:      updateProofType,
						NewSealedSectorCID:   updateWithDetails.update.NewSealedSectorCID,
						OldSealedSectorCID:   updateWithDetails.sectorInfo.SealedCID,
						NewUnsealedSectorCID: updateWithDetails.unsealedSectorCID,
						Proof:                updateWithDetails.update.ReplicaProof,
					})

				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "failed to verify replica proof for sector %d", updateWithDetails.sectorInfo.SectorNumber)

				newSectorInfo := *updateWithDetails.sectorInfo

				newSectorInfo.SealedCID = updateWithDetails.update.NewSealedSectorCID
				if newSectorInfo.SectorKeyCID == nil {
					newSectorInfo.SectorKeyCID = &updateWithDetails.sectorInfo.SealedCID
				}

				newSectorInfo.DealIDs = updateWithDetails.update.Deals
				newSectorInfo.Activation = rt.CurrEpoch()

				newSectorInfo.DealWeight = updateWithDetails.dealWeight.DealWeight
				newSectorInfo.VerifiedDealWeight = updateWithDetails.dealWeight.VerifiedDealWeight

				// compute initial pledge
				duration := updateWithDetails.sectorInfo.Expiration - rt.CurrEpoch()

				pwr := QAPowerForWeight(info.SectorSize, duration, newSectorInfo.DealWeight, newSectorInfo.VerifiedDealWeight)

				newSectorInfo.ReplacedDayReward = updateWithDetails.sectorInfo.ExpectedDayReward
				newSectorInfo.ExpectedDayReward = ExpectedRewardForPower(rewRet.ThisEpochRewardSmoothed, powRet.QualityAdjPowerSmoothed, pwr, builtin.EpochsInDay)
				newSectorInfo.ExpectedStoragePledge = ExpectedRewardForPower(rewRet.ThisEpochRewardSmoothed, powRet.QualityAdjPowerSmoothed, pwr, InitialPledgeProjectionPeriod)
				newSectorInfo.ReplacedSectorAge = maxEpoch(0, rt.CurrEpoch()-updateWithDetails.sectorInfo.Activation)

				initialPledgeAtUpgrade := InitialPledgeForPower(pwr, rewRet.ThisEpochBaselinePower, rewRet.ThisEpochRewardSmoothed,
					powRet.QualityAdjPowerSmoothed, rt.TotalFilCircSupply())

				if initialPledgeAtUpgrade.GreaterThan(updateWithDetails.sectorInfo.InitialPledge) {
					deficit := big.Sub(initialPledgeAtUpgrade, updateWithDetails.sectorInfo.InitialPledge)

					unlockedBalance, err := st.GetUnlockedBalance(rt.CurrentBalance())
					builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to calculate unlocked balance")
					builtin.RequirePredicate(rt, unlockedBalance.GreaterThanEqual(deficit), exitcode.ErrInsufficientFunds, "insufficient funds for new initial pledge requirement %s, available: %s, skipping sector %d",
						deficit, unlockedBalance, updateWithDetails.sectorInfo.SectorNumber)

					err = st.AddInitialPledge(deficit)
					builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add initial pledge")

					newSectorInfo.InitialPledge = initialPledgeAtUpgrade
				}

				var partition Partition
				found, err := partitions.Get(updateWithDetails.update.Partition, &partition)

				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load deadline %v partition %v",
					updateWithDetails.update.Deadline, updateWithDetails.update.Partition)

				if !found {
					rt.Abortf(exitcode.ErrNotFound, "no such deadline %v partition %v", dlIdx, updateWithDetails.update.Partition)
				}

				partitionPowerDelta, partitionPledgeDelta, err := partition.ReplaceSectors(store,
					[]*SectorOnChainInfo{updateWithDetails.sectorInfo},
					[]*SectorOnChainInfo{&newSectorInfo},
					info.SectorSize,
					quant)

				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to replace sector at deadline %d partition %d", updateWithDetails.update.Deadline, updateWithDetails.update.Partition)

				powerDelta = powerDelta.Add(partitionPowerDelta)
				pledgeDelta = big.Add(pledgeDelta, partitionPledgeDelta)

				err = partitions.Set(updateWithDetails.update.Partition, &partition)
				builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadline %v partition %v",
					updateWithDetails.update.Deadline,
					updateWithDetails.update.Partition)

				newSectors[i] = &newSectorInfo
				succeededSectors.Set(uint64(newSectorInfo.SectorNumber))
			}

			deadline.Partitions, err = partitions.Root()
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save partitions for deadline %d", dlIdx)

			err = deadlines.UpdateDeadline(store, dlIdx, deadline)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadline %d", dlIdx)
		}

		successCount, err := succeededSectors.Count()
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to count succeededSectors")
		builtin.RequirePredicate(rt, successCount == uint64(len(validatedUpdates)), exitcode.ErrIllegalState, "unexpected successcount %d != %d", successCount, len(validatedUpdates))

		// Overwrite sector infos.
		err = sectors.Store(newSectors...)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to update sector infos")

		st.Sectors, err = sectors.Root()
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save sectors")

		err = st.SaveDeadlines(store, deadlines)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to save deadlines")

	})

	notifyPledgeChanged(rt, pledgeDelta)
	requestUpdatePower(rt, powerDelta)

	return &succeededSectors
}

//////////
// Cron //
//////////

//type CronEventPayload struct {
//	EventType CronEventType
//}
type CronEventPayload = miner0.CronEventPayload

type CronEventType = miner0.CronEventType

const (
	CronEventProvingDeadline          = miner0.CronEventProvingDeadline
	CronEventProcessEarlyTerminations = miner0.CronEventProcessEarlyTerminations
)

func (a Actor) OnDeferredCronEvent(rt Runtime, params *builtin.DeferredCronEventParams) *abi.EmptyValue {
	rt.ValidateImmediateCallerIs(builtin.StoragePowerActorAddr)

	var payload miner0.CronEventPayload
	err := payload.UnmarshalCBOR(bytes.NewBuffer(params.EventPayload))
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to unmarshal miner cron payload into expected structure")

	switch payload.EventType {
	case CronEventProvingDeadline:
		handleProvingDeadline(rt, params.RewardSmoothed, params.QualityAdjPowerSmoothed)
	case CronEventProcessEarlyTerminations:
		if processEarlyTerminations(rt, params.RewardSmoothed, params.QualityAdjPowerSmoothed) {
			scheduleEarlyTerminationWork(rt)
		}
	default:
		rt.Log(rtt.ERROR, "onDeferredCronEvent invalid event type: %v", payload.EventType)
	}

	var st State
	rt.StateReadonly(&st)
	err = st.CheckBalanceInvariants(rt.CurrentBalance())
	builtin.RequireNoErr(rt, err, ErrBalanceInvariantBroken, "balance invariants broken")
	return nil
}

////////////////////////////////////////////////////////////////////////////////
// Utility functions & helpers
////////////////////////////////////////////////////////////////////////////////

// TODO: We're using the current power+epoch reward. Technically, we
// should use the power/reward at the time of termination.
// https://github.com/filecoin-project/specs-actors/v7/pull/648
func processEarlyTerminations(rt Runtime, rewardSmoothed smoothing.FilterEstimate, qualityAdjPowerSmoothed smoothing.FilterEstimate) (more bool) {
	store := adt.AsStore(rt)

	var (
		result           TerminationResult
		dealsToTerminate []market.OnMinerSectorsTerminateParams
		penalty          = big.Zero()
		pledgeDelta      = big.Zero()
	)

	var st State
	rt.StateTransaction(&st, func() {
		var err error
		result, more, err = st.PopEarlyTerminations(store, AddressedPartitionsMax, AddressedSectorsMax)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to pop early terminations")

		// Nothing to do, don't waste any time.
		// This can happen if we end up processing early terminations
		// before the cron callback fires.
		if result.IsEmpty() {
			rt.Log(rtt.INFO, "no early terminations (maybe cron callback hasn't happened yet?)")
			return
		}

		info := getMinerInfo(rt, &st)

		sectors, err := LoadSectors(store, st.Sectors)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sectors array")

		totalInitialPledge := big.Zero()
		dealsToTerminate = make([]market.OnMinerSectorsTerminateParams, 0, len(result.Sectors))
		err = result.ForEach(func(epoch abi.ChainEpoch, sectorNos bitfield.BitField) error {
			sectors, err := sectors.Load(sectorNos)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to load sector infos")
			params := market.OnMinerSectorsTerminateParams{
				Epoch:   epoch,
				DealIDs: make([]abi.DealID, 0, len(sectors)), // estimate ~one deal per sector.
			}
			for _, sector := range sectors {
				params.DealIDs = append(params.DealIDs, sector.DealIDs...)
				totalInitialPledge = big.Add(totalInitialPledge, sector.InitialPledge)
			}
			penalty = big.Add(penalty, terminationPenalty(info.SectorSize, epoch,
				rewardSmoothed, qualityAdjPowerSmoothed, sectors))
			dealsToTerminate = append(dealsToTerminate, params)

			return nil
		})
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to process terminations")

		// Pay penalty
		err = st.ApplyPenalty(penalty)
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")

		// Remove pledge requirement.
		err = st.AddInitialPledge(totalInitialPledge.Neg())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to add initial pledge %v", totalInitialPledge.Neg())
		pledgeDelta = big.Sub(pledgeDelta, totalInitialPledge)

		// Use unlocked pledge to pay down outstanding fee debt
		penaltyFromVesting, penaltyFromBalance, err := st.RepayPartialDebtInPriorityOrder(store, rt.CurrEpoch(), rt.CurrentBalance())
		builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to pay penalty")
		penalty = big.Add(penaltyFromVesting, penaltyFromBalance)
		pledgeDelta = big.Sub(pledgeDelta, penaltyFromVesting)
	})

	// We didn't do anything, abort.
	if result.IsEmpty() {
		rt.Log(rtt.INFO, "no early terminations")
		return more
	}

	// Burn penalty.
	rt.Log(rtt.DEBUG, "storage provider %s penalized %s for sector termination", rt.Receiver(), penalty)
	burnFunds(rt, penalty, BurnMethodProcessEarlyTerminations)

	// Return pledge.
	notifyPledgeChanged(rt, pledgeDelta)

	// Terminate deals.
	for _, params := range dealsToTerminate {
		requestTerminateDeals(rt, params.Epoch, params.DealIDs)
	}

	// reschedule cron worker, if necessary.
	return more
}

// Invoked at the end of the last epoch for each proving deadline.
func handleProvingDeadline(rt Runtime,
	rewardSmoothed smoothing.FilterEstimate,
	qualityAdjPowerSmoothed smoothing.FilterEstimate) {
	currEpoch := rt.CurrEpoch()
	store := adt.AsStore(rt)

	hadEarlyTerminations := false

	powerDeltaTotal := NewPowerPairZero()
	penaltyTotal := abi.NewTokenAmount(0)
	pledgeDeltaTotal := abi.NewTokenAmount(0)

	var continueCron bool
	var st State
	rt.StateTransaction(&st, func() {
		{
			// Vest locked funds.
			// This happens first so that any subsequent penalties are taken
			// from locked vesting funds before funds free this epoch.
			newlyVested, err := st.UnlockVestedFunds(store, rt.CurrEpoch())
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to vest funds")
			pledgeDeltaTotal = big.Add(pledgeDeltaTotal, newlyVested.Neg())
		}

		{
			// Process pending worker change if any
			info := getMinerInfo(rt, &st)
			processPendingWorker(info, rt, &st)
		}

		{
			depositToBurn, err := st.CleanUpExpiredPreCommits(store, currEpoch)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to expire pre-committed sectors")

			err = st.ApplyPenalty(depositToBurn)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")
			rt.Log(rtt.DEBUG, "storage provider %s penalized %s for expired pre commits", rt.Receiver(), depositToBurn)
		}

		// Record whether or not we _had_ early terminations in the queue before this method.
		// That way, don't re-schedule a cron callback if one is already scheduled.
		hadEarlyTerminations = havePendingEarlyTerminations(rt, &st)

		{
			result, err := st.AdvanceDeadline(store, currEpoch)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to advance deadline")

			// Faults detected by this missed PoSt pay no penalty, but sectors that were already faulty
			// and remain faulty through this deadline pay the fault fee.
			penaltyTarget := PledgePenaltyForContinuedFault(
				rewardSmoothed,
				qualityAdjPowerSmoothed,
				result.PreviouslyFaultyPower.QA,
			)

			powerDeltaTotal = powerDeltaTotal.Add(result.PowerDelta)
			pledgeDeltaTotal = big.Add(pledgeDeltaTotal, result.PledgeDelta)

			err = st.ApplyPenalty(penaltyTarget)
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to apply penalty")
			rt.Log(rtt.DEBUG, "storage provider %s penalized %s for continued fault", rt.Receiver(), penaltyTarget)

			penaltyFromVesting, penaltyFromBalance, err := st.RepayPartialDebtInPriorityOrder(store, currEpoch, rt.CurrentBalance())
			builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to unlock penalty")
			penaltyTotal = big.Add(penaltyFromVesting, penaltyFromBalance)
			pledgeDeltaTotal = big.Sub(pledgeDeltaTotal, penaltyFromVesting)
		}

		continueCron = st.ContinueDeadlineCron()
		if !continueCron {
			st.DeadlineCronActive = false
		}
	})
	// Remove power for new faults, and burn penalties.
	requestUpdatePower(rt, powerDeltaTotal)
	burnFunds(rt, penaltyTotal, BurnMethodHandleProvingDeadline)
	notifyPledgeChanged(rt, pledgeDeltaTotal)

	// Schedule cron callback for next deadline's last epoch.
	if continueCron {
		newDlInfo := st.DeadlineInfo(currEpoch + 1)
		enrollCronEvent(rt, newDlInfo.Last(), &CronEventPayload{
			EventType: CronEventProvingDeadline,
		})
	} else {
		rt.Log(rtt.INFO, "miner %s going inactive, deadline cron discontinued", rt.Receiver())
	}

	// Record whether or not we _have_ early terminations now.
	hasEarlyTerminations := havePendingEarlyTerminations(rt, &st)

	// If we didn't have pending early terminations before, but we do now,
	// handle them at the next epoch.
	if !hadEarlyTerminations && hasEarlyTerminations {
		// First, try to process some of these terminations.
		if processEarlyTerminations(rt, rewardSmoothed, qualityAdjPowerSmoothed) {
			// If that doesn't work, just defer till the next epoch.
			scheduleEarlyTerminationWork(rt)
		}
		// Note: _don't_ process early terminations if we had a cron
		// callback already scheduled. In that case, we'll already have
		// processed AddressedSectorsMax terminations this epoch.
	}
}

// Check expiry is exactly *the epoch before* the start of a proving period.
func validateExpiration(rt Runtime, activation, expiration abi.ChainEpoch, sealProof abi.RegisteredSealProof) {
	// Expiration must be after activation. Check this explicitly to avoid an underflow below.
	if expiration <= activation {
		rt.Abortf(exitcode.ErrIllegalArgument, "sector expiration %v must be after activation (%v)", expiration, activation)
	}
	// expiration cannot be less than minimum after activation
	if expiration-activation < MinSectorExpiration {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid expiration %d, total sector lifetime (%d) must exceed %d after activation %d",
			expiration, expiration-activation, MinSectorExpiration, activation)
	}

	// expiration cannot exceed MaxSectorExpirationExtension from now
	if expiration > rt.CurrEpoch()+MaxSectorExpirationExtension {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid expiration %d, cannot be more than %d past current epoch %d",
			expiration, MaxSectorExpirationExtension, rt.CurrEpoch())
	}

	// total sector lifetime cannot exceed SectorMaximumLifetime for the sector's seal proof
	maxLifetime, err := builtin.SealProofSectorMaximumLifetime(sealProof)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalArgument, "unrecognized seal proof type %d", sealProof)
	if expiration-activation > maxLifetime {
		rt.Abortf(exitcode.ErrIllegalArgument, "invalid expiration %d, total sector lifetime (%d) cannot exceed %d after activation %d",
			expiration, expiration-activation, maxLifetime, activation)
	}
}

func enrollCronEvent(rt Runtime, eventEpoch abi.ChainEpoch, callbackPayload *CronEventPayload) {
	payload := new(bytes.Buffer)
	err := callbackPayload.MarshalCBOR(payload)
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalArgument, "failed to serialize payload: %v", err)
	}
	code := rt.Send(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.EnrollCronEvent,
		&power.EnrollCronEventParams{
			EventEpoch: eventEpoch,
			Payload:    payload.Bytes(),
		},
		abi.NewTokenAmount(0),
		&builtin.Discard{},
	)
	builtin.RequireSuccess(rt, code, "failed to enroll cron event")
}

func requestUpdatePower(rt Runtime, delta PowerPair) {
	if delta.IsZero() {
		return
	}
	code := rt.Send(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.UpdateClaimedPower,
		&power.UpdateClaimedPowerParams{
			RawByteDelta:         delta.Raw,
			QualityAdjustedDelta: delta.QA,
		},
		abi.NewTokenAmount(0),
		&builtin.Discard{},
	)
	builtin.RequireSuccess(rt, code, "failed to update power with %v", delta)
}

func requestTerminateDeals(rt Runtime, epoch abi.ChainEpoch, dealIDs []abi.DealID) {
	for len(dealIDs) > 0 {
		size := min64(cbg.MaxLength, uint64(len(dealIDs)))
		code := rt.Send(
			builtin.StorageMarketActorAddr,
			builtin.MethodsMarket.OnMinerSectorsTerminate,
			&market.OnMinerSectorsTerminateParams{
				Epoch:   epoch,
				DealIDs: dealIDs[:size],
			},
			abi.NewTokenAmount(0),
			&builtin.Discard{},
		)
		builtin.RequireSuccess(rt, code, "failed to terminate deals, exit code %v", code)
		dealIDs = dealIDs[size:]
	}
}

func scheduleEarlyTerminationWork(rt Runtime) {
	rt.Log(rtt.INFO, "scheduling early terminations with cron...")

	enrollCronEvent(rt, rt.CurrEpoch()+1, &CronEventPayload{
		EventType: CronEventProcessEarlyTerminations,
	})
}

func havePendingEarlyTerminations(rt Runtime, st *State) bool {
	// Record this up-front
	noEarlyTerminations, err := st.EarlyTerminations.IsEmpty()
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "failed to count early terminations")
	return !noEarlyTerminations
}

func verifyWindowedPost(rt Runtime, challengeEpoch abi.ChainEpoch, sectors []*SectorOnChainInfo, proofs []proof.PoStProof) error {
	minerActorID, err := addr.IDFromAddress(rt.Receiver())
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "runtime provided bad receiver address %v", rt.Receiver())

	// Regenerate challenge randomness, which must match that generated for the proof.
	var addrBuf bytes.Buffer
	receiver := rt.Receiver()
	err = receiver.MarshalCBOR(&addrBuf)
	builtin.RequireNoErr(rt, err, exitcode.ErrSerialization, "failed to marshal address for window post challenge")
	postRandomness := rt.GetRandomnessFromBeacon(crypto.DomainSeparationTag_WindowedPoStChallengeSeed, challengeEpoch, addrBuf.Bytes())

	sectorProofInfo := make([]proof.SectorInfo, len(sectors))
	for i, s := range sectors {
		sectorProofInfo[i] = proof.SectorInfo{
			SealProof:    s.SealProof,
			SectorNumber: s.SectorNumber,
			SealedCID:    s.SealedCID,
		}
	}

	// Get public inputs
	pvInfo := proof.WindowPoStVerifyInfo{
		Randomness:        abi.PoStRandomness(postRandomness),
		Proofs:            proofs,
		ChallengedSectors: sectorProofInfo,
		Prover:            abi.ActorID(minerActorID),
	}

	// Verify the PoSt ReplicaProof
	err = rt.VerifyPoSt(pvInfo)
	if err != nil {
		return fmt.Errorf("invalid PoSt %+v: %w", pvInfo, err)
	}
	return nil
}

// SealVerifyParams is the structure of information that must be sent with a
// message to commit a sector. Most of this information is not needed in the
// state tree but will be verified in sm.CommitSector. See SealCommitment for
// data stored on the state tree for each sector.
type SealVerifyStuff struct {
	SealedCID        cid.Cid        // CommR
	InteractiveEpoch abi.ChainEpoch // Used to derive the interactive PoRep challenge.
	abi.RegisteredSealProof
	Proof   []byte
	DealIDs []abi.DealID
	abi.SectorNumber
	SealRandEpoch abi.ChainEpoch // Used to tie the seal to a chain.
}

func getVerifyInfo(rt Runtime, params *SealVerifyStuff) *proof.SealVerifyInfo {
	if rt.CurrEpoch() <= params.InteractiveEpoch {
		rt.Abortf(exitcode.ErrForbidden, "too early to prove sector")
	}

	commDs := requestUnsealedSectorCIDs(rt, &market.SectorDataSpec{
		SectorType: params.RegisteredSealProof,
		DealIDs:    params.DealIDs,
	})

	minerActorID, err := addr.IDFromAddress(rt.Receiver())
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "runtime provided non-ID receiver address %v", rt.Receiver())

	buf := new(bytes.Buffer)
	receiver := rt.Receiver()
	err = receiver.MarshalCBOR(buf)
	builtin.RequireNoErr(rt, err, exitcode.ErrSerialization, "failed to marshal address for seal verification challenge")

	svInfoRandomness := rt.GetRandomnessFromTickets(crypto.DomainSeparationTag_SealRandomness, params.SealRandEpoch, buf.Bytes())
	svInfoInteractiveRandomness := rt.GetRandomnessFromBeacon(crypto.DomainSeparationTag_InteractiveSealChallengeSeed, params.InteractiveEpoch, buf.Bytes())

	return &proof.SealVerifyInfo{
		SealProof: params.RegisteredSealProof,
		SectorID: abi.SectorID{
			Miner:  abi.ActorID(minerActorID),
			Number: params.SectorNumber,
		},
		DealIDs:               params.DealIDs,
		InteractiveRandomness: abi.InteractiveSealRandomness(svInfoInteractiveRandomness),
		Proof:                 params.Proof,
		Randomness:            abi.SealRandomness(svInfoRandomness),
		SealedCID:             params.SealedCID,
		UnsealedCID:           commDs[0],
	}
}

// Requests the storage market actor compute the unsealed sector CID from a sector's deals.
func requestUnsealedSectorCIDs(rt Runtime, dataCommitmentInputs ...*market.SectorDataSpec) []cid.Cid {
	if len(dataCommitmentInputs) == 0 {
		return nil
	}
	var ret market.ComputeDataCommitmentReturn
	code := rt.Send(
		builtin.StorageMarketActorAddr,
		builtin.MethodsMarket.ComputeDataCommitment,
		&market.ComputeDataCommitmentParams{
			Inputs: dataCommitmentInputs,
		},
		abi.NewTokenAmount(0),
		&ret,
	)
	builtin.RequireSuccess(rt, code, "failed request for unsealed sector CIDs")
	builtin.RequireState(rt, len(dataCommitmentInputs) == len(ret.CommDs), "number of data commitments computed %d does not match number of data commitment inputs %d", len(ret.CommDs), len(dataCommitmentInputs))
	unsealedCIDs := make([]cid.Cid, len(ret.CommDs))
	for i, cbgCid := range ret.CommDs {
		unsealedCIDs[i] = cid.Cid(cbgCid)
	}
	return unsealedCIDs
}

func requestDealWeights(rt Runtime, sectors []market.SectorDeals) *market.VerifyDealsForActivationReturn {
	// Short-circuit if there are no deals in any of the sectors.
	dealCount := 0
	for _, sector := range sectors {
		dealCount += len(sector.DealIDs)
	}
	if dealCount == 0 {
		emptyResult := &market.VerifyDealsForActivationReturn{
			Sectors: make([]market.SectorWeights, len(sectors)),
		}
		for i := 0; i < len(sectors); i++ {
			emptyResult.Sectors[i] = market.SectorWeights{
				DealSpace:          0,
				DealWeight:         big.Zero(),
				VerifiedDealWeight: big.Zero(),
			}
		}
		return emptyResult
	}

	var dealWeights market.VerifyDealsForActivationReturn
	code := rt.Send(
		builtin.StorageMarketActorAddr,
		builtin.MethodsMarket.VerifyDealsForActivation,
		&market.VerifyDealsForActivationParams{
			Sectors: sectors,
		},
		abi.NewTokenAmount(0),
		&dealWeights,
	)
	builtin.RequireSuccess(rt, code, "failed to verify deals and get deal weight")
	return &dealWeights
}

// Requests the current epoch target block reward from the reward actor.
// return value includes reward, smoothed estimate of reward, and baseline power
func requestCurrentEpochBlockReward(rt Runtime) reward.ThisEpochRewardReturn {
	var ret reward.ThisEpochRewardReturn
	code := rt.Send(builtin.RewardActorAddr, builtin.MethodsReward.ThisEpochReward, nil, big.Zero(), &ret)
	builtin.RequireSuccess(rt, code, "failed to check epoch baseline power")
	return ret
}

// Requests the current network total power and pledge from the power actor.
func requestCurrentTotalPower(rt Runtime) *power.CurrentTotalPowerReturn {
	var pwr power.CurrentTotalPowerReturn
	code := rt.Send(builtin.StoragePowerActorAddr, builtin.MethodsPower.CurrentTotalPower, nil, big.Zero(), &pwr)
	builtin.RequireSuccess(rt, code, "failed to check current power")
	return &pwr
}

// Resolves an address to an ID address and verifies that it is address of an account or multisig actor.
func resolveControlAddress(rt Runtime, raw addr.Address) addr.Address {
	resolved, ok := rt.ResolveAddress(raw)
	if !ok {
		rt.Abortf(exitcode.ErrIllegalArgument, "unable to resolve address %v", raw)
	}
	ownerCode, ok := rt.GetActorCodeCID(resolved)
	if !ok {
		rt.Abortf(exitcode.ErrIllegalArgument, "no code for address %v", resolved)
	}
	if !builtin.IsPrincipal(ownerCode) {
		rt.Abortf(exitcode.ErrIllegalArgument, "owner actor type must be a principal, was %v", ownerCode)
	}
	return resolved
}

// Resolves an address to an ID address and verifies that it is address of an account actor with an associated BLS key.
// The worker must be BLS since the worker key will be used alongside a BLS-VRF.
func resolveWorkerAddress(rt Runtime, raw addr.Address) addr.Address {
	resolved, ok := rt.ResolveAddress(raw)
	if !ok {
		rt.Abortf(exitcode.ErrIllegalArgument, "unable to resolve address %v", raw)
	}
	workerCode, ok := rt.GetActorCodeCID(resolved)
	if !ok {
		rt.Abortf(exitcode.ErrIllegalArgument, "no code for address %v", resolved)
	}
	if workerCode != builtin.AccountActorCodeID {
		rt.Abortf(exitcode.ErrIllegalArgument, "worker actor type must be an account, was %v", workerCode)
	}

	if raw.Protocol() != addr.BLS {
		var pubkey addr.Address
		code := rt.Send(resolved, builtin.MethodsAccount.PubkeyAddress, nil, big.Zero(), &pubkey)
		builtin.RequireSuccess(rt, code, "failed to fetch account pubkey from %v", resolved)
		if pubkey.Protocol() != addr.BLS {
			rt.Abortf(exitcode.ErrIllegalArgument, "worker account %v must have BLS pubkey, was %v", resolved, pubkey.Protocol())
		}
	}
	return resolved
}

func burnFunds(rt Runtime, amt abi.TokenAmount, bt BurnMethod) {
	if amt.GreaterThan(big.Zero()) {
		rt.Log(rtt.DEBUG, "storage provder %s burn type %s burning %s", rt.Receiver(), bt, amt)
		code := rt.Send(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, amt, &builtin.Discard{})
		builtin.RequireSuccess(rt, code, "failed to burn funds")
	}
}

func notifyPledgeChanged(rt Runtime, pledgeDelta abi.TokenAmount) {
	if !pledgeDelta.IsZero() {
		code := rt.Send(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdatePledgeTotal, &pledgeDelta, big.Zero(), &builtin.Discard{})
		builtin.RequireSuccess(rt, code, "failed to update total pledge")
	}
}

// Assigns proving period offset randomly in the range [0, WPoStProvingPeriod) by hashing
// the actor's address and current epoch.
func assignProvingPeriodOffset(myAddr addr.Address, currEpoch abi.ChainEpoch, hash func(data []byte) [32]byte) (abi.ChainEpoch, error) {
	offsetSeed := bytes.Buffer{}
	err := myAddr.MarshalCBOR(&offsetSeed)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize address: %w", err)
	}

	err = binary.Write(&offsetSeed, binary.BigEndian, currEpoch)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize epoch: %w", err)
	}

	digest := hash(offsetSeed.Bytes())
	var offset uint64
	err = binary.Read(bytes.NewBuffer(digest[:]), binary.BigEndian, &offset)
	if err != nil {
		return 0, fmt.Errorf("failed to interpret digest: %w", err)
	}

	offset = offset % uint64(WPoStProvingPeriod)
	return abi.ChainEpoch(offset), nil
}

// Computes the epoch at which a proving period should start such that it is greater than the current epoch, and
// has a defined offset from being an exact multiple of WPoStProvingPeriod.
// A miner is exempt from Winow PoSt until the first full proving period starts.
func currentProvingPeriodStart(currEpoch abi.ChainEpoch, offset abi.ChainEpoch) abi.ChainEpoch {
	currModulus := currEpoch % WPoStProvingPeriod
	var periodProgress abi.ChainEpoch // How far ahead is currEpoch from previous offset boundary.
	if currModulus >= offset {
		periodProgress = currModulus - offset
	} else {
		periodProgress = WPoStProvingPeriod - (offset - currModulus)
	}

	periodStart := currEpoch - periodProgress
	return periodStart
}

// Computes the deadline index for the current epoch for a given period start.
// currEpoch must be within the proving period that starts at provingPeriodStart to produce a valid index.
func currentDeadlineIndex(currEpoch abi.ChainEpoch, periodStart abi.ChainEpoch) uint64 {
	return uint64((currEpoch - periodStart) / WPoStChallengeWindow)
}

// Update worker address with pending worker key if exists and delay has passed
func processPendingWorker(info *MinerInfo, rt Runtime, st *State) {
	if info.PendingWorkerKey == nil || rt.CurrEpoch() < info.PendingWorkerKey.EffectiveAt {
		return
	}

	info.Worker = info.PendingWorkerKey.NewWorker
	info.PendingWorkerKey = nil

	err := st.SaveInfo(adt.AsStore(rt), info)
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not save miner info")
}

// Computes deadline information for a fault or recovery declaration.
// If the deadline has not yet elapsed, the declaration is taken as being for the current proving period.
// If the deadline has elapsed, it's instead taken as being for the next proving period after the current epoch.
func declarationDeadlineInfo(periodStart abi.ChainEpoch, deadlineIdx uint64, currEpoch abi.ChainEpoch) (*dline.Info, error) {
	if deadlineIdx >= WPoStPeriodDeadlines {
		return nil, fmt.Errorf("invalid deadline %d, must be < %d", deadlineIdx, WPoStPeriodDeadlines)
	}

	deadline := NewDeadlineInfo(periodStart, deadlineIdx, currEpoch).NextNotElapsed()
	return deadline, nil
}

// Checks that a fault or recovery declaration at a specific deadline is outside the exclusion window for the deadline.
func validateFRDeclarationDeadline(deadline *dline.Info) error {
	if deadline.FaultCutoffPassed() {
		return fmt.Errorf("late fault or recovery declaration at %v", deadline)
	}
	return nil
}

// Validates that a partition contains the given sectors.
func validatePartitionContainsSectors(partition *Partition, sectors bitfield.BitField) error {
	// Check that the declared sectors are actually assigned to the partition.
	contains, err := BitFieldContainsAll(partition.Sectors, sectors)
	if err != nil {
		return xerrors.Errorf("failed to check sectors: %w", err)
	}
	if !contains {
		return xerrors.Errorf("not all sectors are assigned to the partition")
	}
	return nil
}

func terminationPenalty(sectorSize abi.SectorSize, currEpoch abi.ChainEpoch,
	rewardEstimate, networkQAPowerEstimate smoothing.FilterEstimate, sectors []*SectorOnChainInfo) abi.TokenAmount {
	totalFee := big.Zero()
	for _, s := range sectors {
		sectorPower := QAPowerForSector(sectorSize, s)
		fee := PledgePenaltyForTermination(s.ExpectedDayReward, currEpoch-s.Activation, s.ExpectedStoragePledge,
			networkQAPowerEstimate, sectorPower, rewardEstimate, s.ReplacedDayReward, s.ReplacedSectorAge)
		totalFee = big.Add(fee, totalFee)
	}
	return totalFee
}

func PowerForSector(sectorSize abi.SectorSize, sector *SectorOnChainInfo) PowerPair {
	return PowerPair{
		Raw: big.NewIntUnsigned(uint64(sectorSize)),
		QA:  QAPowerForSector(sectorSize, sector),
	}
}

// Returns the sum of the raw byte and quality-adjusted power for sectors.
func PowerForSectors(ssize abi.SectorSize, sectors []*SectorOnChainInfo) PowerPair {
	qa := big.Zero()
	for _, s := range sectors {
		qa = big.Add(qa, QAPowerForSector(ssize, s))
	}

	return PowerPair{
		Raw: big.Mul(big.NewIntUnsigned(uint64(ssize)), big.NewIntUnsigned(uint64(len(sectors)))),
		QA:  qa,
	}
}

func ConsensusFaultActive(info *MinerInfo, currEpoch abi.ChainEpoch) bool {
	// For penalization period to last for exactly finality epochs
	// consensus faults are active until currEpoch exceeds ConsensusFaultElapsed
	return currEpoch <= info.ConsensusFaultElapsed
}

func getMinerInfo(rt Runtime, st *State) *MinerInfo {
	info, err := st.GetInfo(adt.AsStore(rt))
	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not read miner info")
	return info
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minEpoch(a, b abi.ChainEpoch) abi.ChainEpoch {
	if a < b {
		return a
	}
	return b
}

func maxEpoch(a, b abi.ChainEpoch) abi.ChainEpoch { //nolint:deadcode,unused
	if a > b {
		return a
	}
	return b
}

func checkControlAddresses(rt Runtime, controlAddrs []addr.Address) {
	if len(controlAddrs) > MaxControlAddresses {
		rt.Abortf(exitcode.ErrIllegalArgument, "control addresses length %d exceeds max control addresses length %d", len(controlAddrs), MaxControlAddresses)
	}
}

func checkPeerInfo(rt Runtime, peerID abi.PeerID, multiaddrs []abi.Multiaddrs) {
	if len(peerID) > MaxPeerIDLength {
		rt.Abortf(exitcode.ErrIllegalArgument, "peer ID size of %d exceeds maximum size of %d", peerID, MaxPeerIDLength)
	}

	totalSize := 0
	for _, ma := range multiaddrs {
		if len(ma) == 0 {
			rt.Abortf(exitcode.ErrIllegalArgument, "invalid empty multiaddr")
		}
		totalSize += len(ma)
	}
	if totalSize > MaxMultiaddrData {
		rt.Abortf(exitcode.ErrIllegalArgument, "multiaddr size of %d exceeds maximum of %d", totalSize, MaxMultiaddrData)
	}
}
