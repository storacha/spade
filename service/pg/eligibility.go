package pg

import (
	"context"
	"fmt"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/storacha/spade/service"
)

func (p *PgLotusSpadeService) EligiblePieces(ctx context.Context, sp fil.ActorID, options ...service.EligiblePiecesOption) ([]service.EligiblePiece, bool, error) {
	cfg := service.EligiblePiecesConfig{Limit: service.ListEligibleDefaultSize}
	for _, opt := range options {
		opt(&cfg)
	}

	lim := cfg.Limit
	tenantID := cfg.TenantID
	// how to list: start small, find setting below
	useQueryFunc := "pieces_eligible_head"
	if lim > service.ListEligibleDefaultSize { // deduce from requested lim
		useQueryFunc = "pieces_eligible_full"
	}

	orderedPieces := make([]service.EligiblePiece, 0, lim+1)
	if err := pgxscan.Select(
		ctx,
		p.db,
		&orderedPieces,
		fmt.Sprintf("SELECT * FROM spd.%s( $1, $2, $3, $4, $5 )", useQueryFunc),
		sp,
		lim+1, // ask for one extra, to disambiguate "there is more"
		tenantID,
		cfg.IncludeSourceless,
		false,
	); err != nil {
		return nil, false, err
	}

	var more bool
	if uint64(len(orderedPieces)) > lim {
		orderedPieces = orderedPieces[:lim]
		more = true
	}
	return orderedPieces, more, nil
}
