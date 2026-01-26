package main

import (
	"context"
	"io"
	"net/url"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/internal/app"
	"github.com/storacha/spade/service"
	"github.com/storacha/spade/spid"
)

func NewSpIDAuthMiddleware(svc service.AuthorizationService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return spidAuth(next, svc)
	}
}

func spidAuth(next echo.HandlerFunc, svc service.AuthorizationService) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx, _, _, _ := app.UnpackCtx(c.Request().Context())

		// the SP portion does not accept body payloads
		if b := c.Request().Body; b != nil && c.Request().ContentLength != 0 {
			if _, err := b.Read(make([]byte, 1)); err != io.EOF {
				return retFail(c, apitypes.ErrInvalidRequest, "spid requests with content in the HTTP body are not supported")
			}
		}

		reqCopy := c.Request().Clone(ctx)
		// do not need to store any IPs anywhere in the DB
		for _, strip := range []string{
			"X-Real-Ip", "X-Forwarded-For", "Cf-Connecting-Ip",
		} {
			delete(reqCopy.Header, strip)
		}

		auth, err := svc.Authorize(ctx, service.Request{
			Method:  reqCopy.Method,
			Host:    reqCopy.Host,
			Path:    reqCopy.URL.Path,
			Params:  reqCopy.URL.Query(),
			Headers: reqCopy.Header,
		})
		if err != nil {
			return retAuthFail(c, "authorizing request: %s", spid.Scheme, err.Error())
		}

		// set only on request object for logging, not part of response
		c.Request().Header.Set("X-SPADE-LOGGED-SP", auth.ProviderID.String())

		// if challenge.addr.String() == "f01" {
		// 	challenge.addr, _ = filaddr.NewFromString("f02")
		// }

		c.Response().Header().Set("X-SPADE-FIL-SPID", auth.ProviderID.String())

		// set on both request (for logging ) and response object
		c.Request().Header.Set("X-SPADE-REQUEST-UUID", auth.RequestID.String())
		c.Response().Header().Set("X-SPADE-REQUEST-UUID", auth.RequestID.String())

		c.Set("♠️", metaContext{
			GlobalContext:    app.GetGlobalCtx(ctx),
			requestID:        auth.RequestID,
			stateEpoch:       auth.StateEpoch,
			authedActorID:    auth.ProviderID,
			signedArgs:       auth.SignedArgs,
			spOrgID:          auth.ProviderDetails[0],
			spCityID:         auth.ProviderDetails[1],
			spCountryID:      auth.ProviderDetails[2],
			spContinentID:    auth.ProviderDetails[3],
			spInfo:           auth.ProviderInfo,
			spInfoLastPolled: auth.LastPoll,
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
