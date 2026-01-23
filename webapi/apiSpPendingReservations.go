package main

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	filabi "github.com/filecoin-project/go-state-types/abi"
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
)

func NewSpListPendingProposalsHandler(service Service) echo.HandlerFunc {
	return func(c echo.Context) error {
		return apiSpListPendingProposals(c, service)
	}
}

func apiSpListPendingProposals(c echo.Context, service Service) error {
	ctx, ctxMeta := unpackAuthedEchoContext(c)

	pending, err := service.PendingProposals(
		ctx,
		ctxMeta.authedActorID,
	)
	if err != nil {
		return cmn.WrErr(err)
	}

	type dealTuple struct {
		pieceID  int64
		tenantID int16
	}

	var toPropose, toActivate, outstandingBytes int64
	fails := make(map[dealTuple]apitypes.ProposalFailure)
	ret := apitypes.ResponsePendingProposals{
		PendingProposals: make([]apitypes.DealProposal, 0, len(pending)),
	}

	for _, p := range pending {
		outstandingBytes += (1 << p.PieceLog2Size)

		switch {

		case p.IsPublished:
			toActivate++

		case p.ProposalFailstamp > 0:
			outstandingBytes -= (1 << p.PieceLog2Size) // take it back
			t := dealTuple{pieceID: p.PieceID, tenantID: p.TenantID}
			f := apitypes.ProposalFailure{
				ErrorTimeStamp: time.Unix(0, p.ProposalFailstamp),
				Error:          *p.Error,
				PieceCid:       p.PieceCid,
				ProposalID:     p.ProposalID,
				ProposalCid:    p.ProposalCid,
				TenantID:       p.TenantID,
				TenantClient:   p.ClientID.String(),
			}
			prev, seen := fails[t]
			if !seen || prev.ErrorTimeStamp.Before(f.ErrorTimeStamp) {
				fails[t] = f
			}

		case p.ProposalDelivered == nil:
			toPropose++

		default:
			dp := p.DealProposal
			dp.StartTime = fil.ClockMainnet.EpochToTime(filabi.ChainEpoch(dp.StartEpoch))
			dp.HoursRemaining = int(time.Until(dp.StartTime).Truncate(time.Hour).Hours())
			dp.PieceSize = 1 << p.PieceLog2Size
			dp.TenantClient = p.ClientID.String()
			// should never be nil but be cautious
			if dp.ProposalID != "" {
				dp.ImportCmd = fmt.Sprintf("boostd import-data --delete-after-import %s {{downloaded_or_assembled_file}}",
					dp.ProposalID,
				)
			}
			// when segmented
			if dp.Segmentation != nil && *dp.Segmentation == "frc58" {
				ass := curlAuthedForSP(c, ctxMeta.authedActorID, "/sp/piece_manifest?proposal="+dp.ProposalID, nil) + " | jq .response | fil-datasegment from-manifest"
				dp.AssemblyCmd = &ass
			}

			ret.PendingProposals = append(ret.PendingProposals, dp)
		}
	}

	msg := fmt.Sprintf(
		`
	This is an overview of deals recently proposed to SP %s

	There currently are %0.2f GiB of pending deals:
		% 4d deal-proposals to send out
		% 4d successful proposals pending publishing
		% 4d deals published on chain awaiting sector activation

	You can request deal proposals using API endpoints as described in the docs`,
		ctxMeta.authedActorID,
		float64(outstandingBytes)/(1<<30),
		toPropose,
		len(ret.PendingProposals),
		toActivate,
	)

	if len(fails) > 0 {
		msg += fmt.Sprintf("\n\nIn the past %dh there were %d proposal errors, shown in recent_failures below.", showRecentFailuresHours, len(fails))

		ret.RecentFailures = make([]apitypes.ProposalFailure, 0, len(fails))
		for _, f := range fails {
			ret.RecentFailures = append(ret.RecentFailures, f)
		}
		sort.Slice(ret.RecentFailures, func(i, j int) bool {
			return ret.RecentFailures[j].ErrorTimeStamp.Before(ret.RecentFailures[i].ErrorTimeStamp)
		})
	}

	return retPayloadAnnotated(
		c,
		http.StatusOK,
		0,
		ret,
		msg,
	)
}
