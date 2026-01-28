package client_test

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/url"
	"testing"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	commp "github.com/filecoin-project/go-fil-commp-hashhash"
	filabi "github.com/filecoin-project/go-state-types/abi"
	"github.com/google/uuid"
	cid "github.com/ipfs/go-cid"
	blsgo "github.com/jsign/go-filsigner/bls"
	"github.com/labstack/echo/v4"
	"github.com/multiformats/go-multihash"
	"github.com/storacha/go-piece/pkg/digest"
	"github.com/storacha/go-piece/pkg/piece"
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

func TestClient(t *testing.T) {
	// logging.SetLogLevel("spade/client", "DEBUG")

	sp, workerKey, pk := mockStorageProvider(t)

	svc := mockSpadeService{
		t:                t,
		entropy:          map[filabi.ChainEpoch][]byte{},
		workers:          map[filaddr.Address]filaddr.Address{sp: workerKey},
		eligiblePieces:   map[filaddr.Address][]service.EligiblePiece{},
		pendingProposals: map[filaddr.Address][]service.PendingProposal{},
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
		eligiblePiece := mockEligiblePiece(t)
		svc.eligiblePieces[sp] = []service.EligiblePiece{eligiblePiece}

		res, err := c.EligiblePieces(context.Background())
		require.NoError(t, err)
		require.Len(t, res, 1)
		require.Equal(t, eligiblePiece.PieceCid, res[0].PieceCid)
		require.Equal(t, eligiblePiece.PaddedPieceSize, res[0].PaddedPieceSize)
		require.Equal(t, eligiblePiece.ClaimingTenant, res[0].ClaimingTenant)
	})

	t.Run("pending proposals", func(t *testing.T) {
		undeliveredProposal := mockUndeliveredProposal(t, sp) // should not see in results
		deliveredProposal := mockDeliveredProposal(t, sp)
		failedProposal := mockFailedProposal(t, sp)
		publishedProposal := mockPublishedProposal(t, sp) // should not see in results
		svc.pendingProposals[sp] = []service.PendingProposal{
			undeliveredProposal,
			deliveredProposal,
			failedProposal,
			publishedProposal,
		}

		res, err := c.PendingProposals(context.Background())
		require.NoError(t, err)

		require.Len(t, res.PendingProposals, 1)
		require.Equal(t, deliveredProposal.ProposalID, res.PendingProposals[0].ProposalID)
		require.Equal(t, deliveredProposal.ProposalCid, res.PendingProposals[0].ProposalCid)
		require.Equal(t, deliveredProposal.HoursRemaining, res.PendingProposals[0].HoursRemaining)
		require.Equal(t, deliveredProposal.PieceSize, res.PendingProposals[0].PieceSize)
		require.Equal(t, deliveredProposal.PieceCid, res.PendingProposals[0].PieceCid)
		require.Equal(t, deliveredProposal.TenantID, res.PendingProposals[0].TenantID)
		require.Equal(t, deliveredProposal.TenantClient, res.PendingProposals[0].TenantClient)
		require.Equal(t, deliveredProposal.StartTime, res.PendingProposals[0].StartTime)
		require.Equal(t, deliveredProposal.StartEpoch, res.PendingProposals[0].StartEpoch)

		require.Len(t, res.RecentFailures, 1)
		require.Equal(t, failedProposal.ProposalID, res.RecentFailures[0].ProposalID)
		require.Equal(t, failedProposal.ProposalCid, res.RecentFailures[0].ProposalCid)
		require.Equal(t, failedProposal.PieceCid, res.RecentFailures[0].PieceCid)
		require.Equal(t, failedProposal.TenantID, res.RecentFailures[0].TenantID)
		require.Equal(t, failedProposal.TenantClient, res.RecentFailures[0].TenantClient)
		require.Equal(t, *failedProposal.Error, res.RecentFailures[0].Error)
		require.Equal(t, failedProposal.ProposalFailstamp, res.RecentFailures[0].ErrorTimeStamp.UnixNano())
	})
}

func startSpadeServer(t *testing.T, svc service.SpadeService) url.URL {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Listener, _ = net.Listen("tcp", ":0")

	webapi.RegisterRoutes(e, svc)

	go e.Start(":0")
	t.Cleanup(func() { e.Shutdown(context.Background()) })

	baseURL, err := url.Parse(fmt.Sprintf("http://localhost:%d", e.Listener.Addr().(*net.TCPAddr).Port))
	require.NoError(t, err)
	return *baseURL
}

func mockStorageProvider(t *testing.T) (filaddr.Address, filaddr.Address, []byte) {
	sp, err := filaddr.NewIDAddress(uint64(100 + rand.Int63n(10000)))
	require.NoError(t, err)

	pk, err := base64.StdEncoding.DecodeString("DHdFHAqEZV/DJ8WlHqHkvxyEGqUfOJd78QwrkqkFrp4=")
	require.NoError(t, err)

	workerKey, err := blsgo.GetPubKey(pk)
	require.NoError(t, err)

	return sp, workerKey, pk
}

type mockSpadeService struct {
	t                *testing.T
	entropy          map[filabi.ChainEpoch][]byte
	workers          map[filaddr.Address]filaddr.Address // sp address -> worker key
	eligiblePieces   map[filaddr.Address][]service.EligiblePiece
	pendingProposals map[filaddr.Address][]service.PendingProposal
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
	return m.eligiblePieces[storageProvider.AsFilAddr()], false, nil
}

func (m *mockSpadeService) PendingProposals(ctx context.Context, storageProvider fil.ActorID) ([]service.PendingProposal, error) {
	return m.pendingProposals[storageProvider.AsFilAddr()], nil
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

const sectorSize = 32 * 1024

func mockEligiblePiece(t *testing.T) service.EligiblePiece {
	t.Helper()
	unpaddedSize := sectorSize + rand.Int63n(sectorSize/2)
	piece := randomPiece(t, unpaddedSize)
	return service.EligiblePiece{
		PieceID:       1 + rand.Int63n(1000),
		PieceLog2Size: piece.Height() + uint8(math.Log2(32)),
		Tenants:       []int16{13},
		Piece: apitypes.Piece{
			PieceCid:         piece.Link().String(),
			PaddedPieceSize:  piece.PaddedSize(),
			ClaimingTenant:   13,
			TenantPolicyCid:  randomCID(t).String(),
			SampleReserveCmd: "test",
		},
	}
}

func mockUndeliveredProposal(t *testing.T, sp filaddr.Address) service.PendingProposal {
	t.Helper()
	unpaddedSize := sectorSize + rand.Int63n(sectorSize/2)
	piece := randomPiece(t, unpaddedSize)

	startEpoch := fil.ClockMainnet.TimeToEpoch(time.Now()) + 900
	startTime := fil.ClockMainnet.EpochToTime(startEpoch)
	segmentation := "frc58"

	return service.PendingProposal{
		ClientID:          fil.MustParseActorString(sp.String()),
		PieceID:           1 + rand.Int63n(1000),
		ProposalFailstamp: 0,
		Error:             nil,
		ProposalDelivered: nil,
		IsPublished:       false,
		PieceLog2Size:     int8(piece.Height()) + int8(math.Log2(32)),
		DealProposal: apitypes.DealProposal{
			ProposalID:     uuid.New().String(),
			ProposalCid:    nil,
			HoursRemaining: int(time.Until(startTime).Truncate(time.Hour).Hours()),
			PieceSize:      int64(piece.PaddedSize()),
			PieceCid:       piece.Link().String(),
			TenantID:       13,
			TenantClient:   sp.String(),
			StartTime:      startTime,
			StartEpoch:     int64(startEpoch),
			ImportCmd:      "",
			Segmentation:   &segmentation,
			AssemblyCmd:    nil,
			DataSources:    []string{},
		},
	}
}

func mockDeliveredProposal(t *testing.T, sp filaddr.Address) service.PendingProposal {
	t.Helper()
	p := mockUndeliveredProposal(t, sp)
	now := time.Now()
	p.ProposalDelivered = &now
	return p
}

func mockFailedProposal(t *testing.T, sp filaddr.Address) service.PendingProposal {
	t.Helper()
	p := mockDeliveredProposal(t, sp)
	p.ProposalFailstamp = time.Now().UnixNano()
	errMsg := "test failed proposal"
	p.Error = &errMsg
	return p
}

func mockPublishedProposal(t *testing.T, sp filaddr.Address) service.PendingProposal {
	t.Helper()
	p := mockDeliveredProposal(t, sp)
	p.IsPublished = true
	return p
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	bytes := make([]byte, size)
	_, err := crand.Read(bytes)
	require.NoError(t, err)
	return bytes
}

func randomCID(t *testing.T) cid.Cid {
	return cid.NewCidV1(cid.Raw, randomMultihash(t))
}

func randomMultihash(t *testing.T) multihash.Multihash {
	bytes := randomBytes(t, 10)
	digest, err := multihash.Sum(bytes, multihash.SHA2_256, -1)
	assert.NoError(t, err)
	return digest
}

func randomPiece(t *testing.T, unpaddedSize int64) piece.PieceLink {
	t.Helper()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	dataReader := io.LimitReader(r, unpaddedSize)

	calc := &commp.Calc{}
	n, err := io.Copy(calc, dataReader)
	assert.NoError(t, err, "failed copying data into commp.Calc")
	assert.Equal(t, unpaddedSize, n)

	commP, paddedSize, err := calc.Digest()
	assert.NoError(t, err, "failed to compute commP")

	pieceDigest, err := digest.FromCommitmentAndSize(commP, uint64(unpaddedSize))
	assert.NoError(t, err, "failed building piece digest from commP")

	p := piece.FromPieceDigest(pieceDigest)
	assert.Equal(t, paddedSize, p.PaddedSize())

	return p
}
