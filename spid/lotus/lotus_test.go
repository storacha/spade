package lotus_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"testing"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filprovider "github.com/filecoin-project/go-state-types/builtin/v9/miner"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-cid"
	blsgo "github.com/jsign/go-filsigner/bls"
	"github.com/storacha/spade/spid"
	spid_lotus "github.com/storacha/spade/spid/lotus"
	"github.com/stretchr/testify/require"
)

func TestLotus(t *testing.T) {
	sp, err := filaddr.NewIDAddress(1000)
	require.NoError(t, err)

	workerID, err := filaddr.NewIDAddress(1001)
	require.NoError(t, err)

	pk, err := base64.StdEncoding.DecodeString("DHdFHAqEZV/DJ8WlHqHkvxyEGqUfOJd78QwrkqkFrp4=")
	require.NoError(t, err)

	workerKey, err := blsgo.GetPubKey(pk)
	require.NoError(t, err)

	argVals := url.Values{}
	argVals.Set("foo", "bar")

	t.Run("roundtrip", func(t *testing.T) {
		epoch := fil.ClockMainnet.TimeToEpoch(time.Now())

		lotusAPI := &mockLotusClient{
			t:       t,
			wallets: map[filaddr.Address][]byte{sp: pk},
			miners:  map[filaddr.Address]filaddr.Address{sp: workerID},
			workers: map[filaddr.Address]filaddr.Address{workerID: workerKey},
			beacons: map[filabi.ChainEpoch]fil.LotusBeaconEntry{
				epoch: {Round: 1000, Data: []byte("beacon_for_1000")},
				// we add a beacon for the next epoch, just incase we transition during
				// the test.
				epoch + 1: {Round: 1000, Data: []byte("beacon_for_1001")},
				// we must add a beacon for the finality epoch too, as it's used
				// to resolve the worker address.
				epoch - filprovider.ChainFinality:     {Round: 100, Data: []byte("beacon_for_100")},
				epoch - filprovider.ChainFinality + 1: {Round: 101, Data: []byte("beacon_for_101")},
			},
		}

		id, err := spid_lotus.New(context.Background(), lotusAPI, sp, argVals)
		require.NoError(t, err)

		idstr := spid.Format(id)
		t.Logf("Authorization: %s", idstr)

		parsed, err := spid.Parse(idstr)
		require.NoError(t, err)
		require.Equal(t, id, parsed)

		err = spid_lotus.ResolveAndVerify(context.Background(), lotusAPI, parsed)
		require.NoError(t, err)
	})
}

type mockLotusClient struct {
	t       *testing.T
	wallets map[filaddr.Address][]byte          // address -> private key (bls only)
	miners  map[filaddr.Address]filaddr.Address // miner ID -> worker ID
	workers map[filaddr.Address]filaddr.Address // worker ID -> worker key
	beacons map[filabi.ChainEpoch]fil.LotusBeaconEntry
}

func (m *mockLotusClient) ChainGetTipSetByHeight(ctx context.Context, epoch filabi.ChainEpoch, tsk types.TipSetKey) (*types.TipSet, error) {
	be, ok := m.beacons[epoch]
	if !ok {
		return nil, fmt.Errorf("tipset for epoch %d not found", epoch)
	}
	bh := mockBlockHeader(m.t)
	bh.BeaconEntries = []types.BeaconEntry{be}
	return types.NewTipSet([]*types.BlockHeader{bh})
}

func (m *mockLotusClient) StateAccountKey(ctx context.Context, worker filaddr.Address, tsk types.TipSetKey) (filaddr.Address, error) {
	worker, ok := m.workers[worker]
	if !ok {
		return filaddr.Address{}, fmt.Errorf("account key for worker %s not found", worker)
	}
	return worker, nil
}

func (m *mockLotusClient) StateMinerInfo(ctx context.Context, addr filaddr.Address, tsk types.TipSetKey) (api.MinerInfo, error) {
	worker, ok := m.miners[addr]
	if !ok {
		return api.MinerInfo{}, fmt.Errorf("info for miner %s not found", addr)
	}
	return api.MinerInfo{Worker: worker}, nil
}

func (m *mockLotusClient) WalletSign(ctx context.Context, addr filaddr.Address, msg []byte) (*crypto.Signature, error) {
	pk, ok := m.wallets[addr]
	if !ok {
		return nil, fmt.Errorf("no wallet for address %s", addr)
	}
	sig, err := blsgo.Sign(pk, msg)
	if err != nil {
		return nil, fmt.Errorf("signing message: %w", err)
	}
	return &crypto.Signature{Type: crypto.SigTypeBLS, Data: sig}, nil
}

var _ spid_lotus.LotusSignatureVerifierClient = (*mockLotusClient)(nil)

func mockBlockHeader(t testing.TB) *types.BlockHeader {
	addr, err := filaddr.NewIDAddress(12512063)
	require.NoError(t, err)

	c, err := cid.Decode("bafyreicmaj5hhoy5mgqvamfhgexxyergw7hdeshizghodwkjg6qmpoco7i")
	require.NoError(t, err)

	return &types.BlockHeader{
		Miner: addr,
		Ticket: &types.Ticket{
			VRFProof: []byte("vrf proof0000000vrf proof0000000"),
		},
		ElectionProof: &types.ElectionProof{
			VRFProof: []byte("vrf proof0000000vrf proof0000000"),
		},
		Parents:               []cid.Cid{c, c},
		ParentMessageReceipts: c,
		BLSAggregate:          &crypto.Signature{Type: crypto.SigTypeBLS, Data: []byte("boo! im a signature")},
		ParentWeight:          types.NewInt(123125126212),
		Messages:              c,
		Height:                85919298723,
		ParentStateRoot:       c,
		BlockSig:              &crypto.Signature{Type: crypto.SigTypeBLS, Data: []byte("boo! im a signature")},
		ParentBaseFee:         types.NewInt(3432432843291),
	}
}
