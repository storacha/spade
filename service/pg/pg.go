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

type PgSpadeService struct {
	db        PgClient
	filClient service.SpadeFilecoinClient
	lookback  uint
}

type PgSpadeServiceOption func(*PgSpadeService)

// WithLookback sets the number of lookback epochs.
func WithLookback(lookback uint) PgSpadeServiceOption {
	return func(s *PgSpadeService) {
		s.lookback = lookback
	}
}

// New creates a new Spade service backed by Postgres.
func New(db PgClient, filClient service.SpadeFilecoinClient, opts ...PgSpadeServiceOption) *PgSpadeService {
	s := &PgSpadeService{db: db, filClient: filClient, lookback: uint(app.FilDefaultLookback)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ service.SpadeService = (*PgSpadeService)(nil)
