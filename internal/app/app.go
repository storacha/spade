package app

import (
	"context"
	"fmt"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"code.riba.cloud/go/toolbox/ufcli"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filbig "github.com/filecoin-project/go-state-types/big"
	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
)

var TEMPPolicies = map[int16]string{
	13: "bafkreihuqkipjv2sgc3ypr5lcervqitht2m5f6iyr4g432mpqwzmfm7jtq",
}

const (
	AppName                       = "spade"
	FilDefaultLookback            = filabi.ChainEpoch(10)
	PolledSPInfoStaleAfterMinutes = 15
)

const (
	DbMain = dbtype(iota)
)

type (
	dbtype        int
	DbConns       map[dbtype]*pgxpool.Pool
	GlobalContext struct {
		Db       DbConns
		LotusAPI fil.LotusDaemonAPIClientV0
		Logger   ufcli.Logger
	}
	ctxKey string
)

var ck = ctxKey("♠️")

func GetGlobalCtx(ctx context.Context) GlobalContext {
	return ctx.Value(ck).(GlobalContext)
}

func UnpackCtx(ctx context.Context) (
	origCtx context.Context,
	logger ufcli.Logger,
	mainDB *pgxpool.Pool,
	globalCtx GlobalContext,
) {
	gctx := GetGlobalCtx(ctx)
	return ctx, gctx.Logger, gctx.Db[DbMain], gctx
}

var lotusLookbackEpochs uint

func DefaultLookbackTipset(ctx context.Context) (*fil.LotusTS, error) {
	return fil.GetTipset(ctx, GetGlobalCtx(ctx).LotusAPI, filabi.ChainEpoch(lotusLookbackEpochs))
}

var CommonFlags = []ufcli.Flag{
	ufcli.ConfStringFlag(&ufcli.StringFlag{
		Name:  "lotus-api",
		Value: "https://api.chain.love",
	}),
	&ufcli.UintFlag{
		Name:  "lotus-lookback-epochs",
		Value: uint(FilDefaultLookback),
		DefaultText: fmt.Sprintf("%d epochs / %ds",
			FilDefaultLookback,
			filbuiltin.EpochDurationSeconds*FilDefaultLookback,
		),
		Destination: &lotusLookbackEpochs,
	},
	ufcli.ConfStringFlag(&ufcli.StringFlag{
		Name:  "pg-connstring",
		Value: "postgres:///dbname?user=username&password=&host=/var/run/postgresql",
	}),
}

func GlobalInit(cctx *ufcli.Context, uf *ufcli.UFcli) (func() error, error) {

	gctx := GlobalContext{
		Logger: uf.Logger,
		Db:     make(DbConns, 2),
	}

	lApi, lApiCloser, err := fil.NewLotusDaemonAPIClientV0(cctx.Context, cctx.String("lotus-api"), 30, "")
	if err != nil {
		return nil, cmn.WrErr(err)
	}
	gctx.LotusAPI = lApi

	dbConnCfg, err := pgxpool.ParseConfig(cctx.String("pg-connstring"))
	if err != nil {
		return nil, cmn.WrErr(err)
	}
	// dbConnCfg.MaxConns = 42
	dbConnCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		// _, err := c.Exec(ctx, `SET search_path = spade`)
		// _, err := c.Exec(ctx, fmt.Sprintf(`SET STATEMENT_TIMEOUT = %d`, (2*time.Hour).Milliseconds()))
		// return WrErr(err)
		return nil
	}
	gctx.Db[DbMain], err = pgxpool.ConnectConfig(cctx.Context, dbConnCfg)
	if err != nil {
		return nil, cmn.WrErr(err)
	}

	cctx.Context = context.WithValue(cctx.Context, ck, gctx)

	return func() error {
		lApiCloser()
		gctx.Db[DbMain].Close()
		return nil
	}, nil
}

func RefreshMatviews(ctx context.Context, tx pgx.Tx) error {
	log := GetGlobalCtx(ctx).Logger

	// refresh matviews
	log.Info("refreshing materialized views")
	for _, mv := range []string{
		"mv_deals_prefiltered_for_repcount", "mv_orglocal_presence",
		"mv_replicas_continent", "mv_replicas_org", "mv_replicas_city", "mv_replicas_country",
		"mv_overreplicated_city", "mv_overreplicated_country", "mv_overreplicated_total", "mv_overreplicated_continent", "mv_overreplicated_org",
		"mv_pieces_availability",
	} {
		t0 := time.Now()
		if _, err := tx.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY spd.`+mv); err != nil {
			return cmn.WrErr(err)
		}
		if _, err := tx.Exec(ctx, `ANALYZE spd.`+mv); err != nil {
			return cmn.WrErr(err)
		}
		log.Infow("refreshed", "view", mv, "took_seconds", time.Since(t0).Truncate(time.Millisecond).Seconds())
	}

	return nil
}

func EpochMinProviderCollateralEstimateGiB(ctx context.Context, sourceEpoch filabi.ChainEpoch) (filabi.TokenAmount, error) {

	gctx := GetGlobalCtx(ctx)

	// check the DB and return if found
	{
		var collateralGiBStr []*int64
		if err := pgxscan.Select(
			ctx,
			gctx.Db[DbMain],
			&collateralGiBStr,
			`SELECT (metadata->'legacy_f05_mincollateral'->>$1)::BIGINT FROM spd.global`,
			sourceEpoch,
		); err != nil {
			return filbig.Zero(), cmn.WrErr(err)
		}

		if len(collateralGiBStr) != 0 && collateralGiBStr[0] != nil {
			return filabi.NewTokenAmount(*collateralGiBStr[0]), nil
		}
	}

	// not in DB - call out to the API, hopefully before public API history cutoff
	ts, err := gctx.LotusAPI.ChainGetTipSetByHeight(ctx, sourceEpoch, fil.LotusTSK{})
	if err != nil {
		return filbig.Zero(), cmn.WrErr(err)
	}

	collateralGiB, err := gctx.LotusAPI.StateDealProviderCollateralBounds(
		ctx,
		filabi.PaddedPieceSize(1<<30), // 1 GiB
		true,                          // fil+
		ts.Key(),
	)
	if err != nil {
		return filbig.Zero(), cmn.WrErr(err)
	}

	return collateralGiB.Min, nil
}
