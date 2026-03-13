package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"code.riba.cloud/go/toolbox/cmn"
	"github.com/ipfs/go-cid"
	"github.com/labstack/echo/v4"
	"github.com/multiformats/go-multihash"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/internal/app"
	"github.com/storacha/spade/internal/filtypes"
	"github.com/storacha/spade/service"
)

func NewSpInvokeHandler(svc service.ReservationService) echo.HandlerFunc {
	return func(c echo.Context) error {
		return apiSpInvoke(c, svc)
	}
}

func apiSpInvoke(c echo.Context, svc service.ReservationService) (defErr error) {
	ctx, ctxMeta := unpackAuthedEchoContext(c)

	// return retFail(
	// 	c,
	// 	apitypes.ErrSystemTemporarilyDisabled,
	// 	"Operations temporarily suspended",
	// )

	if argCall := ctxMeta.signedArgs.Get("call"); argCall != "reserve_piece" {
		return retFail(
			c,
			svc,
			apitypes.ErrInvalidRequest,
			"Unrecognized call '%s'",
			argCall,
		)
	}

	pCidArg := ctxMeta.signedArgs.Get("piece_cid")
	pCid, err := cid.Parse(pCidArg)
	if err != nil {
		return retFail(c, svc, apitypes.ErrInvalidRequest, "Requested PieceCid '%s' is not valid: %s", pCidArg, err)
	}
	if pCid.Prefix().Codec != cid.FilCommitmentUnsealed || pCid.Prefix().MhType != multihash.SHA2_256_TRUNC254_PADDED {
		return retFail(
			c,
			svc,
			apitypes.ErrInvalidRequest,
			"Requested PieceCID '%s' does not have expected codec (%x) and multihash (%x)",
			pCid,
			cid.FilCommitmentUnsealed,
			multihash.SHA2_256_TRUNC254_PADDED,
		)
	}

	tenantID := int16(0) // 0 == any
	if c.QueryParams().Has("tenant") {
		tid, err := parseUIntQueryParam(c, "tenant", 1, 1<<15)
		if err != nil {
			return retFail(c, svc, apitypes.ErrInvalidRequest, err.Error())
		}
		tenantID = int16(tid)
	}

	// check whether the provider has been polled
	if ctxMeta.spInfoLastPolled == nil ||
		ctxMeta.spInfoLastPolled.Before(time.Now().Add(-1*app.PolledSPInfoStaleAfterMinutes*time.Minute)) {
		return retFail(
			c,
			svc,
			apitypes.ErrStorageProviderInfoTooOld,
			"Provider has not been dialed by the polling system recently: please try again in about a minute",
		)
	}

	// check whether dialable at all
	if ctxMeta.spInfo.PeerInfo == nil || len(ctxMeta.spInfo.PeerInfo.Protos) == 0 {
		return retFail(
			c,
			svc,
			apitypes.ErrStorageProviderUndialable,
			strings.Join([]string{
				"It appears your provider can not be libp2p-dialed over the TCP transport.",
				"Please invoke the status endpoint for further details:",
				curlAuthedForSP(c, ctxMeta.authedActorID, "/sp/status", nil),
			}, "\n"),
		)
	}

	// only boost
	if _, canV120 := ctxMeta.spInfo.PeerInfo.Protos[filtypes.StorageProposalV120]; !canV120 {
		return retFail(
			c,
			svc,
			apitypes.ErrStorageProviderUnsupported,
			strings.Join([]string{
				"It appears your provider does not support %s.",
				"You must upgrade to Boost v1.5.1 or equivalent to use ♠️",
			}, "\n"),
			filtypes.StorageProposalV120,
		)
	}

	replStates, err := svc.ReservePiece(
		ctx,
		ctxMeta.authedActorID,
		ctxMeta.spInfo,
		pCid,
		service.WithReservePieceRequestID(ctxMeta.requestID),
		service.WithReservePieceTenantID(tenantID),
		service.WithReservePieceTenantPolicy(ctxMeta.signedArgs.Get("tenant_policy")),
	)
	if err != nil {
		if errors.Is(err, service.ErrStorageProviderSuspended) {
			return retFail(c, svc, apitypes.ErrStorageProviderSuspended, ineligibleSpMsg(ctxMeta.authedActorID))
		}
		if errors.Is(err, service.ErrStorageProviderIneligibleToMine) {
			return retFail(c, svc, apitypes.ErrStorageProviderIneligibleToMine, ineligibleSpMsg(ctxMeta.authedActorID))
		}
		if errors.Is(err, service.ErrUnclaimedPiece) {
			return retFail(c, svc, apitypes.ErrUnclaimedPieceCID, "Piece %s is not claimed by any tenant", pCid)
		}
		if errors.Is(err, service.ErrOversizedPiece) {
			return retFail(c, svc, apitypes.ErrOversizedPiece,
				"Piece %s is larger than the %d GiB sector size your SP supports",
				pCid,
				1<<(ctxMeta.spInfo.SectorLog2Size-30),
			)
		}
		if errors.Is(err, service.ErrProviderHasReplica) {
			return retPayloadAnnotated(c, svc, http.StatusForbidden,
				apitypes.ErrProviderHasReplica,
				apitypes.ResponseDealRequest{ReplicationStates: replStates},
				"Provider already has proposed or active replica for %s according to all selected replication rules", pCid,
			)
		}
		if errors.Is(err, service.ErrTenantsOutOfDatacap) {
			return retPayloadAnnotated(c, svc, http.StatusForbidden,
				apitypes.ErrTenantsOutOfDatacap,
				apitypes.ResponseDealRequest{ReplicationStates: replStates},
				"All selected tenants with claim to %s are out of DataCap 🙀", pCid,
			)
		}
		if errors.Is(err, service.ErrTooManyReplicas) {
			return retPayloadAnnotated(c, svc, http.StatusForbidden,
				apitypes.ErrProviderAboveMaxInFlight,
				apitypes.ResponseDealRequest{ReplicationStates: replStates},
				"Provider has more proposals in-flight than permitted by selected tenant rules",
			)
		}
		if errors.Is(err, service.ErrProviderAboveMaxInFlight) {
			return retPayloadAnnotated(c, svc, http.StatusForbidden,
				apitypes.ErrProviderAboveMaxInFlight,
				apitypes.ResponseDealRequest{ReplicationStates: replStates},
				"Provider has more proposals in-flight than permitted by selected tenant rules",
			)
		}
		if errors.Is(err, service.ErrReplicationRulesViolation) {
			return retPayloadAnnotated(c, svc, http.StatusForbidden,
				apitypes.ErrReplicationRulesViolation,
				apitypes.ResponseDealRequest{ReplicationStates: replStates},
				"None of the selected tenants would grant a deal for %s according to their individual rules", pCid,
			)
		}
		if errors.Is(err, service.ErrTenantPolicyMismatch) {
			if tenantID == 0 && len(replStates) > 0 {
				tenantID = replStates[0].TenantID
			}
			return retFail(
				c,
				svc,
				apitypes.ErrInvalidRequest,
				"Incorrect policy for tenant %d",
				tenantID,
			)
		}
		return cmn.WrErr(err)
	}

	return retPayloadAnnotated(
		c,
		svc,
		http.StatusOK,
		0,
		apitypes.ResponseDealRequest{
			ReplicationStates: replStates,
		},
		strings.Join([]string{
			fmt.Sprintf("Deal queued for PieceCID %s", pCid),
			``,
			`In about 5 minutes check the pending list:`,
			" " + curlAuthedForSP(c, ctxMeta.authedActorID, "/sp/pending_proposals", nil),
		}, "\n"),
	)
}
