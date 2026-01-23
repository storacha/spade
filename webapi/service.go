package main

import (
	"context"
	"fmt"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/storacha/spade/apitypes"
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
	TenantID int16
}

func WithReservePieceTenantID(tenantID int16) ReservePieceOption {
	return func(opts *ReservePieceConfig) {
		opts.TenantID = tenantID
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
	PieceCid      cid.Cid // aggregated piece CID (v1)
	PieceLog2Size int
	SegmentCids   []cid.Cid // segment piece CIDs (v2)
	UrlTemplate   string
}

// Service defines the core business logic of the Spade SP web API.
type Service interface {
	// EligiblePieces lists Piece CIDs a storage provider is eligible to receive a
	// deal for. Unless configured differently, [listEligibleDefaultSize] pieces
	// are returned. The boolean return value indicates whether there are more
	// pieces available beyond those returned.
	EligiblePieces(ctx context.Context, actor fil.ActorID, options ...EligiblePiecesOption) ([]EligiblePiece, bool, error)
	// PendingProposals lists current outstanding reservations including those in
	// error.
	PendingProposals(ctx context.Context, actor fil.ActorID) ([]PendingProposal, error)
	// PieceManifest produces a manifest for a segmented piece.
	PieceManifest(ctx context.Context, actor fil.ActorID, proposal uuid.UUID) (PieceManifest, error)
	// ReservePiece requests a deal proposal (and thus reservation) for a specific
	// Piece CID.
	ReservePiece(ctx context.Context, actor fil.ActorID, piece cid.Cid, options ...ReservePieceOption) ([]apitypes.TenantReplicationState, error)
}

type PgService struct {
	db pgxscan.Querier
}

func NewPgService(db pgxscan.Querier) *PgService {
	return &PgService{db}
}

func (p *PgService) EligiblePieces(ctx context.Context, actor fil.ActorID, options ...EligiblePiecesOption) ([]EligiblePiece, bool, error) {
	cfg := EligiblePiecesConfig{Limit: listEligibleDefaultSize}
	for _, opt := range options {
		opt(&cfg)
	}

	lim := cfg.Limit
	tenantID := cfg.TenantID
	// how to list: start small, find setting below
	useQueryFunc := "pieces_eligible_head"
	if lim > listEligibleDefaultSize { // deduce from requested lim
		useQueryFunc = "pieces_eligible_full"
	}

	orderedPieces := make([]EligiblePiece, 0, lim+1)
	if err := pgxscan.Select(
		ctx,
		p.db,
		&orderedPieces,
		fmt.Sprintf("SELECT * FROM spd.%s( $1, $2, $3, $4, $5 )", useQueryFunc),
		actor,
		lim+1, // ask for one extra, to disambiguate "there is more"
		tenantID,
		cfg.IncludeSourceless,
		false,
	); err != nil {
		return nil, false, err
	}

	var more bool
	if uint64(len(orderedPieces)) > lim {
		orderedPieces = orderedPieces[:lim]
		more = true
	}
	return orderedPieces, more, nil
}

func (p *PgService) PendingProposals(ctx context.Context, actor fil.ActorID) ([]PendingProposal, error) {
	pending := make([]PendingProposal, 0, 4096)

	if err := pgxscan.Select(
		ctx,
		p.db,
		&pending,
		`
			SELECT
					pr.proposal_uuid AS proposal_id,
					pr.piece_id,
					pr.proposal_meta->>'signed_proposal_cid' AS proposal_cid,
					pr.start_epoch,
					pr.client_id,
					pr.proposal_delivered,
					c.tenant_id,
					p.piece_cid,
					pr.proxied_log2_size AS piece_log2_size,
					pr.proposal_failstamp,
					pr.proposal_meta->>'failure' AS error,
					( EXISTS (
						SELECT 42
							FROM spd.published_deals pd
						WHERE
							pd.piece_id = pr.piece_id
								AND
							pd.provider_id = pr.provider_id
								AND
							pd.client_id = pr.client_id
								AND
							pd.status = 'published'
					) ) AS is_published,
					ARRAY(
						SELECT uri FROM spd.sources_uri WHERE sources_uri.piece_id = pr.piece_id
					) AS data_sources,
					(
						CASE WHEN (p.piece_meta->'is_frc58_segmented')::bool THEN 'frc58' ELSE NULL END
					) AS segmentation
				FROM spd.proposals pr
				JOIN spd.pieces p USING ( piece_id )
				JOIN spd.clients c USING ( client_id )
				LEFT JOIN spd.mv_pieces_availability pa USING ( piece_id )
			WHERE
				pr.provider_id = $1
					AND
				pr.start_epoch > $2
					AND
				pr.activated_deal_id is NULL
					AND
				(
					pr.proposal_failstamp = 0
						OR
					-- show everything failed in the past N hours
					pr.proposal_failstamp > ( spd.big_now() - $3::BIGINT * 3600 * 1000 * 1000 * 1000 )
				)
			ORDER BY
				pr.proposal_failstamp DESC,
				( pr.start_epoch / 360 ), -- 3h sort granularity
				pr.proxied_log2_size,
				p.piece_cid
			`,
		actor,
		fil.ClockMainnet.TimeToEpoch(time.Now())+filbuiltin.EpochsInHour,
		showRecentFailuresHours,
	); err != nil {
		return nil, err
	}
	return pending, nil
}

func (p *PgService) PieceManifest(ctx context.Context, actor fil.ActorID, proposal uuid.UUID) (PieceManifest, error) {
	panic("unimplemented")
}

func (p *PgService) ReservePiece(ctx context.Context, actor fil.ActorID, piece cid.Cid, options ...ReservePieceOption) ([]apitypes.TenantReplicationState, error) {
	panic("unimplemented")
}

var _ Service = (*PgService)(nil)
