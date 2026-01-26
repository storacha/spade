package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/storacha/spade/apitypes"
)

// Service errors map to [apitypes.APIErrorCode] codes.
var (
	ErrManifestNotFound                = errors.New("piece manifest not found")
	ErrUnclaimedPiece                  = errors.New("piece is not claimed by any selected tenant")
	ErrOversizedPiece                  = errors.New("piece size exceeds provider's sector size")
	ErrProviderHasReplica              = errors.New("provider already has proposed or active replica according to all selected replication rules")
	ErrTenantsOutOfDatacap             = errors.New("all selected tenants are out of DataCap")
	ErrTooManyReplicas                 = errors.New("piece is over-replicated according to all selected replication rules")
	ErrProviderAboveMaxInFlight        = errors.New("provider has more proposals in-flight than permitted by selected tenant rules")
	ErrReplicationRulesViolation       = errors.New("no selected tenants would grant a deal according to their individual rules")
	ErrTenantPolicyMismatch            = errors.New("tenant policy does not match the expected value")
	ErrStorageProviderSuspended        = errors.New("provider is suspended")
	ErrStorageProviderIneligibleToMine = errors.New("provider is ineligible to mine new deals")
)

type EligiblePiece struct {
	PieceID       int64
	PieceLog2Size uint8
	Tenants       []int16 `db:"tenant_ids"`
	apitypes.Piece
}

type EligiblePiecesOption = func(*EligiblePiecesConfig)

type EligiblePiecesConfig struct {
	TenantID          int16
	Limit             uint64
	IncludeSourceless bool
}

func WithEligiblePiecesIncludeSourceless(include bool) EligiblePiecesOption {
	return func(opts *EligiblePiecesConfig) {
		opts.IncludeSourceless = include
	}
}

func WithEligiblePiecesTenantID(tenantID int16) EligiblePiecesOption {
	return func(opts *EligiblePiecesConfig) {
		opts.TenantID = tenantID
	}
}

func WithEligiblePiecesLimit(limit uint64) EligiblePiecesOption {
	return func(opts *EligiblePiecesConfig) {
		opts.Limit = limit
	}
}

type ReservePieceOption = func(*ReservePieceConfig)

type ReservePieceConfig struct {
	TenantID     int16
	TenantPolicy string
	RequestID    uuid.UUID
}

// WithReservePieceTenantID sets an optional tenant ID the pirce should be
// reserved with.
func WithReservePieceTenantID(tenantID int16) ReservePieceOption {
	return func(opts *ReservePieceConfig) {
		opts.TenantID = tenantID
	}
}

// WithReservePieceTenantPolicy sets an optional tenant policy string to
// validate against when reserving the piece. Note: if the chosen tenant has a
// policy configured, this must match. i.e. this is mandatory for tenants that
// have a policy set.
func WithReservePieceTenantPolicy(tenantPolicy string) ReservePieceOption {
	return func(opts *ReservePieceConfig) {
		opts.TenantPolicy = tenantPolicy
	}
}

// WithReservePieceRequestID sets an optional request ID to associate with the
// reservation.
func WithReservePieceRequestID(requestID uuid.UUID) ReservePieceOption {
	return func(opts *ReservePieceConfig) {
		opts.RequestID = requestID
	}
}

type PendingProposal struct {
	apitypes.DealProposal
	ClientID          fil.ActorID
	PieceID           int64
	ProposalFailstamp int64
	Error             *string
	ProposalDelivered *time.Time
	IsPublished       bool
	PieceLog2Size     int8
}

type PieceManifest struct {
	PieceCid    cid.Cid   // aggregated piece CID (v2)
	SegmentCids []cid.Cid // segment piece CIDs (v2)
	UrlTemplate string
}

type Request struct {
	Method  string
	Host    string
	Path    string
	Params  url.Values
	Headers http.Header
}

type Authorization struct {
	RequestID       uuid.UUID
	SignedArgs      url.Values
	StateEpoch      int64
	ProviderID      fil.ActorID
	ProviderDetails [4]int16
	ProviderInfo    apitypes.SPInfo
	LastPoll        *time.Time
}

type AuthorizationService interface {
	// Authorize validates and verifies a request's SPID challenge and returns
	// authorization details.
	Authorize(ctx context.Context, req Request) (Authorization, error)
}

type EligibilityService interface {
	// EligiblePieces lists Piece CIDs a storage provider is eligible to receive a
	// deal for. Unless configured differently, [listEligibleDefaultSize] pieces
	// are returned. The boolean return value indicates whether there are more
	// pieces available beyond those returned.
	EligiblePieces(ctx context.Context, storageProvider fil.ActorID, options ...EligiblePiecesOption) ([]EligiblePiece, bool, error)
}

type ProposalService interface {
	// PendingProposals lists current outstanding reservations including those in
	// error.
	PendingProposals(ctx context.Context, storageProvider fil.ActorID) ([]PendingProposal, error)
}

type PieceManifestService interface {
	// PieceManifest produces a manifest for a segmented piece.
	PieceManifest(ctx context.Context, storageProvider fil.ActorID, proposal uuid.UUID) (PieceManifest, error)
}

type ReservationService interface {
	// ReservePiece requests a deal proposal (and thus reservation) for a specific
	// Piece CID. Note: replication states may be returned for feedback to users
	// even when an error occurs.
	ReservePiece(ctx context.Context, storageProvider fil.ActorID, storageProviderInfo apitypes.SPInfo, piece cid.Cid, options ...ReservePieceOption) ([]apitypes.TenantReplicationState, error)
}

// SpadeService defines the core business logic of the Spade SP API.
type SpadeService interface {
	AuthorizationService
	EligibilityService
	ProposalService
	PieceManifestService
	ReservationService
}
