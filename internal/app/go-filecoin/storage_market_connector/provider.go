package storagemarketconnector

import (
	"context"
	"io"

	"github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/internal/pkg/wallet"

	"github.com/filecoin-project/go-filecoin/internal/pkg/piecemanager"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-fil-markets/shared/tokenamount"
	t2 "github.com/filecoin-project/go-fil-markets/shared/types"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	spaabi "github.com/filecoin-project/specs-actors/actors/abi"
	spasm "github.com/filecoin-project/specs-actors/actors/builtin/storage_market"
	spaminer "github.com/filecoin-project/specs-actors/actors/builtin/storage_miner"
	spautil "github.com/filecoin-project/specs-actors/actors/util"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-filecoin/internal/app/go-filecoin/plumbing/msg"
	"github.com/filecoin-project/go-filecoin/internal/pkg/block"
	"github.com/filecoin-project/go-filecoin/internal/pkg/message"
	"github.com/filecoin-project/go-filecoin/internal/pkg/types"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/abi"
	fcsm "github.com/filecoin-project/go-filecoin/internal/pkg/vm/actor/builtin/storagemarket"
	fcaddr "github.com/filecoin-project/go-filecoin/internal/pkg/vm/address"
)

type WorkerGetter func(ctx context.Context, minerAddr fcaddr.Address, baseKey block.TipSetKey) (fcaddr.Address, error)

type chainReader interface {
	Head() block.TipSetKey
	GetTipSet(block.TipSetKey) (block.TipSet, error)
	GetActorStateAt(ctx context.Context, tipKey block.TipSetKey, addr fcaddr.Address, out interface{}) error
}

type StorageProviderNodeConnector struct {
	minerAddr    address.Address
	chainStore   chainReader
	outbox       *message.Outbox
	waiter       *msg.Waiter
	pieceManager piecemanager.PieceManager
	workerGetter WorkerGetter
	wallet       *wallet.Wallet
}

func NewStorageProviderNodeConnector(ma address.Address, cs chainReader, ob *message.Outbox, w *msg.Waiter, pm piecemanager.PieceManager, wg WorkerGetter, wlt *wallet.Wallet) *StorageProviderNodeConnector {
	return &StorageProviderNodeConnector{
		minerAddr:    ma,
		chainStore:   cs,
		outbox:       ob,
		waiter:       w,
		pieceManager: pm,
		workerGetter: wg,
		wallet:       wlt,
	}
}

func (s *StorageProviderNodeConnector) MostRecentStateId(ctx context.Context) (storagemarket.StateKey, error) {
	key := s.chainStore.Head()
	ts, err := s.chainStore.GetTipSet(key)

	if err != nil {
		return nil, err
	}

	return &stateKey{key, uint64(ts.At(0).Height)}, nil
}

func (s *StorageProviderNodeConnector) AddFunds(ctx context.Context, addr address.Address, amount tokenamount.TokenAmount) error {
	params, err := abi.ToEncodedValues(addr)
	if err != nil {
		return err
	}

	workerAddr, err := s.getFCWorker(ctx)
	if err != nil {
		return err
	}

	mcid, cerr, err := s.outbox.Send(
		ctx,
		workerAddr,
		fcaddr.StorageMarketAddress,
		types.NewAttoFIL(amount.Int),
		types.NewGasPrice(1),
		types.NewGasUnits(300),
		true,
		fcsm.AddBalance,
		params,
	)
	if err != nil {
		return err
	}

	_, err = s.wait(ctx, mcid, cerr)

	return err
}

func (s *StorageProviderNodeConnector) EnsureFunds(ctx context.Context, addr address.Address, amount tokenamount.TokenAmount) error {
	var smState spasm.StorageMarketActorState
	err := s.chainStore.GetActorStateAt(ctx, s.chainStore.Head(), fcaddr.StorageMarketAddress, &smState)
	if err != nil {
		return err
	}

	return nil
}

func (s *StorageProviderNodeConnector) GetBalance(ctx context.Context, addr address.Address) (storagemarket.Balance, error) {
	var smState spasm.StorageMarketActorState
	err := s.chainStore.GetActorStateAt(ctx, s.chainStore.Head(), fcaddr.StorageMarketAddress, &smState)
	if err != nil {
		return storagemarket.Balance{}, err
	}

	// Dragons: Balance or similar should be an exported method on StorageMarketState. Do it ourselves for now.
	available, ok := spautil.BalanceTable_GetEntry(smState.EscrowTable, addr)
	if !ok {
		available = spaabi.NewTokenAmount(0)
	}

	locked, ok := spautil.BalanceTable_GetEntry(smState.LockedReqTable, addr)
	if !ok {
		locked = spaabi.NewTokenAmount(0)
	}

	return storagemarket.Balance{
		Available: tokenamount.FromInt(available.Int.Uint64()),
		Locked:    tokenamount.FromInt(locked.Int.Uint64()),
	}, nil
}

func (s *StorageProviderNodeConnector) PublishDeals(ctx context.Context, deal storagemarket.MinerDeal) (storagemarket.DealID, cid.Cid, error) {
	client, err := fcaddr.NewFromBytes(deal.Proposal.Client.Bytes())
	if err != nil {
		return 0, cid.Undef, err
	}

	provider, err := fcaddr.NewFromBytes(deal.Proposal.Provider.Bytes())
	if err != nil {
		return 0, cid.Undef, err
	}

	sig := types.Signature(deal.Proposal.ProposerSignature.Data)

	fcStorageProposal := types.StorageDealProposal{
		PieceRef:  deal.Proposal.PieceRef,
		PieceSize: types.Uint64(deal.Proposal.PieceSize),

		Client:   client,
		Provider: provider,

		ProposalExpiration: types.Uint64(deal.Proposal.ProposalExpiration),
		Duration:           types.Uint64(deal.Proposal.Duration),

		StoragePricePerEpoch: types.Uint64(deal.Proposal.StoragePricePerEpoch.Uint64()),
		StorageCollateral:    types.Uint64(deal.Proposal.StorageCollateral.Uint64()),

		ProposerSignature: &sig,
	}
	params, err := abi.ToEncodedValues([]types.StorageDealProposal{fcStorageProposal})
	if err != nil {
		return 0, cid.Undef, err
	}

	workerAddr, err := s.getFCWorker(ctx)
	if err != nil {
		return 0, cid.Undef, err
	}

	mcid, cerr, err := s.outbox.Send(
		ctx,
		workerAddr,
		fcaddr.StorageMarketAddress,
		types.ZeroAttoFIL,
		types.NewGasPrice(1),
		types.NewGasUnits(300),
		true,
		fcsm.PublishStorageDeals,
		params,
	)
	if err != nil {
		return 0, cid.Undef, err
	}

	receipt, err := s.wait(ctx, mcid, cerr)

	dealIDValues, err := abi.Deserialize(receipt.Return[0], abi.UintArray)
	if err != nil {
		return 0, cid.Undef, err
	}

	dealIds, ok := dealIDValues.Val.([]uint64)
	if !ok {
		return 0, cid.Undef, xerrors.New("decoded deal ids are not a []uint64")
	}

	if len(dealIds) < 1 {
		return 0, cid.Undef, xerrors.New("Successful call to publish storage deals did not return deal ids")
	}

	return storagemarket.DealID(dealIds[0]), mcid, err
}

func (s *StorageProviderNodeConnector) ListProviderDeals(ctx context.Context, addr address.Address) ([]storagemarket.StorageDeal, error) {
	var smState spasm.StorageMarketActorState
	err := s.chainStore.GetActorStateAt(ctx, s.chainStore.Head(), fcaddr.StorageMarketAddress, &smState)
	if err != nil {
		return nil, err
	}

	// Dragons: ListDeals or similar should be an exported method on StorageMarketState. Do it ourselves for now.
	providerDealIds, ok := smState.CachedDealIDsByParty[addr]
	if !ok {
		return nil, errors.Errorf("No deals for %s", addr.String())
	}

	deals := []storagemarket.StorageDeal{}
	for dealId, _ := range providerDealIds {
		onChainDeal, ok := smState.Deals[dealId]
		if !ok {
			return nil, errors.Errorf("Could not find deal for id %d", dealId)
		}
		proposal := onChainDeal.Deal.Proposal
		deals = append(deals, storagemarket.StorageDeal{
			// Dragons: We're almost certainly looking for a CommP here.
			PieceRef:             proposal.PieceCID.Bytes(),
			PieceSize:            uint64(proposal.PieceSize.Total()),
			Client:               proposal.Client,
			Provider:             proposal.Provider,
			ProposalExpiration:   uint64(proposal.EndEpoch),
			Duration:             uint64(proposal.Duration()),
			StoragePricePerEpoch: tokenamount.FromInt(proposal.StoragePricePerEpoch.Int.Uint64()),
			StorageCollateral:    tokenamount.FromInt(proposal.ProviderCollateral.Int.Uint64()),
			ActivationEpoch:      uint64(proposal.StartEpoch),
		})
	}

	return deals, nil
}

func (s *StorageProviderNodeConnector) OnDealComplete(ctx context.Context, deal storagemarket.MinerDeal, pieceSize uint64, pieceReader io.Reader) (uint64, error) {
	// TODO: storage provider is expecting a sector ID here. This won't work. The sector ID needs to be removed from
	// TODO: the return value, and storage provider needs to call OnDealSectorCommitted which should add Sector ID to its
	// TODO: callback.
	return 0, s.pieceManager.SealPieceIntoNewSector(ctx, deal.DealID, pieceSize, pieceReader)
}

func (s *StorageProviderNodeConnector) GetMinerWorker(ctx context.Context, miner address.Address) (address.Address, error) {
	fcMiner, err := fcaddr.NewFromBytes(miner.Bytes())
	if err != nil {
		return address.Undef, err
	}

	fcworker, err := s.workerGetter(ctx, fcMiner, s.chainStore.Head())
	if err != nil {
		return address.Undef, err
	}

	return address.NewFromBytes(fcworker.Bytes())
}

func (s *StorageProviderNodeConnector) SignBytes(ctx context.Context, signer address.Address, b []byte) (*t2.Signature, error) {
	fcSigner, err := fcaddr.NewFromBytes(signer.Bytes())
	if err != nil {
		return nil, err
	}

	fcSig, err := s.wallet.SignBytes(b, fcSigner)
	if err != nil {
		return nil, err
	}

	var sigType string
	if signer.Protocol() == address.BLS {
		sigType = t2.KTBLS
	} else {
		sigType = t2.KTSecp256k1
	}
	return &t2.Signature{
		Type: sigType,
		Data: fcSig[:],
	}, nil
}

func (s *StorageProviderNodeConnector) OnDealSectorCommitted(ctx context.Context, provider address.Address, dealID uint64, cb storagemarket.DealSectorCommittedCallback) error {
	// TODO: is this provider address the miner address or the miner worker address?

	pred := func(msg *types.SignedMessage, msgCid cid.Cid) bool {
		m := msg.Message
		if m.Method != fcsm.CommitSector {
			return false
		}

		// TODO: compare addresses directly when they share a type #3719
		if m.From.String() != provider.String() {
			return false
		}

		values, err := abi.DecodeValues(m.Params, []abi.Type{abi.SectorProveCommitInfo})
		if err != nil {
			return false
		}

		commitInfo := values[0].Val.(*types.SectorProveCommitInfo)
		for _, id := range commitInfo.DealIDs {
			if uint64(id) == dealID {
				return true
			}
		}
		return false
	}

	_, found, err := s.waiter.Find(ctx, pred)
	if found {
		// TODO: DealSectorCommittedCallback should take a sector ID which we would provide here.
		cb(err)
		return nil
	}

	return s.waiter.WaitPredicate(ctx, pred, func(_ *block.Block, _ *types.SignedMessage, _ *types.MessageReceipt) error {
		// TODO: DealSectorCommittedCallback should take a sector ID which we would provide here.
		cb(nil)
		return nil
	})
}

func (s *StorageProviderNodeConnector) LocatePieceForDealWithinSector(ctx context.Context, dealID uint64) (sectorNumber uint64, offset uint64, length uint64, err error) {
	var smState spasm.StorageMarketActorState
	err = s.chainStore.GetActorStateAt(ctx, s.chainStore.Head(), fcaddr.StorageMarketAddress, &smState)
	if err != nil {
		return 0, 0, 0, err
	}

	minerAddr, err := fcaddr.NewFromBytes(s.minerAddr.Bytes())
	if err != nil {
		return 0, 0, 0, err
	}

	var minerState spaminer.StorageMinerActorState
	err = s.chainStore.GetActorStateAt(ctx, s.chainStore.Head(), minerAddr, &minerState)
	if err != nil {
		return 0, 0, 0, err
	}

	for sectorNumber, sectorInfo := range minerState.PreCommittedSectors {
		for _, deal := range sectorInfo.Info.DealIDs.Items {
			if uint64(deal) == dealID {
				offset := uint64(0)
				for _, did := range sectorInfo.Info.DealIDs.Items {
					deal, ok := smState.Deals[did]
					if !ok {
						return 0, 0, 0, errors.Errorf("Could not find miner deal %d in storage market state", did)
					}

					if uint64(did) == dealID {
						return uint64(sectorNumber), offset, uint64(deal.Deal.Proposal.PieceSize.Total()), nil
					}
					offset += uint64(deal.Deal.Proposal.PieceSize.Total())
				}
			}
		}
	}
	return 0, 0, 0, errors.New("Deal not found")
}

func (s *StorageProviderNodeConnector) wait(ctx context.Context, mcid cid.Cid, pubErrCh chan error) (*types.MessageReceipt, error) {
	receiptChan := make(chan *types.MessageReceipt)
	errChan := make(chan error)

	err := <-pubErrCh
	if err != nil {
		return nil, err
	}

	go func() {
		err := s.waiter.Wait(ctx, mcid, func(b *block.Block, message *types.SignedMessage, r *types.MessageReceipt) error {
			receiptChan <- r
			return nil
		})
		if err != nil {
			errChan <- err
		}
	}()

	select {
	case receipt := <-receiptChan:
		if receipt.ExitCode != 0 {
			return nil, xerrors.Errorf("non-zero exit code: %d", receipt.ExitCode)
		}

		return receipt, nil
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, xerrors.New("context ended prematurely")
	}
}

func (s *StorageProviderNodeConnector) getFCWorker(ctx context.Context) (fcaddr.Address, error) {
	worker, err := s.GetMinerWorker(ctx, s.minerAddr)
	if err != nil {
		return fcaddr.Undef, err
	}

	workerAddr, err := fcaddr.NewFromBytes(worker.Bytes())
	if err != nil {
		return fcaddr.Undef, err
	}
	return workerAddr, nil
}