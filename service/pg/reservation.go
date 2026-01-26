package pg

import (
	"context"
	"math/bits"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"github.com/dgraph-io/ristretto"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filbig "github.com/filecoin-project/go-state-types/big"
	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	filmarket "github.com/filecoin-project/go-state-types/builtin/v9/market"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/internal/app"
	"github.com/storacha/spade/service"
)

const requestPieceLockStatement = `SELECT PG_ADVISORY_XACT_LOCK( 1234567890111 )`

func (p *PgSpadeService) ReservePiece(ctx context.Context, sp fil.ActorID, spInfo apitypes.SPInfo, piece cid.Cid, options ...service.ReservePieceOption) ([]apitypes.TenantReplicationState, error) {
	cfg := service.ReservePieceConfig{}
	for _, opt := range options {
		opt(&cfg)
	}

	err := spIneligibleErr(ctx, p.db, p.filClient, sp, p.lookback)
	if err != nil {
		return nil, err
	}

	var replStates []apitypes.TenantReplicationState
	if err := p.db.BeginFunc(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, requestPieceLockStatement)
		if err != nil {
			return err
		}

		type tenantEligible struct {
			apitypes.TenantReplicationState
			IsExclusive         bool         `db:"exclusive_replication"`
			TenantClientID      *fil.ActorID `db:"client_id_to_use"`
			TenantClientAddress *string      `db:"client_address_to_use"`

			PieceID        int64
			PieceSizeBytes int64

			DealDurationDays       int16
			StartWithinHours       int16
			RecentlyUsedStartEpoch *int64

			TenantMeta []byte
		}

		tenantsEligible := make([]tenantEligible, 0, 8)

		if err := pgxscan.Select(
			ctx,
			tx,
			&tenantsEligible,
			`
			SELECT
					*
				FROM spd.piece_realtime_eligibility( $1, $2 )
			WHERE
				( 0 = $3 OR tenant_id = $3)
			`,
			sp,
			piece,
			cfg.TenantID,
		); err != nil {
			return err
		}

		if len(tenantsEligible) == 0 {
			return service.ErrUnclaimedPiece
		}

		if tenantsEligible[0].PieceSizeBytes > 1<<spInfo.SectorLog2Size {
			return service.ErrOversizedPiece
		}

		// count ineligibles, assemble actual return
		var countNoDataCap, countAlreadyDealt, countOverReplicated, countOverPending int
		var chosenTenant *tenantEligible
		replStates = make([]apitypes.TenantReplicationState, len(tenantsEligible))
		for i, te := range tenantsEligible {
			if te.TenantClientID != nil {
				s := te.TenantClientID.String()
				te.TenantReplicationState.TenantClient = &s
			}
			replStates[i] = te.TenantReplicationState

			var invalidated bool

			if te.TenantClient == nil {
				countNoDataCap++
				invalidated = true
			}
			if te.DealAlreadyExists {
				countAlreadyDealt++
				invalidated = true
			}
			if te.Total >= te.MaxTotal ||
				te.InOrg >= te.MaxOrg ||
				te.InCity >= te.MaxCity ||
				te.InCountry >= te.MaxCountry ||
				te.InContinent >= te.MaxContinent {
				countOverReplicated++
				invalidated = true
			}
			if te.SpInFlightBytes+te.PieceSizeBytes > te.MaxInFlightBytes {
				countOverPending++
				invalidated = true
			}

			if !invalidated && chosenTenant == nil {
				chosenTenant = &te
			}
		}

		// handle "no takers" here, for ease of reading further down
		// this is slightly convoluted since we can have a "mixed error condition" - handled in the default:
		if chosenTenant == nil {
			switch len(tenantsEligible) {
			case countAlreadyDealt:
				return service.ErrProviderHasReplica
			case countNoDataCap:
				return service.ErrTenantsOutOfDatacap
			case countOverReplicated:
				return service.ErrTooManyReplicas
			case countOverPending:
				return service.ErrProviderAboveMaxInFlight
			default:
				return service.ErrReplicationRulesViolation
			}
		}

		if cfg.TenantPolicy != app.TEMPPolicies[chosenTenant.TenantID] {
			return service.ErrTenantPolicyMismatch
		}

		//
		// Here, at the very end, is where we would make a tightly-timeboxed outbound call
		// to check for potential external eligibility criteria
		// Then either return ErrExternalReservationRefused or proceed below.
		//
		// We *DO* always check using our own replication rules first, and keep a lock for the duration
		// ( in order to maintain a uniform "decency floor" among our esteemed SPs ;)
		//

		// We got that far - let's do it!
		startEpoch := fil.ClockMainnet.TimeToEpoch(time.Now().Add(
			time.Hour * time.Duration(chosenTenant.StartWithinHours),
		))

		// a lot of this logic is broken / needs to be replaced by something saner. But... in another life.
		if chosenTenant.RecentlyUsedStartEpoch != nil {
			startEpoch = filabi.ChainEpoch(*chosenTenant.RecentlyUsedStartEpoch)
		}

		// round the epoch down to a day boundary
		// we *must* work with startEpoch/StartWithinHours to produce identical retry-deals
		// 2h +/- because network started at 22:00 UTC
		rde := ((startEpoch-app.FilDefaultLookback-(filbuiltin.EpochsInHour*filabi.ChainEpoch(chosenTenant.StartWithinHours))-240)/2880)*2880 + 240

		// this is relatively expensive to do within the txn lock
		// however we cache it and call it exactly once per day, so we should be fine
		gbpce, err := service.ProviderCollateralEstimateGiB(
			ctx, rde,
		)
		if err != nil {
			return cmn.WrErr(err)
		}

		encodedLabel, err := filmarket.NewLabelFromString(piece.String())
		if err != nil {
			return cmn.WrErr(err)
		}

		prop := struct {
			ProposalV0 filmarket.DealProposal `json:"filmarket_proposal"`
		}{
			ProposalV0: filmarket.DealProposal{
				// do not change under any circumstances: even when payments eventually happen, they will happen explicitly out of band
				// ( a notable exception here would be contract-listener style interactions, but that's way off )
				StoragePricePerEpoch: filbig.Zero(), // DO NOT CHANGE

				VerifiedDeal: true,
				PieceCID:     piece,
				PieceSize:    filabi.PaddedPieceSize(chosenTenant.PieceSizeBytes),

				Provider: sp.AsFilAddr(),
				Client:   chosenTenant.TenantClientID.AsFilAddr(),

				StartEpoch: startEpoch,
				EndEpoch:   startEpoch + filabi.ChainEpoch(chosenTenant.DealDurationDays)*filbuiltin.EpochsInDay,
				Label:      encodedLabel,

				ClientCollateral: filbig.Zero(),
				ProviderCollateral: filbig.Rsh(
					filbig.Mul(gbpce, filbig.NewInt(chosenTenant.PieceSizeBytes)),
					30,
				),
			},
		}

		// inherit the request uuid as the proposal uuid (a uuid is a uuid is a uuid)
		proposalID := cfg.RequestID
		if proposalID == uuid.Nil {
			newUUID, err := uuid.NewRandom()
			if err != nil {
				return err
			}
			proposalID = newUUID
		}

		if _, err := tx.Exec(
			ctx,
			`
			INSERT INTO spd.proposals
				( proposal_uuid, piece_id, provider_id, client_id, start_epoch, end_epoch, proxied_log2_size, proposal_meta )
			VALUES ( $1, $2, $3, $4, $5, $6, $7, $8 )
			`,
			proposalID.String(),
			chosenTenant.PieceID,
			sp,
			*chosenTenant.TenantClientID,
			prop.ProposalV0.StartEpoch,
			prop.ProposalV0.EndEpoch,
			bits.TrailingZeros64(uint64(chosenTenant.PieceSizeBytes)),
			prop,
		); err != nil {
			return err
		}

		// we managed - bump the counts where applicable and return stats
		for i := range tenantsEligible {
			if tenantsEligible[i].IsExclusive && replStates[i].TenantID != chosenTenant.TenantID {
				continue
			}
			replStates[i].Total++
			replStates[i].InOrg++
			replStates[i].InCity++
			replStates[i].InCountry++
			replStates[i].InContinent++
			replStates[i].DealAlreadyExists = true
			replStates[i].SpInFlightBytes += chosenTenant.PieceSizeBytes
		}

		return nil
	}); err != nil {
		return replStates, err
	}

	return replStates, nil
}

// using ristretto here because of SetWithTTL() below
var providerEligibleCache, _ = ristretto.NewCache(&ristretto.Config{
	NumCounters: 1e7, BufferItems: 64,
	MaxCost: 1024,
	Cost:    func(interface{}) int64 { return 1 },
})

func spIneligibleErr(ctx context.Context, db PgClient, filClient service.ReservationFilecoinClient, spID fil.ActorID, lookback uint) (defErr error) {
	// do not cache chain-independent factors
	var ignoreChainEligibility bool
	err := db.QueryRow(
		ctx,
		`
		SELECT COALESCE( ( provider_meta->'ignore_chain_eligibility' )::BOOL, false )
			FROM spd.providers
		WHERE
			NOT COALESCE( ( provider_meta->'globally_inactivated' )::BOOL, false )
				AND
			provider_id = $1
		`,
		spID,
	).Scan(&ignoreChainEligibility)
	if err == pgx.ErrNoRows {
		return service.ErrStorageProviderSuspended
	} else if err != nil {
		return err
	} else if ignoreChainEligibility {
		return nil
	}

	defer func() {
		if defErr != nil {
			providerEligibleCache.Del(uint64(spID))
			defIneligibleCode = 0
		} else {
			providerEligibleCache.SetWithTTL(uint64(spID), defErr, 1, time.Minute)
		}
	}()

	if protoReason, found := providerEligibleCache.Get(uint64(spID)); found {
		return protoReason.(error)
	}

	curTipset, err := service.GetTipset(ctx, filClient, lookback)
	if err != nil {
		return err
	}

	mbi, err := filClient.MinerGetBaseInfo(ctx, spID.AsFilAddr(), curTipset.Height(), curTipset.Key())
	if err != nil {
		return err
	}
	if mbi == nil || !mbi.EligibleForMining {
		return service.ErrStorageProviderIneligibleToMine
	}

	return nil
}
