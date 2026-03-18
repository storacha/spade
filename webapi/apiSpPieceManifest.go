package webapi

import (
	"bytes"
	"errors"
	"net/http"
	"text/template"

	"code.riba.cloud/go/toolbox/cmn"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/service"
)

type TemplateParams struct {
	SegPCidV2 string
}

func NewSpPieceManifestHandler(svc service.PieceManifestService) echo.HandlerFunc {
	return func(c echo.Context) error {
		return apiSpPieceManifest(c, svc)
	}
}

func apiSpPieceManifest(c echo.Context, svc service.PieceManifestService) error {
	ctx, ctxMeta := unpackAuthedEchoContext(c)

	ps := c.QueryParams().Get("proposal")
	if ps == "" {
		return retFail(
			c,
			svc,
			apitypes.ErrInvalidRequest,
			"A `proposal` UUID parameter must be supplied to this call",
		)
	}
	pu, err := uuid.Parse(ps)
	if err != nil {
		return retFail(
			c,
			svc,
			apitypes.ErrInvalidRequest,
			"The supplied `proposal` parameter '%s' is not a valid UUID: %s",
			ps,
			err,
		)
	}

	manifest, err := svc.PieceManifest(ctx, ctxMeta.authedActorID, pu)
	if err != nil {
		if errors.Is(err, service.ErrManifestNotFound) {
			return retFail(
				c,
				svc,
				apitypes.ErrInvalidRequest,
				"no results for proposal UUID '%s': either it does not exist, is too recent, does not belong to %s or is not segmented",
				ps,
				ctxMeta.authedActorID.AsFilAddr().String(),
			)
		}
		return cmn.WrErr(err)
	}

	utText := manifest.UrlTemplate
	if utText == "" {
		return errors.New("do not know how to handle segments without a URL template yet...")
	}
	ut, err := template.New("url").Parse(utText)
	if err != nil {
		return cmn.WrErr(err)
	}

	resp := apitypes.ResponsePieceManifestFR58{
		AggPCidV2: manifest.PieceCid.String(),
		Segments:  make([]apitypes.Segment, len(manifest.SegmentCids)),
	}

	for i, s := range manifest.SegmentCids {
		u := new(bytes.Buffer)
		if err := ut.Execute(u, TemplateParams{SegPCidV2: s.String()}); err != nil {
			return cmn.WrErr(err)
		}
		resp.Segments[i].PCidV2 = s.String()
		// TODO: at this point we need to get HTTP headers to include for each
		// segment. We should be able to lookup piece -> node DID+URL and obtain a
		// delegation from them, allowing transfer of the blobs. We then create a
		// blob/retrieve invocation for each segment, and include the resulting
		// URL+headers here. For now we just include the URL without any auth,
		// which works for the public network but not the private one.
		//
		// The following issues will address this TODO:
		// * https://github.com/storacha/spade/issues/16
		// * https://github.com/storacha/spade/issues/17
		resp.Segments[i].Sources = []apitypes.Source{{URL: u.String()}}
	}

	return retPayloadAnnotated(
		c,
		svc,
		http.StatusOK,
		0,
		resp,
		"",
	)
}
