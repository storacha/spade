package service

import (
	"context"
	"fmt"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filbig "github.com/filecoin-project/go-state-types/big"
	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/storacha/spade/internal/app"
)

// AuthorizationLotusClient defines the minimal Filecoin client interface
// required to support SP authorization.
type AuthorizationLotusClient interface {
	// ChainGetTipSetByHeight looks back for a tipset at the specified epoch.
	ChainGetTipSetByHeight(context.Context, abi.ChainEpoch, types.TipSetKey) (*types.TipSet, error)
	// StateAccountKey retrieves the key address for an account at a given tipset.
	StateAccountKey(context.Context, address.Address, types.TipSetKey) (address.Address, error)
	// StateMinerInfo retrieves miner info at a given tipset.
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (api.MinerInfo, error)
}

// LookbackLotusClient defines the minimal Filecoin client interface required to
// support lookback tipset retrieval.
type LookbackLotusClient interface {
	// ChainHead returns the current head of the chain.
	ChainHead(context.Context) (*types.TipSet, error)
	// ChainGetTipSetByHeight looks back for a tipset at the specified epoch.
	ChainGetTipSetByHeight(context.Context, abi.ChainEpoch, types.TipSetKey) (*types.TipSet, error)
}

// ReservationLotusClient defines the minimal Filecoin client interface required
// to support reservation eligibility checks and related operations.
type ReservationLotusClient interface {
	LookbackLotusClient
	// MinerGetBaseInfo retrieves mining base info for a miner at a given tipset.
	MinerGetBaseInfo(context.Context, address.Address, abi.ChainEpoch, types.TipSetKey) (*api.MiningBaseInfo, error)
}

// SpadeLotusClient defines the minimal Lotus client interface required to
// support all Spade service operations.
type SpadeLotusClient interface {
	AuthorizationLotusClient
	ReservationLotusClient
}

var collateralCache, _ = lru.New[filabi.ChainEpoch, filbig.Int](128)

func ProviderCollateralEstimateGiB(ctx context.Context, sourceEpoch filabi.ChainEpoch) (filbig.Int, error) {
	if pc, didFind := collateralCache.Get(sourceEpoch); didFind {
		return pc, nil
	}

	collateralGiB, err := app.EpochMinProviderCollateralEstimateGiB(ctx, sourceEpoch)
	if err != nil {
		return collateralGiB, cmn.WrErr(err)
	}

	// make it 1.7 times larger, so that fluctuations in the state won't prevent the deal from being proposed/published later
	// capped by https://github.com/filecoin-project/lotus/blob/v1.13.2-rc2/markets/storageadapter/provider.go#L267
	// and https://github.com/filecoin-project/lotus/blob/v1.13.2-rc2/markets/storageadapter/provider.go#L41
	inflatedCollateralGiB := filbig.Div(
		filbig.Product(
			collateralGiB,
			filbig.NewInt(17),
		),
		filbig.NewInt(10),
	)

	collateralCache.Add(sourceEpoch, inflatedCollateralGiB)
	return inflatedCollateralGiB, nil
}

// GetTipset retrieves the tipset at the specified lookback epoch. It is a
// copy of [fil.GetTipset] adjusted to use the minimal interface
// [LookbackLotusClient].
func GetTipset(ctx context.Context, lapi LookbackLotusClient, lookback uint) (*fil.LotusTS, error) {
	latestHead, err := lapi.ChainHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed getting chain head: %w", err)
	}

	wallUnix := time.Now().Unix()
	filUnix := int64(latestHead.Blocks()[0].Timestamp)

	if wallUnix < filUnix-3 || // allow few seconds clock-drift tolerance
		wallUnix > filUnix+int64(
			fil.PropagationDelaySecs+(fil.APIMaxTipsetsBehind*filbuiltin.EpochDurationSeconds),
		) {
		return nil, fmt.Errorf(
			"lotus API out of sync: chainHead reports unixtime %d (height: %d) while walltime is %d (delta: %s)",
			filUnix,
			latestHead.Height(),
			wallUnix,
			time.Second*time.Duration(wallUnix-filUnix),
		)
	}

	if lookback == 0 {
		return latestHead, nil
	}

	latestHeight := latestHead.Height()
	tipsetAtLookback, err := lapi.ChainGetTipSetByHeight(ctx, latestHeight-filabi.ChainEpoch(lookback), latestHead.Key())
	if err != nil {
		return nil, fmt.Errorf("determining target tipset %d epochs ago failed: %w", lookback, err)
	}

	return tipsetAtLookback, nil
}
