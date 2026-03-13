package pg

import (
	"context"
	"fmt"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filabi "github.com/filecoin-project/go-state-types/abi"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/storacha/spade/service"
)

func (p *PgLotusSpadeService) PieceManifest(ctx context.Context, sp fil.ActorID, proposal uuid.UUID) (service.PieceManifest, error) {
	pcs := make([]struct {
		AggLog2Size int    `db:"agg_log2size"`
		AggPCidV1   string `db:"agg_pcid_v1"`
		SegPCidV2   string `db:"seg_pcid_v2"`
		UrlTemplate string
	}, 0, 8<<10)

	if err := pgxscan.Select(
		ctx,
		p.db,
		&pcs,
		`
		SELECT
				ap.piece_cid AS agg_pcid_v1,
				ap.piece_log2_size AS agg_log2size,
				sp.piece_cid AS seg_pcid_v2,
				t.tenant_meta->'bulk_piece_source'->>'url_template' AS url_template
			FROM spd.piece_segments ps
			JOIN spd.pieces ap USING ( piece_id )
			JOIN spd.pieces sp ON ( ps.segment_id = sp.piece_id )
			JOIN spd.proposals pr ON ( pr.piece_id = ps.piece_id )
			JOIN spd.clients cl USING ( client_id )
			JOIN spd.tenants t USING ( tenant_id )
		WHERE
			(ap.piece_meta->'is_frc58_segmented')::bool
				AND
			pr.proposal_uuid = $1
				AND
			-- ensure we only display SPs own proposals, no list-sharing
			pr.provider_id = $2
				AND
			-- only pending proposals
			pr.proposal_delivered IS NOT NULL AND pr.proposal_failstamp = 0 AND pr.activated_deal_id IS NULL

		-- ordering is critical
		ORDER BY ps.position
		`,
		proposal.String(),
		sp,
	); err != nil {
		return service.PieceManifest{}, err
	}

	if len(pcs) == 0 {
		return service.PieceManifest{}, service.ErrManifestNotFound
	}

	aggCP, err := fil.CommPFromPieceInfo(filabi.PieceInfo{
		Size:     1 << pcs[0].AggLog2Size,
		PieceCID: cid.MustParse(pcs[0].AggPCidV1),
	})
	if err != nil {
		return service.PieceManifest{}, err
	}

	segCids := make([]cid.Cid, 0, len(pcs))
	for _, pc := range pcs {
		s, err := cid.Parse(pc.SegPCidV2)
		if err != nil {
			return service.PieceManifest{}, fmt.Errorf("parsing segment CID %q: %w", pc.SegPCidV2, err)
		}
		segCids = append(segCids, s)
	}

	return service.PieceManifest{
		PieceCid:    aggCP.PCidV2(),
		SegmentCids: segCids,
		UrlTemplate: pcs[0].UrlTemplate,
	}, nil
}
