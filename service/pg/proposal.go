package pg

import (
	"context"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/storacha/spade/service"
)

func (p *PgLotusSpadeService) PendingProposals(ctx context.Context, sp fil.ActorID) ([]service.PendingProposal, error) {
	pending := make([]service.PendingProposal, 0, 4096)

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
		sp,
		fil.ClockMainnet.TimeToEpoch(time.Now())+filbuiltin.EpochsInHour,
		service.ShowRecentFailuresHours,
	); err != nil {
		return nil, err
	}
	return pending, nil
}
