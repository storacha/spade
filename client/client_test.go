package client_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	"github.com/google/uuid"
	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	blsgo "github.com/jsign/go-filsigner/bls"
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/client"
	"github.com/storacha/spade/service"
	"github.com/storacha/spade/spid"
	spid_args "github.com/storacha/spade/spid/args"
	"github.com/storacha/spade/spid/signature"
	"github.com/storacha/spade/webapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	bytes := make([]byte, size)
	_, err := rand.Read(bytes)
	require.NoError(t, err)
	return bytes
}

func startSpadeServer(t *testing.T, svc service.SpadeService) url.URL {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	e.Listener, _ = net.Listen("tcp", ":0")

	webapi.RegisterRoutes(e, svc)

	go e.Start(":0")
	t.Cleanup(func() { e.Shutdown(context.Background()) })

	baseURL, err := url.Parse(fmt.Sprintf("http://localhost:%d", e.Listener.Addr().(*net.TCPAddr).Port))
	require.NoError(t, err)
	return *baseURL
}

func mockStorageProvider(t *testing.T) (filaddr.Address, filaddr.Address, []byte) {
	sp, err := filaddr.NewIDAddress(1000)
	require.NoError(t, err)

	pk, err := base64.StdEncoding.DecodeString("DHdFHAqEZV/DJ8WlHqHkvxyEGqUfOJd78QwrkqkFrp4=")
	require.NoError(t, err)

	workerKey, err := blsgo.GetPubKey(pk)
	require.NoError(t, err)

	return sp, workerKey, pk
}

func TestClient(t *testing.T) {
	logging.SetLogLevel("spade/client", "DEBUG")

	sp, workerKey, pk := mockStorageProvider(t)

	svc := mockSpadeService{
		t:       t,
		entropy: map[filabi.ChainEpoch][]byte{},
		workers: map[filaddr.Address]filaddr.Address{sp: workerKey},
	}

	baseURL := startSpadeServer(t, &svc)

	signers := map[filaddr.Address]signature.BLSSigner{sp: {PrivateKey: pk}}

	identify := func(ctx context.Context, addr filaddr.Address, rawArgs url.Values) (spid.Challenge, error) {
		epoch := fil.ClockMainnet.TimeToEpoch(time.Now())
		entropy := randomBytes(t, 32)

		// set the generated entropy on the mock service so that when it receives
		// the request it can authorize it
		svc.entropy[epoch] = entropy

		signer, ok := signers[addr]
		if !ok {
			require.FailNowf(t, "missing signer", "address: %s", addr)
		}

		args, err := spid_args.NewFromValues(signer, entropy, rawArgs)
		require.NoError(t, err)
		return spid.New(addr, epoch, args)
	}

	c := client.New(sp, identify, client.WithBaseURL(baseURL))

	t.Run("eligible pieces", func(t *testing.T) {
		_, err := c.EligiblePieces(context.Background())
		require.NoError(t, err)
	})
}

type mockSpadeService struct {
	t       *testing.T
	entropy map[filabi.ChainEpoch][]byte
	workers map[filaddr.Address]filaddr.Address // sp address -> worker key
}

func (m *mockSpadeService) Authorize(ctx context.Context, req service.Request) (service.Authorization, error) {
	challenge, err := spid.Parse(req.Headers.Get("Authorization"))
	assert.NoError(m.t, err)

	// typically these are both resolved via filecoin chain state
	entropy, ok := m.entropy[challenge.Epoch()]
	if !ok {
		assert.FailNowf(m.t, "missing entropy for epoch", "epoch: %d", challenge.Epoch())
	}
	workerKey, ok := m.workers[challenge.Addr()]
	if !ok {
		assert.FailNowf(m.t, "missing worker key address", "sp: %s", challenge.Addr())
	}

	err = spid.Verify(workerKey, entropy, challenge)
	assert.NoError(m.t, err)

	sp := fil.MustParseActorString(challenge.Addr().String())
	signedArgs, err := challenge.Args().Values()
	assert.NoError(m.t, err)

	now := time.Now()

	return service.Authorization{
		RequestID:       uuid.New(),
		SignedArgs:      signedArgs,
		StateEpoch:      int64(challenge.Epoch()),
		ProviderID:      sp,
		ProviderDetails: [4]int16{1, 2, 3, 4},
		ProviderInfo:    apitypes.SPInfo{},
		LastPoll:        &now,
	}, nil
}

func (m *mockSpadeService) EligiblePieces(ctx context.Context, storageProvider fil.ActorID, options ...service.EligiblePiecesOption) ([]service.EligiblePiece, bool, error) {
	return nil, false, nil
}

func (m *mockSpadeService) PendingProposals(ctx context.Context, storageProvider fil.ActorID) ([]service.PendingProposal, error) {
	panic("unimplemented")
}

func (m *mockSpadeService) PieceManifest(ctx context.Context, storageProvider fil.ActorID, proposal uuid.UUID) (service.PieceManifest, error) {
	panic("unimplemented")
}

func (m *mockSpadeService) RequestError(ctx context.Context, requestID uuid.UUID, code apitypes.APIErrorCode, message string, payload any) error {
	panic("unimplemented")
}

func (m *mockSpadeService) ReservePiece(ctx context.Context, storageProvider fil.ActorID, storageProviderInfo apitypes.SPInfo, piece cid.Cid, options ...service.ReservePieceOption) ([]apitypes.TenantReplicationState, error) {
	panic("unimplemented")
}

var _ service.SpadeService = (*mockSpadeService)(nil)
