package spid

import (
	"context"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
)

// VerificationFilecoinClient defines the minimal Filecoin client interface
// required to support SPID verification.
type VerificationFilecoinClient interface {
	// ChainGetTipSetByHeight looks back for a tipset at the specified epoch.
	ChainGetTipSetByHeight(context.Context, abi.ChainEpoch, types.TipSetKey) (*types.TipSet, error)
	// StateAccountKey retrieves the key address for an account at a given tipset.
	StateAccountKey(context.Context, address.Address, types.TipSetKey) (address.Address, error)
	// StateMinerInfo retrieves miner info at a given tipset.
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (api.MinerInfo, error)
}
