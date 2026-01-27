// SPID Lotus integration utilities
package lotus

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filprovider "github.com/filecoin-project/go-state-types/builtin/v9/miner"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/storacha/spade/spid"
	"github.com/storacha/spade/spid/args"
)

type LotusWalletSignerClient interface {
	// WalletSign signs the given bytes with the specified address.
	WalletSign(context.Context, filaddr.Address, []byte) (*crypto.Signature, error)
}

// LotusSigner is a [signature.Signer] that uses a Lotus node's wallet to sign
// messages.
type LotusSigner struct {
	Context context.Context         // context for the API call
	Addr    filaddr.Address         // address to sign with
	Client  LotusWalletSignerClient // lotus wallet client
}

func (ls LotusSigner) Sign(msg []byte) ([]byte, error) {
	sig, err := ls.Client.WalletSign(ls.Context, ls.Addr, msg)
	if err != nil {
		return nil, fmt.Errorf("signing message: %w", err)
	}
	return sig.Data, nil
}

type LotusTipSetGetterClient interface {
	// ChainGetTipSetByHeight looks back for a tipset at the specified epoch.
	ChainGetTipSetByHeight(context.Context, filabi.ChainEpoch, types.TipSetKey) (*types.TipSet, error)
}

type LotusWorkerAddressResolverClient interface {
	LotusTipSetGetterClient
	// StateAccountKey retrieves the key address for an account at a given tipset.
	StateAccountKey(context.Context, filaddr.Address, types.TipSetKey) (filaddr.Address, error)
	// StateMinerInfo retrieves miner info at a given tipset.
	StateMinerInfo(context.Context, filaddr.Address, types.TipSetKey) (api.MinerInfo, error)
}

type LotusSignatureVerifierClient interface {
	LotusTipSetGetterClient
	LotusWorkerAddressResolverClient
}

type LotusBeaconGetterAndWalletSignerClient interface {
	LotusTipSetGetterClient
	LotusWalletSignerClient
}

var beaconCache, _ = lru.New[filabi.ChainEpoch, types.BeaconEntry](spid.SigGraceEpochs * 4)

func BeaconByHeight(ctx context.Context, client LotusTipSetGetterClient, epoch filabi.ChainEpoch) (types.BeaconEntry, error) {
	be, didFind := beaconCache.Get(epoch)
	if !didFind {
		var curChallengeTs *fil.LotusTS
		var err error

		// Do it a few times because lotus is getting slower and slower to finalize 😭
		// Can't sleep too much though not to timeout the call
		// spid.bash has been adjusted with a backoff to deal with this as well
		for i := 0; i < 3; i++ {
			curChallengeTs, err = client.ChainGetTipSetByHeight(ctx, epoch, fil.LotusTSK{})
			if err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}

		if err != nil {
			// do not make slow-chain a 500
			return types.BeaconEntry{}, fmt.Errorf(
				"unable to get tipset at height %d (%s): %s",
				epoch, fil.ClockMainnet.EpochToTime(epoch),
				err,
			)
		}
		be = curChallengeTs.Blocks()[0].BeaconEntries[len(curChallengeTs.Blocks()[0].BeaconEntries)-1]
		beaconCache.Add(epoch, be)
	}
	return be, nil
}

// ResolveWorkerAddress resolves the worker address for the given storage
// provider address. The address must be resolvable as of
// [filprovider.ChainFinality] epochs before the specified epoch.
func ResolveWorkerAddress(ctx context.Context, client LotusWorkerAddressResolverClient, addr filaddr.Address, epoch filabi.ChainEpoch) (filaddr.Address, error) {
	miFinTs, err := client.ChainGetTipSetByHeight(ctx, epoch-filprovider.ChainFinality, fil.LotusTSK{})
	if err != nil {
		return filaddr.Address{}, err
	}
	mi, err := client.StateMinerInfo(ctx, addr, miFinTs.Key())
	if err != nil {
		return filaddr.Address{}, err
	}
	return client.StateAccountKey(ctx, mi.Worker, miFinTs.Key())
}

// ResolveAndVerify resolves the worker address for the given storage provider
// address at the challenge epoch, retrieves the beacon entry for that epoch,
// and verifies the signature over the challenge args.
//
// This is a convenience function that combines [ResolveWorkerAddress],
// [BeaconByHeight], and [Verify] in one step.
func ResolveAndVerify(ctx context.Context, client LotusSignatureVerifierClient, challenge spid.Challenge) error {
	worker, err := ResolveWorkerAddress(ctx, client, challenge.Addr(), challenge.Epoch())
	if err != nil {
		return fmt.Errorf("resolving worker address %q at height %d: %w", challenge.Addr(), challenge.Epoch(), err)
	}

	beacon, err := BeaconByHeight(ctx, client, challenge.Epoch())
	if err != nil {
		return fmt.Errorf("getting beacon at height %d: %w", challenge.Epoch(), err)
	}

	return spid.Verify(worker, beacon.Data, challenge)
}

// New creates a new SPID challenge for the given storage provider address and
// values, using the Lotus API to retrieve the beacon data for the current epoch
// and sign the args.
func New(ctx context.Context, client LotusBeaconGetterAndWalletSignerClient, addr filaddr.Address, values url.Values) (spid.Challenge, error) {
	epoch := fil.ClockMainnet.TimeToEpoch(time.Now())
	beacon, err := BeaconByHeight(ctx, client, epoch)
	if err != nil {
		return spid.Challenge{}, fmt.Errorf("getting beacon at height %d: %w", epoch, err)
	}
	args, err := NewArgs(ctx, client, addr, beacon.Data, values)
	if err != nil {
		return spid.Challenge{}, fmt.Errorf("creating SPID args: %w", err)
	}
	return spid.New(addr, epoch, args)
}

// NewArgs creates SPID authorization args using the Lotus API to sign them.
func NewArgs(ctx context.Context, client LotusWalletSignerClient, addr filaddr.Address, entropy []byte, values url.Values) (args.Args, error) {
	signer := LotusSigner{
		Context: ctx,
		Addr:    addr,
		Client:  client,
	}
	return args.NewFromValues(signer, entropy, values)
}
