package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"text/template"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"code.riba.cloud/go/toolbox/ufcli"
	filabi "github.com/filecoin-project/go-state-types/abi"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/internal/app"
	"golang.org/x/xerrors"
)

var pieceManifestCmd = &ufcli.Command{
	Usage: "generate a piece manifest by proposal uuid",
	Name:  "piece-manifest",
	Action: func(cctx *ufcli.Context) error {
		spadeCtx := app.GetGlobalCtx(cctx.Context)
		pu := cctx.Args().Get(0)
		if pu == "" {
			return errors.New("A `proposal` UUID parameter must be supplied to this call")
		}
		if _, err := uuid.Parse(pu); err != nil {
			return fmt.Errorf("The supplied `proposal` parameter '%s' is not a valid UUID: %w",
				pu,
				err,
			)
		}

		pcs := make([]struct {
			AggLog2Size int    `db:"agg_log2size"`
			AggPCidV1   string `db:"agg_pcid_v1"`
			SegPCidV2   string `db:"seg_pcid_v2"`
			UrlTemplate string
		}, 0, 8<<10)

		if err := pgxscan.Select(
			cctx.Context,
			spadeCtx.Db[app.DbMain],
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
		-- ordering is critical
		ORDER BY ps.position
		`,
			pu,
		); err != nil {
			return cmn.WrErr(err)
		}

		if len(pcs) == 0 {
			return fmt.Errorf("no results for proposal UUID '%s': either it does not exist or is too recent, or is not segmented",
				pu,
			)
		}

		utText := pcs[0].UrlTemplate
		if utText == "" {
			return xerrors.New("do not know how to handle segments without a URL template yet...")
		}
		ut, err := template.New("url").Parse(utText)
		if err != nil {
			return cmn.WrErr(err)
		}

		aggCP, err := fil.CommPFromPieceInfo(filabi.PieceInfo{
			Size:     1 << pcs[0].AggLog2Size,
			PieceCID: cid.MustParse(pcs[0].AggPCidV1),
		})
		if err != nil {
			return cmn.WrErr(err)
		}

		resp := apitypes.ResponsePieceManifestFR58{
			AggPCidV2: aggCP.PCidV2().String(),
			Segments:  make([]apitypes.Segment, len(pcs)),
		}

		for i := range pcs {
			u := new(bytes.Buffer)
			if err := ut.Execute(u, pcs[i]); err != nil {
				return cmn.WrErr(err)
			}
			resp.Segments[i].PCidV2 = pcs[i].SegPCidV2
			resp.Segments[i].Sources = []apitypes.Source{{URL: u.String()}}
		}

		jb, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return cmn.WrErr(err)
		}
		fmt.Println(string(jb))
		return nil
	},
}
