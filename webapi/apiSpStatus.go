package main

import (
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
)

func apiSpStatus(c echo.Context) error {
	_, ctxMeta := unpackAuthedEchoContext(c)

	return retFail(
		c,
		apitypes.ErrSystemTemporarilyDisabled,
		`
		Auth successful, your SP is authorized for Spade and your signature is valid.
		Storage Provider: %s
    `,
		ctxMeta.authedActorID.String(),
	)
}
