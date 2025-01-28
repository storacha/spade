package main

import (
	"encoding/json"
	"sort"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"code.riba.cloud/go/toolbox/ufcli"
	filabi "github.com/filecoin-project/go-state-types/abi"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/storacha/spade/internal/app"
)

var keepCacheDays = 10

var updateCurrentF05CollateralEstimate = &ufcli.Command{
	Usage: "Update f05 collateral midnight UTC estimate",
	Name:  "update-f05-mincollateral",
	Flags: []ufcli.Flag{},
	Action: func(cctx *ufcli.Context) error {
		ctx, _, db, _ := app.UnpackCtx(cctx.Context)

		collateralGiB := make(map[filabi.ChainEpoch]filabi.TokenAmount, keepCacheDays)
		{
			var collateralGiBEnc []string
			if err := pgxscan.Select(
				ctx,
				db,
				&collateralGiBEnc,
				`SELECT metadata->'legacy_f05_mincollateral' FROM spd.global`,
			); err != nil {
				return cmn.WrErr(err)
			}

			if len(collateralGiBEnc) != 0 {
				if err := json.Unmarshal([]byte(collateralGiBEnc[0]), &collateralGiB); err != nil {
					return cmn.WrErr(err)
				}
			}
		}

		epochs := make([]filabi.ChainEpoch, 0, len(collateralGiB))
		for e := range collateralGiB {
			epochs = append(epochs, e)
		}
		sort.Slice(epochs, func(i, j int) bool {
			return epochs[i] < epochs[j]
		})

		curMidnightEpoch := ((fil.ClockMainnet.TimeToEpoch(time.Now())-240)/2880)*2880 + 240

		if len(epochs) > 0 && epochs[len(epochs)-1] == curMidnightEpoch {
			// nothing to do
			return nil
		}

		// trim the map, leaving one slot for the API call we are about to run
		for len(epochs) >= keepCacheDays {
			delete(collateralGiB, epochs[0])
			epochs = epochs[1:]
		}

		var err error
		collateralGiB[curMidnightEpoch], err = app.EpochMinProviderCollateralEstimateGiB(ctx, curMidnightEpoch)
		if err != nil {
			return cmn.WrErr(err)
		}

		_, err = db.Exec(
			ctx,
			`UPDATE spd.global SET metadata = JSONB_SET( metadata, '{ legacy_f05_mincollateral }', $1 )`,
			collateralGiB,
		)
		return cmn.WrErr(err)
	},
}
