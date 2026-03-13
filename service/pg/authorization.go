package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"github.com/google/uuid"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/service"
	"github.com/storacha/spade/spid"
	spid_lotus "github.com/storacha/spade/spid/lotus"
)

func (p *PgLotusSpadeService) Authorize(ctx context.Context, req service.Request) (service.Authorization, error) {
	challenge, err := spid.Parse(req.Headers.Get("Authorization"))
	if err != nil {
		return service.Authorization{}, fmt.Errorf("parsing authorization header: %w", err)
	}

	err = spid_lotus.ResolveAndVerify(ctx, p.lotusAPI, challenge)
	if err != nil {
		return service.Authorization{}, fmt.Errorf("verifying authorization signature: %w", err)
	}

	sp := fil.MustParseActorString(challenge.Addr().String())
	signedArgs, err := challenge.Args().Values()
	if err != nil {
		return service.Authorization{}, fmt.Errorf("getting signed args values: %w", err)
	}

	reqJ, err := json.Marshal(
		struct {
			Method       string
			Host         string
			Path         string
			Params       string
			ParamsSigned url.Values
			Headers      http.Header
		}{
			Method:       req.Method,
			Host:         req.Host,
			Path:         req.Path,
			Params:       req.Params.Encode(),
			ParamsSigned: signedArgs,
			Headers:      req.Headers,
		},
	)
	if err != nil {
		return service.Authorization{}, err
	}

	spDetails := [4]int16{-1, -1, -1, -1}
	var requestUUID string
	var stateEpoch int64
	var spInfo apitypes.SPInfo
	var spInfoLastPoll *time.Time
	if err := p.db.QueryRow(
		ctx,
		`
		INSERT INTO spd.requests ( provider_id, request_dump )
			VALUES ( $1, $2 )
		RETURNING
			request_uuid,
			( SELECT ( metadata->'market_state'->'epoch' )::INTEGER FROM spd.global ),
			COALESCE(	(
				SELECT
					ARRAY[
						COALESCE( org_id, -1 ),
						COALESCE( city_id, -1),
						COALESCE( country_id, -1),
						COALESCE( continent_id, -1)
					]
				FROM spd.providers
				WHERE provider_id = $1
				LIMIT 1
			), ARRAY[-1, -1, -1, -1] ),
			(
				SELECT info
					FROM spd.providers_info
				WHERE provider_id = $1
			),
			(
				SELECT provider_last_polled
					FROM spd.providers_info
				WHERE provider_id = $1
			)
		`,
		sp,
		reqJ,
	).Scan(&requestUUID, &stateEpoch, &spDetails, &spInfo, &spInfoLastPoll); err != nil {
		return service.Authorization{}, err
	}

	reqID, err := uuid.Parse(requestUUID)
	if err != nil {
		return service.Authorization{}, fmt.Errorf("parsing UUID: %w", err)
	}

	return service.Authorization{
		RequestID:       reqID,
		StateEpoch:      stateEpoch,
		SignedArgs:      signedArgs,
		ProviderID:      sp,
		ProviderDetails: spDetails,
		ProviderInfo:    spInfo,
		LastPoll:        spInfoLastPoll,
	}, nil
}
