package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/bits"
	"os"
	"sync"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	"code.riba.cloud/go/toolbox/cmn"
	"code.riba.cloud/go/toolbox/ufcli"
	"github.com/dustin/go-humanize"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filbig "github.com/filecoin-project/go-state-types/big"
	filbuiltin "github.com/filecoin-project/go-state-types/builtin"
	lotusadt "github.com/filecoin-project/lotus/chain/actors/adt"
	lotusmarket "github.com/filecoin-project/lotus/chain/actors/builtin/market"
	"github.com/georgysavva/scany/pgxscan"
	ipldblock "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipldcbor "github.com/ipfs/go-ipld-cbor"
	"github.com/jackc/pgx/v4"
	"github.com/mattn/go-isatty"
	"github.com/storacha/spade/internal/app"
	"golang.org/x/xerrors"

	// force bundle load, needed for actors.GetActorCodeID() to work
	_ "github.com/filecoin-project/lotus/build"
)

// IMPORTANT - keep datacap that is no longer "ours" at 0
var unusableDatacap = map[string]struct{}{
	"f01151139": {},
	"f02090659": {},
	"f02759235": {},
}

type remoteBs struct {
	api fil.LotusDaemonAPIClientV0
}

func (rbs *remoteBs) Put(context.Context, ipldblock.Block) error {
	return xerrors.New("this is a readonly store")
}
func (rbs *remoteBs) Get(ctx context.Context, c cid.Cid) (ipldblock.Block, error) {
	d, err := rbs.api.ChainReadObj(ctx, c)
	if err != nil {
		return nil, cmn.WrErr(err)
	}
	return ipldblock.NewBlockWithCid(d, c)
}

var trackDeals = &ufcli.Command{
	Usage: "Track state of fil deals related to known PieceCIDs",
	Name:  "track-deals",
	Flags: []ufcli.Flag{
		ufcli.ConfStringFlag(&ufcli.StringFlag{
			Name:  "lotus-api-blockstore",
			Value: "http://localhost:1234",
		}),
	},

	Action: func(cctx *ufcli.Context) error {

		ctx, log, db, _ := app.UnpackCtx(cctx.Context)

		curTipset, err := app.DefaultLookbackTipset(ctx)
		if err != nil {
			return cmn.WrErr(err)
		}

		lApi, closer, err := fil.NewLotusDaemonAPIClientV0(cctx.Context, cctx.String("lotus-api-blockstore"), 300, "")
		if err != nil {
			return cmn.WrErr(err)
		}
		defer closer()

		mActor, err := lApi.StateGetActor(ctx, filbuiltin.StorageMarketActorAddr, curTipset.Key())
		if err != nil {
			return cmn.WrErr(err)
		}
		mState, err := lotusmarket.Load(lotusadt.WrapStore(ctx, ipldcbor.NewCborStore(&remoteBs{lApi})), mActor)
		if err != nil {
			return cmn.WrErr(err)
		}
		dealProposals, err := mState.Proposals()
		if err != nil {
			return cmn.WrErr(err)
		}
		dealStates, err := mState.States()
		if err != nil {
			return cmn.WrErr(err)
		}

		tenantClients := make([]fil.ActorID, 0, 32)
		if err := pgxscan.Select(
			ctx,
			db,
			&tenantClients,
			`SELECT client_id FROM spd.clients WHERE tenant_id IS NOT NULL`,
		); err != nil {
			return cmn.WrErr(err)
		}

		tenantClientDatacap := make(map[filaddr.Address]*filbig.Int, len(tenantClients))
		for _, c := range tenantClients {
			dcap, err := lApi.StateVerifiedClientStatus(ctx, c.AsFilAddr(), curTipset.Key())
			if err != nil {
				return cmn.WrErr(err)
			}
			tenantClientDatacap[c.AsFilAddr()] = dcap
		}

		log.Infof("queried datacap for %d clients", len(tenantClientDatacap))

		ourPieceList := make([]string, 0, 64<<10)
		if err := pgxscan.Select(
			ctx,
			db,
			&ourPieceList,
			`SELECT piece_cid FROM spd.pieces WHERE piece_id in ( SELECT piece_id from spd.tenants_pieces )`,
		); err != nil {
			return cmn.WrErr(err)
		}

		ourPieceMap := make(map[string]struct{}, len(ourPieceList))
		for _, p := range ourPieceList {
			ourPieceMap[p] = struct{}{}
		}

		log.Infof("system holds %s unique relevant pieces", humanize.Comma(int64(len(ourPieceMap))))

		type filDeal struct {
			pieceID  int64
			pieceCid cid.Cid
			status   string
		}

		// entries from this list are deleted below as we process the new state
		// anything left at the end - terminate
		initialDbDeals := make(map[int64]filDeal)

		rows, err := db.Query(
			ctx,
			`
			SELECT d.deal_id, d.piece_id, d.piece_cid, d.status
				FROM spd.published_deals d
			`,
		)
		if err != nil {
			return cmn.WrErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var dID int64
			var d filDeal
			var pcidStr string

			if err = rows.Scan(&dID, &d.pieceID, &pcidStr, &d.status); err != nil {
				return cmn.WrErr(err)
			}
			if d.pieceCid, err = cid.Parse(pcidStr); err != nil {
				return cmn.WrErr(err)
			}
			initialDbDeals[dID] = d
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return cmn.WrErr(err)
		}

		log.Infof("retrieved %s existing f05 records", humanize.Comma(int64(len(initialDbDeals))))

		dealCountsByState := make(map[string]int64, 8)
		seenPieces := make(map[cid.Cid]struct{}, 1<<20)
		seenProviders := make(map[filaddr.Address]struct{}, 4096)
		seenClients := make(map[filaddr.Address]struct{}, 4096)

		defer func() {
			log.Infow("summary",
				"totalDeals", dealCountsByState,
				"uniquePieces", len(seenPieces),
				"uniqueProviders", len(seenProviders),
				"uniqueClients", len(seenClients),
			)
		}()

		type deal struct {
			proposal          lotusmarket.DealProposal
			dealID            int64
			pieceID           int64
			providerID        fil.ActorID
			clientID          fil.ActorID
			pieceLog2Size     uint8
			prevState         *filDeal
			sectorStart       *filabi.ChainEpoch
			status            string
			terminationReason string
			label             []byte
			metaJSONB         []byte
		}

		toUpsert := make([]*deal, 0, 8<<10)
		var mu sync.Mutex
		showProgress := isatty.IsTerminal(os.Stderr.Fd())

		log.Infow("iterating over f05 MarketState as of", "state", curTipset.Key(), "epoch", curTipset.Height(), "wallTime", time.Unix(int64(curTipset.Blocks()[0].Timestamp), 0))

		if err := dealProposals.ForEach(func(id filabi.DealID, prop lotusmarket.DealProposal) error {

			mu.Lock()
			dealCountsByState["f05Total"]++
			if showProgress && (dealCountsByState["f05Total"]%10_000) == 0 {
				fmt.Fprintf(os.Stderr, "  %s f05 entries\r", humanize.Comma(dealCountsByState["f05Total"]))
			}

			if _, known := ourPieceMap[prop.PieceCID.String()]; !known {
				// not a piece we care about
				mu.Unlock()
				return nil
			}

			d := deal{
				proposal: prop,
				dealID:   int64(id),
				status:   "published", // always begin as "published" adjust accordingly below
			}

			if kd, known := initialDbDeals[d.dealID]; known {
				d.prevState = &kd
				delete(initialDbDeals, d.dealID) // at the end whatever remains is not in SMA list, thus will be marked "terminated"
			}

			seenPieces[d.proposal.PieceCID] = struct{}{}
			seenProviders[d.proposal.Provider] = struct{}{}
			seenClients[d.proposal.Client] = struct{}{}
			mu.Unlock()

			s, found, err := dealStates.Get(id)
			if err != nil {
				return xerrors.Errorf("failed to get state for deal in proposals array: %w", err)
			} else if !found {
				s = lotusmarket.EmptyDealState()
			}

			if s.SlashEpoch() != -1 {
				d.status = "terminated"
				d.terminationReason = "entered on-chain final-slashed state"
			} else if s.SectorStartEpoch() > 0 {
				sStart := s.SectorStartEpoch()
				d.sectorStart = &sStart
				d.status = "active"
			} else if d.proposal.StartEpoch+filbuiltin.EpochsInDay < curTipset.Height() { // FIXME replace with DealUpdatesInterval
				// if things are that late: they are never going to make it
				d.status = "terminated"
				d.terminationReason = "containing sector missed expected sealing epoch"
			}

			// because of how we account for datacap, the in-db value must reflect everything not-yet-activated
			mu.Lock()
			if dcap, known := tenantClientDatacap[d.proposal.Client]; known && d.proposal.VerifiedDeal && d.status == "published" {
				if dcap == nil {
					mu.Unlock()
					return xerrors.Errorf("client %s does not seem to have datacap yet published fil+ deal %d", d.proposal.Client, d.dealID)
				}
				nv := filbig.Add(*dcap, filbig.NewInt(int64(d.proposal.PieceSize)))
				tenantClientDatacap[d.proposal.Client] = &nv
			}

			dealCountsByState[d.status]++
			if d.prevState == nil {
				if d.status == "terminated" {
					dealCountsByState["terminatedNewDirect"]++
				} else if d.status == "active" {
					dealCountsByState["activeNewDirect"]++
				} else {
					dealCountsByState["publishedNew"]++
				}
				toUpsert = append(toUpsert, &d)
			} else if d.status != d.prevState.status {
				dealCountsByState[d.status+"New"]++
				toUpsert = append(toUpsert, &d)
			}
			mu.Unlock()

			return nil
		}); err != nil {
			return cmn.WrErr(err)
		}

		// fill in some blanks
		for _, d := range toUpsert {

			if d.proposal.Label.IsBytes() {
				d.label, _ = d.proposal.Label.ToBytes()
			} else if d.proposal.Label.IsString() {
				ls, _ := d.proposal.Label.ToString()
				d.label = []byte(ls)
			} else {
				return xerrors.New("this should not happen...")
			}

			d.metaJSONB, err = json.Marshal(
				struct {
					TermReason string `json:"termination_reason,omitempty"`
				}{TermReason: d.terminationReason},
			)
			if err != nil {
				return cmn.WrErr(err)
			}

			d.clientID, err = fil.ParseActorString(d.proposal.Client.String())
			if err != nil {
				return cmn.WrErr(err)
			}
			d.providerID, err = fil.ParseActorString(d.proposal.Provider.String())
			if err != nil {
				return cmn.WrErr(err)
			}

			if bits.OnesCount64(uint64(d.proposal.PieceSize)) != 1 {
				return xerrors.Errorf("deal %d size for is not a power of 2", d.proposal.PieceSize)
			}
			d.pieceLog2Size = uint8(bits.TrailingZeros64(uint64(d.proposal.PieceSize)))
		}

		toFail := make([]int64, 0, len(initialDbDeals))
		// whatever remains here is gone from the state entirely
		for dID, d := range initialDbDeals {
			dealCountsByState["terminated"]++
			if d.status != "terminated" {
				dealCountsByState["terminatedNew"]++
				toFail = append(toFail, dID)
			}
		}

		log.Infof(
			"about to upsert %s modified deal states, and terminate %s no longer existing deals",
			humanize.Comma(int64(len(toUpsert))),
			humanize.Comma(int64(len(toFail))),
		)

		return db.BeginTxFunc(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {

			if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL STATEMENT_TIMEOUT = %d`, (1*time.Hour).Milliseconds())); err != nil {
				return err
			}

			for _, d := range toUpsert {
				if err = tx.QueryRow(
					ctx,
					`
					INSERT INTO spd.published_deals
						( deal_id, client_id, provider_id, piece_cid, claimed_log2_size, label, is_filplus, status, published_deal_meta, start_epoch, end_epoch, sector_start_epoch )
						VALUES ( $1, $2, $3, $4, $5, $6, $7, $8, $9::JSONB, $10, $11, $12 )
					ON CONFLICT ( deal_id ) DO UPDATE SET
						status = EXCLUDED.status,
						published_deal_meta = spd.published_deals.published_deal_meta || EXCLUDED.published_deal_meta,
						sector_start_epoch = COALESCE( EXCLUDED.sector_start_epoch, spd.published_deals.sector_start_epoch )
					RETURNING piece_id
					`,
					d.dealID,
					d.clientID,
					d.providerID,
					d.proposal.PieceCID,
					d.pieceLog2Size,
					d.label,
					d.proposal.VerifiedDeal,
					d.status,
					d.metaJSONB,
					d.proposal.StartEpoch,
					d.proposal.EndEpoch,
					d.sectorStart,
				).Scan(&d.pieceID); err != nil {
					return cmn.WrErr(err)
				}

				if d.status == "active" && (d.prevState == nil || d.prevState.status != "active") {
					if _, err := tx.Exec(
						ctx,
						`
						UPDATE spd.proposals
							SET activated_deal_id = $1
						WHERE
							proposal_failstamp = 0
								AND
							proposal_delivered IS NOT NULL
								AND
							activated_deal_id IS NULL
								AND
							piece_id = $2
								AND
							provider_id = $3
								AND
							client_id = $4
						`,
						d.dealID,
						d.pieceID,
						d.providerID,
						d.clientID,
					); err != nil {
						return cmn.WrErr(err)
					}
				}
			}

			// we may have some terminations ( no longer in the market state )
			if len(toFail) > 0 {
				if _, err = tx.Exec(
					ctx,
					`
					UPDATE spd.published_deals SET
						status = 'terminated',
						published_deal_meta = published_deal_meta || '{ "termination_reason":"deal no longer part of market-actor state" }'
					WHERE
						deal_id = ANY ( $1::BIGINT[] )
							AND
						status != 'terminated'
					`,
					toFail,
				); err != nil {
					return cmn.WrErr(err)
				}
			}

			// update datacap
			for c, d := range tenantClientDatacap {
				var di int64
				if d != nil {
					di = d.Int64()
				}

				if _, exists := unusableDatacap[c.String()]; exists {
					di = 0
				}

				if _, err := tx.Exec(
					ctx,
					`
					UPDATE spd.clients SET
						client_meta = JSONB_SET( client_meta, '{ activatable_datacap }', TO_JSONB( $1::BIGINT ) )
					WHERE
						client_id = $2
					`,
					di,
					fil.MustParseActorString(c.String()),
				); err != nil {
					return cmn.WrErr(err)
				}
			}

			// anything that activated is obviously the correct size
			if _, err := tx.Exec(
				ctx,
				`
				UPDATE spd.pieces SET piece_meta = piece_meta || '{ "size_proven_correct":true }',
						piece_log2_size = active.proven_log2_size
					FROM (
						SELECT pd.piece_id, pd.claimed_log2_size AS proven_log2_size
							FROM spd.published_deals pd, spd.pieces p
						WHERE
							( NOT COALESCE( (p.piece_meta->'size_proven_correct')::BOOL, false) )
								AND
							pd.piece_id = p.piece_id
								AND
							pd.status = 'active'
					) active
				WHERE
					( NOT COALESCE( (pieces.piece_meta->'size_proven_correct')::BOOL, false) )
						AND
					pieces.piece_id = active.piece_id
				`,
			); err != nil {
				return cmn.WrErr(err)
			}

			// clear out proposals that will never make it
			if _, err := tx.Exec(
				ctx,
				`
				UPDATE spd.proposals SET
					proposal_failstamp = spd.big_now(),
					proposal_meta = JSONB_SET(
						proposal_meta,
						'{ failure }',
						TO_JSONB( 'proposal DealStartEpoch missed without activation'::TEXT )
					)
				WHERE
					proposal_failstamp = 0
						AND
					activated_deal_id IS NULL
						AND
					start_epoch < $1
				`,
				curTipset.Height()-filbuiltin.EpochsInDay, // FIXME replace with DealUpdatesInterval
			); err != nil {
				return cmn.WrErr(err)
			}

			// clear out proposals that had an active deal which subsequently terminated
			if _, err := tx.Exec(
				ctx,
				`
				UPDATE spd.proposals SET
					activated_deal_id = NULL,
					proposal_failstamp = spd.big_now(),
					proposal_meta = JSONB_SET(
						proposal_meta,
						'{ failure }',
						TO_JSONB( 'sector containing deal was terminated'::TEXT )
					)
				WHERE
					activated_deal_id IN ( SELECT deal_id FROM spd.published_deals WHERE status = 'terminated' )
				`,
			); err != nil {
				return cmn.WrErr(err)
			}

			// clear out proposals that had an active deal which subsequently was deemed invalid
			if _, err := tx.Exec(
				ctx,
				`
				UPDATE spd.proposals SET
					proposal_failstamp = spd.big_now(),
					proposal_meta = JSONB_SET(
						proposal_meta,
						'{ failure }',
						TO_JSONB( 'deal declared invalid'::TEXT )
					)
				WHERE
					proposal_failstamp = 0
						AND
					activated_deal_id IN ( SELECT deal_id FROM spd.invalidated_deals )
				`,
			); err != nil {
				return cmn.WrErr(err)
			}

			msJ, _ := json.Marshal(struct {
				Epoch  filabi.ChainEpoch `json:"epoch"`
				Tipset fil.LotusTSK      `json:"tipset"`
			}{
				Epoch:  curTipset.Height(),
				Tipset: curTipset.Key(),
			})

			if _, err := tx.Exec(
				ctx,
				`
				UPDATE spd.global SET metadata = JSONB_SET( metadata, '{ market_state }', $1 )
				`,
				msJ,
			); err != nil {
				return cmn.WrErr(err)
			}

			return app.RefreshMatviews(ctx, tx)
		})
	},
}
