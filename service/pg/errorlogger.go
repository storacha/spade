package pg

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/storacha/spade/apitypes"
)

func (s *PgLotusSpadeService) RequestError(ctx context.Context, requestID uuid.UUID, code apitypes.APIErrorCode, message string, payload any) error {
	jPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
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
		message,
		code,
		code.String(),
		jPayload,
		requestID.String(),
	)
	return err
}
