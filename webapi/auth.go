package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filprovider "github.com/filecoin-project/go-state-types/builtin/v9/miner"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	blsgo "github.com/jsign/go-filsigner/bls"
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/internal/app"
	"github.com/storacha/spade/service"
)

const (
	sigGraceEpochs = 5 // 30 secs per epoch
	authScheme     = `FIL-SPID-V0`
)

type rawHdr struct {
	epoch  string
	addr   string
	sigB64 string
	arg    string
}
type sigChallenge struct {
	authHdr string
	addr    filaddr.Address
	epoch   int64
	arg     []byte
	hdr     rawHdr
}

type verifySigResult struct {
	invalidSigErrstr string
}

var (
	spAuthRe = regexp.MustCompile(
		`^` + authScheme + `\s+` +
			// fil epoch
			`([0-9]+)` + `\s*;\s*` +
			// spID
			`([ft]0[0-9]+)` + `\s*;\s*` +
			// signature
			`([^; ]+)` +
			// optional signed argument
			`(?:\s*\;\s*([^; ]+))?` +
			`\s*$`,
	)
	challengeCache, _ = lru.New[rawHdr, verifySigResult](sigGraceEpochs * 128)
	beaconCache, _    = lru.New[int64, *fil.LotusBeaconEntry](sigGraceEpochs * 4)
)

func NewSpIDAuthMiddleware(svc service.RequestService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return spidAuth(next, svc)
	}
}

func spidAuth(next echo.HandlerFunc, svc service.RequestService) echo.HandlerFunc {
	return func(c echo.Context) error {

		ctx, log, _, _ := app.UnpackCtx(c.Request().Context())

		// the SP portion does not accept body payloads
		if b := c.Request().Body; b != nil && c.Request().ContentLength != 0 {
			if _, err := b.Read(make([]byte, 1)); err != io.EOF {
				return retFail(c, apitypes.ErrInvalidRequest, "spid requests witn content in the HTTP body are not supported")
			}
		}

		var challenge sigChallenge
		challenge.authHdr = c.Request().Header.Get(echo.HeaderAuthorization)
		res := spAuthRe.FindStringSubmatch(challenge.authHdr)

		if len(res) == 5 {
			challenge.hdr.epoch, challenge.hdr.addr, challenge.hdr.sigB64, challenge.hdr.arg = res[1], res[2], res[3], res[4]
		} else {
			return retAuthFail(c, "invalid/unexpected %s Authorization header '%s'", authScheme, challenge.authHdr)
		}

		var err error
		challenge.addr, err = filaddr.NewFromString(challenge.hdr.addr)
		if err != nil {
			return retAuthFail(c, "unexpected %s auth address '%s'", authScheme, challenge.hdr.addr)
		}

		challenge.epoch, err = strconv.ParseInt(challenge.hdr.epoch, 10, 32)
		if err != nil {
			return retAuthFail(c, "unexpected %s auth epoch '%s'", authScheme, challenge.hdr.epoch)
		}

		curFilEpoch := int64(fil.ClockMainnet.TimeToEpoch(time.Now()))
		if curFilEpoch < challenge.epoch {
			log.Debugw("challenge from the future", "challengeEpoch", challenge.epoch, "wallEpoch", curFilEpoch)
			return retAuthFail(c, "%s auth epoch '%d' is in the future", authScheme, challenge.epoch)
		}
		if curFilEpoch-challenge.epoch > sigGraceEpochs {
			return retAuthFail(c, "%s auth epoch '%d' is too far in the past", authScheme, challenge.epoch)
		}

		challenge.arg, err = base64.StdEncoding.DecodeString(challenge.hdr.arg)
		if err != nil {
			return retAuthFail(c, "unable to decode optional argument: %s", err.Error())
		}

		signedArgs, err := url.ParseQuery(string(challenge.arg))
		if err != nil {
			return retAuthFail(c, "unable to parse optional argument: %s", err.Error())
		}

		var vsr verifySigResult
		if maybeResult, known := challengeCache.Get(challenge.hdr); known {
			vsr = maybeResult
		} else {
			vsr, err = verifySig(ctx, challenge)
			if err != nil {
				return cmn.WrErr(err)
			}
			challengeCache.Add(challenge.hdr, vsr)
		}

		if vsr.invalidSigErrstr != "" {
			return retAuthFail(c, vsr.invalidSigErrstr)
		}

		// set only on request object for logging, not part of response
		c.Request().Header.Set("X-SPADE-LOGGED-SP", challenge.addr.String())

		// if challenge.addr.String() == "f01" {
		// 	challenge.addr, _ = filaddr.NewFromString("f02")
		// }

		spID := fil.MustParseActorString(challenge.addr.String())

		reqCopy := c.Request().Clone(ctx)
		// do not need to store any IPs anywhere in the DB
		for _, strip := range []string{
			"X-Real-Ip", "X-Forwarded-For", "Cf-Connecting-Ip",
		} {
			delete(reqCopy.Header, strip)
		}

		reqState, err := svc.Request(ctx, spID, service.Request{
			Method:       reqCopy.Method,
			Host:         reqCopy.Host,
			Path:         reqCopy.URL.Path,
			Params:       reqCopy.URL.Query(),
			ParamsSigned: signedArgs,
			Headers:      reqCopy.Header,
		})
		if err != nil {
			return cmn.WrErr(err)
		}

		c.Response().Header().Set("X-SPADE-FIL-SPID", challenge.addr.String())

		// set on both request (for logging ) and response object
		c.Request().Header.Set("X-SPADE-REQUEST-UUID", reqState.RequestID.String())
		c.Response().Header().Set("X-SPADE-REQUEST-UUID", reqState.RequestID.String())

		c.Set("♠️", metaContext{
			GlobalContext:    app.GetGlobalCtx(ctx),
			requestID:        reqState.RequestID,
			stateEpoch:       reqState.StateEpoch,
			authedActorID:    spID,
			signedArgs:       signedArgs,
			spOrgID:          reqState.ProviderDetails[0],
			spCityID:         reqState.ProviderDetails[1],
			spCountryID:      reqState.ProviderDetails[2],
			spContinentID:    reqState.ProviderDetails[3],
			spInfo:           reqState.ProviderInfo,
			spInfoLastPolled: reqState.LastPoll,
		})

		return next(c)
	}
}

type metaContext struct {
	app.GlobalContext
	requestID        uuid.UUID
	authedActorID    fil.ActorID
	stateEpoch       int64
	spInfo           apitypes.SPInfo
	spInfoLastPolled *time.Time
	spOrgID          int16
	spCityID         int16
	spCountryID      int16
	spContinentID    int16
	signedArgs       url.Values
}

func unpackAuthedEchoContext(c echo.Context) (context.Context, metaContext) {
	meta, _ := c.Get("♠️").(metaContext) // ignore potential nil error on purpose
	return c.Request().Context(), meta
}

func verifySig(ctx context.Context, challenge sigChallenge) (verifySigResult, error) {

	sig, err := base64.StdEncoding.DecodeString(challenge.hdr.sigB64)
	if err != nil {
		return verifySigResult{
			invalidSigErrstr: fmt.Sprintf("unexpected %s auth signature encoding '%s'", authScheme, challenge.hdr.sigB64),
		}, nil
	}

	lAPI := app.GetGlobalCtx(ctx).LotusAPI

	be, didFind := beaconCache.Get(challenge.epoch)
	if !didFind {

		var curChallengeTs *fil.LotusTS
		var err error

		// Do it a few times because lotus is getting slower and slower to finalize 😭
		// Can't sleep too much though not to timeout the call
		// spid.bash has been adjusted with a backoff to deal with this as well
		for i := 0; i < 3; i++ {
			curChallengeTs, err = lAPI.ChainGetTipSetByHeight(ctx, filabi.ChainEpoch(challenge.epoch), fil.LotusTSK{})
			if err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}

		if err != nil {
			// do not make slow-chain a 500
			return verifySigResult{
				invalidSigErrstr: fmt.Sprintf(
					"%s signature validation failed for auth header '%s': unable to get tipset at height %d (%s): %s",
					authScheme, challenge.authHdr,
					challenge.epoch, fil.ClockMainnet.EpochToTime(filabi.ChainEpoch(challenge.epoch)),
					err,
				)}, nil
		}
		bev := curChallengeTs.Blocks()[0].BeaconEntries[len(curChallengeTs.Blocks()[0].BeaconEntries)-1]
		be = &bev

		beaconCache.Add(challenge.epoch, be)
	}

	miFinTs, err := lAPI.ChainGetTipSetByHeight(ctx, filabi.ChainEpoch(challenge.epoch)-filprovider.ChainFinality, fil.LotusTSK{})
	if err != nil {
		return verifySigResult{}, cmn.WrErr(err)
	}
	mi, err := lAPI.StateMinerInfo(ctx, challenge.addr, miFinTs.Key())
	if err != nil {
		return verifySigResult{}, cmn.WrErr(err)
	}
	workerAddr, err := lAPI.StateAccountKey(ctx, mi.Worker, miFinTs.Key())
	if err != nil {
		return verifySigResult{}, cmn.WrErr(err)
	}

	// worker keys are always BLS
	sigMatch, err := blsgo.Verify(
		workerAddr.Payload(),
		append(append([]byte{0x20, 0x20, 0x20}, be.Data...), challenge.arg...),
		[]byte(sig),
	)
	if err != nil {
		return verifySigResult{}, cmn.WrErr(err)
	}

	if !sigMatch {
		return verifySigResult{
			invalidSigErrstr: fmt.Sprintf("%s signature validation failed for auth header '%s'", authScheme, challenge.authHdr),
		}, nil
	}
	return verifySigResult{}, nil
}
