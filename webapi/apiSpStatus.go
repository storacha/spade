package main

import (
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/service"
)

func NewSpStatusHandler(svc service.StatusService) echo.HandlerFunc {
	return func(c echo.Context) error {
		return apiSpStatus(c, svc)
	}
}

func apiSpStatus(c echo.Context, svc service.StatusService) error {
	_, ctxMeta := unpackAuthedEchoContext(c)

	return retFail(
		c,
		svc,
		apitypes.ErrSystemTemporarilyDisabled,
		`
		Auth successful, your SP is authorized for Spade and your signature is valid.
		Storage Provider: %s
    `,
		ctxMeta.authedActorID.String(),
	)
}
