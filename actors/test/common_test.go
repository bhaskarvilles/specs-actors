package test

import (
	"bytes"
	"fmt"
	"testing"

	miner0 "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/v8/actors/builtin/miner"

	"github.com/filecoin-project/go-address"
	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/filecoin-project/specs-actors/v8/actors/builtin/power"
	"github.com/filecoin-project/specs-actors/v8/actors/states"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/specs-actors/v8/actors/builtin"
	"github.com/filecoin-project/specs-actors/v8/actors/builtin/market"
	tutil "github.com/filecoin-project/specs-actors/v8/support/testing"
	"github.com/filecoin-project/specs-actors/v8/support/vm"
)

func preCommitSectors(t *testing.T, v *vm.VM, count, batchSize int, worker, mAddr address.Address, sealProof abi.RegisteredSealProof, sectorNumberBase abi.SectorNumber, expectCronEnrollment bool, expiration abi.ChainEpoch) []*miner.SectorPreCommitOnChainInfo {
	invocsCommon := []vm.ExpectInvocation{
		{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward},
		{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower},
	}
	invocFirst := vm.ExpectInvocation{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.EnrollCronEvent}

	sectorIndex := 0
	for sectorIndex < count {
		msgSectorIndexStart := sectorIndex
		invocs := invocsCommon

		// Prepare message.
		params := miner.PreCommitSectorBatchParams{Sectors: make([]miner0.SectorPreCommitInfo, batchSize)}
		if expiration < 0 {
			expiration = v.GetEpoch() + miner.MinSectorExpiration + miner.MaxProveCommitDuration[sealProof] + 100
		}

		for j := 0; j < batchSize && sectorIndex < count; j++ {
			sectorNumber := sectorNumberBase + abi.SectorNumber(sectorIndex)
			sealedCid := tutil.MakeCID(fmt.Sprintf("%d", sectorNumber), &miner.SealedCIDPrefix)
			params.Sectors[j] = miner0.SectorPreCommitInfo{
				SealProof:     sealProof,
				SectorNumber:  sectorNumber,
				SealedCID:     sealedCid,
				SealRandEpoch: v.GetEpoch() - 1,
				DealIDs:       nil,
				Expiration:    expiration,
			}
			sectorIndex++
		}
		if sectorIndex == count && sectorIndex%batchSize != 0 {
			// Trim the last, partial batch.
			params.Sectors = params.Sectors[:sectorIndex%batchSize]
		}

		// Finalize invocation expectation list
		if len(params.Sectors) > 1 {
			aggFee := miner.AggregatePreCommitNetworkFee(len(params.Sectors), big.Zero())
			invocs = append(invocs, vm.ExpectInvocation{To: builtin.BurntFundsActorAddr, Method: builtin.MethodSend, Value: &aggFee})
		}
		if expectCronEnrollment && msgSectorIndexStart == 0 {
			invocs = append(invocs, invocFirst)
		}
		vm.ApplyOk(t, v, worker, mAddr, big.Zero(), builtin.MethodsMiner.PreCommitSectorBatch, &params)
		vm.ExpectInvocation{
			To:             mAddr,
			Method:         builtin.MethodsMiner.PreCommitSectorBatch,
			Params:         vm.ExpectObject(&params),
			SubInvocations: invocs,
		}.Matches(t, v.LastInvocation())
	}

	// Extract chain state.
	var minerState miner.State
	err := v.GetState(mAddr, &minerState)
	require.NoError(t, err)

	precommits := make([]*miner.SectorPreCommitOnChainInfo, count)
	for i := 0; i < count; i++ {
		precommit, found, err := minerState.GetPrecommittedSector(v.Store(), sectorNumberBase+abi.SectorNumber(i))
		require.NoError(t, err)
		require.True(t, found)
		precommits[i] = precommit
	}
	return precommits
}

func createMiner(t *testing.T, v *vm.VM, owner, worker addr.Address, wPoStProof abi.RegisteredPoStProof, balance abi.TokenAmount) *power.CreateMinerReturn {
	params := power.CreateMinerParams{
		Owner:               owner,
		Worker:              worker,
		WindowPoStProofType: wPoStProof,
		Peer:                abi.PeerID("not really a peer id"),
	}
	ret := vm.ApplyOk(t, v, worker, builtin.StoragePowerActorAddr, balance, builtin.MethodsPower.CreateMiner, &params)
	minerAddrs, ok := ret.(*power.CreateMinerReturn)
	require.True(t, ok)
	return minerAddrs
}

func publishDeal(t *testing.T, v *vm.VM, provider, dealClient, minerID addr.Address, dealLabel string,
	pieceSize abi.PaddedPieceSize, verifiedDeal bool, dealStart abi.ChainEpoch, dealLifetime abi.ChainEpoch,
) *market.PublishStorageDealsReturn {
	deal := market.DealProposal{
		PieceCID:             tutil.MakeCID(dealLabel, &market.PieceCIDPrefix),
		PieceSize:            pieceSize,
		VerifiedDeal:         verifiedDeal,
		Client:               dealClient,
		Provider:             minerID,
		Label:                dealLabel,
		StartEpoch:           dealStart,
		EndEpoch:             dealStart + dealLifetime,
		StoragePricePerEpoch: abi.NewTokenAmount(1 << 20),
		ProviderCollateral:   big.Mul(big.NewInt(2), vm.FIL),
		ClientCollateral:     big.Mul(big.NewInt(1), vm.FIL),
	}

	paramBuf := new(bytes.Buffer)
	err := deal.MarshalCBOR(paramBuf)
	require.NoError(t, err)

	publishDealParams := market.PublishStorageDealsParams{
		Deals: []market.ClientDealProposal{{
			Proposal: deal,
			ClientSignature: crypto.Signature{
				Type: crypto.SigTypeBLS,
				Data: paramBuf.Bytes(),
			},
		}},
	}
	result := vm.RequireApplyMessage(t, v, provider, builtin.StorageMarketActorAddr, big.Zero(), builtin.MethodsMarket.PublishStorageDeals, &publishDealParams, t.Name())
	require.Equal(t, exitcode.Ok, result.Code)

	expectedPublishSubinvocations := []vm.ExpectInvocation{
		{To: minerID, Method: builtin.MethodsMiner.ControlAddresses, SubInvocations: []vm.ExpectInvocation{}},
		{To: builtin.RewardActorAddr, Method: builtin.MethodsReward.ThisEpochReward, SubInvocations: []vm.ExpectInvocation{}},
		{To: builtin.StoragePowerActorAddr, Method: builtin.MethodsPower.CurrentTotalPower, SubInvocations: []vm.ExpectInvocation{}},
	}

	if verifiedDeal {
		expectedPublishSubinvocations = append(expectedPublishSubinvocations, vm.ExpectInvocation{
			To:             builtin.VerifiedRegistryActorAddr,
			Method:         builtin.MethodsVerifiedRegistry.UseBytes,
			SubInvocations: []vm.ExpectInvocation{},
		})
	}

	vm.ExpectInvocation{
		To:             builtin.StorageMarketActorAddr,
		Method:         builtin.MethodsMarket.PublishStorageDeals,
		SubInvocations: expectedPublishSubinvocations,
	}.Matches(t, v.LastInvocation())

	return result.Ret.(*market.PublishStorageDealsReturn)
}

type dealBatcher struct {
	deals []market.DealProposal
	v     *vm.VM
}

func newDealBatcher(v *vm.VM) *dealBatcher {
	return &dealBatcher{
		deals: make([]market.DealProposal, 0),
		v:     v,
	}
}

func (db *dealBatcher) stage(t *testing.T, dealClient, dealProvider addr.Address, dealLabel string, pieceSize abi.PaddedPieceSize, verifiedDeal bool, dealStart,
	dealLifetime abi.ChainEpoch, pricePerEpoch, providerCollateral, clientCollateral abi.TokenAmount) {
	deal := market.DealProposal{
		PieceCID:             tutil.MakeCID(dealLabel, &market.PieceCIDPrefix),
		PieceSize:            pieceSize,
		VerifiedDeal:         verifiedDeal,
		Client:               dealClient,
		Provider:             dealProvider,
		Label:                dealLabel,
		StartEpoch:           dealStart,
		EndEpoch:             dealStart + dealLifetime,
		StoragePricePerEpoch: pricePerEpoch,
		ProviderCollateral:   providerCollateral,
		ClientCollateral:     clientCollateral,
	}

	db.deals = append(db.deals, deal)
}

func (db *dealBatcher) publishOK(t *testing.T, sender addr.Address) *market.PublishStorageDealsReturn {
	publishDealParams := market.PublishStorageDealsParams{}
	for _, deal := range db.deals {
		paramBuf := new(bytes.Buffer)
		err := deal.MarshalCBOR(paramBuf)
		require.NoError(t, err)

		publishDealParams.Deals = append(publishDealParams.Deals, market.ClientDealProposal{
			Proposal: deal,
			ClientSignature: crypto.Signature{
				Type: crypto.SigTypeBLS,
				Data: paramBuf.Bytes(),
			},
		})
	}

	result := vm.RequireApplyMessage(t, db.v, sender, builtin.StorageMarketActorAddr, big.Zero(), builtin.MethodsMarket.PublishStorageDeals, &publishDealParams, t.Name())
	require.Equal(t, exitcode.Ok, result.Code)

	return result.Ret.(*market.PublishStorageDealsReturn)
}

func (db *dealBatcher) publishFail(t *testing.T, sender addr.Address) {
	publishDealParams := market.PublishStorageDealsParams{}
	for _, deal := range db.deals {
		publishDealParams.Deals = append(publishDealParams.Deals, market.ClientDealProposal{
			Proposal: deal,
			ClientSignature: crypto.Signature{
				Type: crypto.SigTypeBLS,
			},
		})
	}

	result := vm.RequireApplyMessage(t, db.v, sender, builtin.StorageMarketActorAddr, big.Zero(), builtin.MethodsMarket.PublishStorageDeals, &publishDealParams, t.Name())
	require.Equal(t, exitcode.ErrIllegalArgument, result.Code) // because we can't return multiple codes for batch failures we return 16 in all cases
}

func requireActor(t *testing.T, v *vm.VM, addr address.Address) *states.Actor {
	a, found, err := v.GetActor(addr)
	require.NoError(t, err)
	require.True(t, found)
	return a
}
