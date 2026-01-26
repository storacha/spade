package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/internal/app"
	"golang.org/x/xerrors"
)

func truthyBoolQueryParam(c echo.Context, pname string) bool {
	if !c.QueryParams().Has(pname) {
		return false
	}
	p := strings.ToLower(c.QueryParams().Get(pname))
	if p != "0" && p != "false" && p != "no" {
		return true
	}
	return false
}

func parseUIntQueryParam(c echo.Context, pname string, min, max uint64) (uint64, error) {
	str := c.QueryParam(pname)
	val, err := strconv.ParseUint(str, 10, 64)
	if str == "" || err != nil {
		return 0, xerrors.Errorf("provided '%s' value '%s' is not a valid integer", pname, str)
	}
	if val < min || val > max {
		return 0, xerrors.Errorf("provided '%s' value '%s' is out of bounds ( %d ~ %d )", pname, str, min, max)
	}
	return val, nil
}

func retPayloadAnnotated(c echo.Context, httpCode int, errCode apitypes.APIErrorCode, payload apitypes.ResponsePayload, fmsg string, args ...interface{}) error {
	ctx, ctxMeta := unpackAuthedEchoContext(c)

	msg := fmt.Sprintf(fmsg, args...)

	var lines []string
	if msg != "" {
		lines = strings.Split(msg, "\n")
		longest := 0
		for _, l := range lines {
			encLen := len([]rune(l)) + strings.Count(l, `"`)
			if encLen > longest {
				longest = encLen
			}
		}
		for i, l := range lines {
			lines[i] = fmt.Sprintf(" %*s", -longest-1+strings.Count(l, `"`), l)
		}
	}

	r := apitypes.ResponseEnvelope{
		RequestID:          c.Request().Header.Get("X-SPADE-REQUEST-UUID"),
		ResponseStateEpoch: int64(ctxMeta.stateEpoch),
		ResponseTime:       time.Now(),
		ResponseCode:       httpCode,
		Response:           payload,
	}

	pv := reflect.ValueOf(payload)
	switch pv.Kind() {
	case reflect.Array, reflect.Slice, reflect.Map:
		l := pv.Len()
		r.ResponseEntries = &l
	}

	if httpCode < 400 {
		if errCode != 0 {
			return xerrors.Errorf("HTTP code %d incongruent with internal errCode %d", httpCode, errCode)
		}
		r.InfoLines = lines
	} else {
		r.ErrCode = int(errCode)
		r.ErrSlug = errCode.String()
		r.ErrLines = lines

		c.Response().Header().Set("X-SPADE-FAILURE-SLUG", r.ErrSlug)
		c.Request().Header.Set("X-SPADE-FAILURE-SLUG", r.ErrSlug) // set on *request* so that echo can log it

		if r.RequestID != "" && (msg != "" || errCode != 0) {
			jPayload, err := json.Marshal(payload)
			if err != nil {
				return cmn.WrErr(err)
			}
			if _, err := ctxMeta.Db[app.DbMain].Exec(
				ctx,
				`
				UPDATE spd.requests SET
					request_meta = JSONB_STRIP_NULLS( request_meta || JSONB_BUILD_OBJECT(
						'error', $1::TEXT,
						'error_code', $2::INTEGER,
						'error_slug', $3::TEXT,
						'payload', $4::JSONB
					) )
				WHERE
					request_uuid = $5
				`,
				msg,
				r.ErrCode,
				r.ErrSlug,
				jPayload,
				r.RequestID,
			); err != nil {
				return cmn.WrErr(err)
			}
		}
	}

	// FIXME - only prettify on curl and similar
	return c.JSONPretty(httpCode, r, "  ")
}

func curlAuthedForSP(c echo.Context, spID fil.ActorID, path string, sigArgs url.Values) string {
	prot := c.Request().Header.Get("X-Forwarded-Proto")
	if prot == "" {
		prot = "http"
	}

	var doPost, argStr string
	if sigArgs != nil {
		doPost = "-XPOST "
		argStr = fmt.Sprintf(`echo -n '%s' | `, sigArgs.Encode())

	}

	return fmt.Sprintf(
		`echo curl --compressed %s-sLH "Authorization: $( %s./fil-spid.bash %s )" %s://%s%s | sh`,
		doPost,
		argStr,
		spID,
		prot,
		c.Request().Host,
		path,
	)
}

func retFail(c echo.Context, errCode apitypes.APIErrorCode, fMsg string, args ...interface{}) error {
	return retPayloadAnnotated(
		c,
		http.StatusForbidden, // DO NOT use 400: we rewrite that on the nginx level to normalize a class of transport errors
		errCode,
		nil,
		fMsg, args...,
	)
}

func retAuthFail(c echo.Context, f string, args ...interface{}) error {
	c.Response().Header().Set(echo.HeaderWWWAuthenticate, authScheme)
	return retPayloadAnnotated(
		c,
		http.StatusUnauthorized,
		apitypes.ErrUnauthorizedAccess,
		nil,
		echo.ErrUnauthorized.Error()+"\n\n"+f,
		args...,
	)
}

func retInvalidRoute(c echo.Context) error {
	return retFail(
		c,
		apitypes.ErrInvalidRequest,
		"invalid route request: %s %s",
		c.Request().Method,
		c.Request().RequestURI,
	)
}

func ineligibleSpMsg(spID fil.ActorID) string {
	return fmt.Sprintf(
		`
At the time of this request Storage provider %s is not eligible to use this API
( this state is is almost certainly *temporary* )

Make sure that you:
- Have registered your SP in accordance with each individual tenant
- Are continuing to serve previously onboarded datasets reliably and free of charge
- Have sufficient quality-adjusted power to participate in block rewards
- Have not faulted in the past 48h

If the problem persists, or you believe this is a spurious error: please contact the API
administrators in #♠-spade-sp-♠ over at the Storacha Discord https://discord.gg/pqa6Dn6RnP.
( direct link: https://discord.com/channels/1247475892435816553/1365086771347587072 )
`,
		spID,
	)
}
