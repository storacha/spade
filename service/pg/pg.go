package pg

import (
	"context"

	"github.com/georgysavva/scany/pgxscan"
	"github.com/jackc/pgx/v4"
	"github.com/storacha/spade/internal/app"
	"github.com/storacha/spade/service"
)

type PgClient interface {
	pgxscan.Querier
	QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row
	BeginFunc(ctx context.Context, f func(pgx.Tx) error) error
}

type PgLotusSpadeService struct {
	db       PgClient
	lotusAPI service.SpadeLotusClient
	lookback uint
}

type PgSpadeServiceOption func(*PgLotusSpadeService)

// WithLookback sets the number of lookback epochs.
func WithLookback(lookback uint) PgSpadeServiceOption {
	return func(s *PgLotusSpadeService) {
		s.lookback = lookback
	}
}

// New creates a new Spade service backed by Postgres and Lotus.
func New(db PgClient, lotusAPI service.SpadeLotusClient, opts ...PgSpadeServiceOption) *PgLotusSpadeService {
	s := &PgLotusSpadeService{db: db, lotusAPI: lotusAPI, lookback: uint(app.FilDefaultLookback)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ service.SpadeService = (*PgLotusSpadeService)(nil)
