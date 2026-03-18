package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	filaddr "github.com/filecoin-project/go-address"
	"github.com/google/uuid"
	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/storacha/spade/apitypes"
	"github.com/storacha/spade/spid"
	spid_lotus "github.com/storacha/spade/spid/lotus"
)

var log = logging.Logger("spade/client")

var DefaultBaseURL, _ = url.Parse("https://api.spade.storacha.network")

// IdentifyFunc produces a FIL-SPID-V0 challenge for authentication.
type IdentifyFunc func(ctx context.Context, addr filaddr.Address, args url.Values) (spid.Challenge, error)

type clientConfig struct {
	baseURL    url.URL
	httpClient *http.Client
}

type Option func(*clientConfig)

func WithBaseURL(baseURL url.URL) Option {
	return func(c *clientConfig) {
		c.baseURL = baseURL
	}
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *clientConfig) {
		c.httpClient = httpClient
	}
}

type SpadeClient struct {
	baseURL         url.URL
	httpClient      *http.Client
	identify        IdentifyFunc
	storageProvider filaddr.Address
}

func New(storageProvider filaddr.Address, identify IdentifyFunc, options ...Option) *SpadeClient {
	config := &clientConfig{
		httpClient: http.DefaultClient,
		baseURL:    *DefaultBaseURL,
	}
	for _, option := range options {
		option(config)
	}
	return &SpadeClient{storageProvider: storageProvider, baseURL: config.baseURL, identify: identify, httpClient: config.httpClient}
}

func (c *SpadeClient) EligiblePieces(ctx context.Context, options ...EligiblePiecesOption) (apitypes.ResponsePiecesEligible, error) {
	cfg := EligiblePiecesConfig{}
	for _, option := range options {
		option(&cfg)
	}

	args := url.Values{}
	if cfg.TenantID != 0 {
		args.Set("tenant", fmt.Sprintf("%d", cfg.TenantID))
	}
	if cfg.Limit > 0 {
		args.Set("limit", fmt.Sprintf("%d", cfg.Limit))
	}

	data, err := c.doRequest(ctx, http.MethodGet, "/sp/eligible_pieces", args)
	var resp apitypes.ResponseEnvelope[apitypes.ResponsePiecesEligible]
	if err = unmarshalResponseEnvelope(&resp, data); err != nil {
		return apitypes.ResponsePiecesEligible{}, err
	}
	return resp.Response, nil
}

func (c *SpadeClient) PendingProposals(ctx context.Context) (apitypes.ResponsePendingProposals, error) {
	args := url.Values{}
	data, err := c.doRequest(ctx, http.MethodGet, "/sp/pending_proposals", args)
	var resp apitypes.ResponseEnvelope[apitypes.ResponsePendingProposals]
	if err = unmarshalResponseEnvelope(&resp, data); err != nil {
		return apitypes.ResponsePendingProposals{}, err
	}
	return resp.Response, nil
}

func (c *SpadeClient) PieceManifest(ctx context.Context, proposal uuid.UUID) (apitypes.ResponsePieceManifestFR58, error) {
	args := url.Values{}
	args.Set("proposal", proposal.String())
	data, err := c.doRequest(ctx, http.MethodGet, "/sp/piece_manifest", args)
	var resp apitypes.ResponseEnvelope[apitypes.ResponsePieceManifestFR58]
	if err = unmarshalResponseEnvelope(&resp, data); err != nil {
		return apitypes.ResponsePieceManifestFR58{}, err
	}
	return resp.Response, nil
}

func (c *SpadeClient) ReservePiece(ctx context.Context, piece cid.Cid, options ...ReservePieceOption) (apitypes.ResponseDealRequest, error) {
	cfg := ReservePieceConfig{}
	for _, option := range options {
		option(&cfg)
	}

	args := url.Values{}
	args.Set("call", "reserve_piece")
	args.Set("piece_cid", piece.String())
	if cfg.TenantID != 0 {
		args.Set("tenant", fmt.Sprintf("%d", cfg.TenantID))
	}
	if cfg.TenantPolicy != "" {
		args.Set("tenant_policy", cfg.TenantPolicy)
	}

	data, err := c.doRequest(ctx, http.MethodPost, "/sp/invoke", args)
	if err != nil {
		return apitypes.ResponseDealRequest{}, err
	}

	var resp apitypes.ResponseEnvelope[apitypes.ResponseDealRequest]
	if err = unmarshalResponseEnvelope(&resp, data); err != nil {
		return apitypes.ResponseDealRequest{}, err
	}

	return resp.Response, nil
}

func (c *SpadeClient) doRequest(ctx context.Context, method string, path string, args url.Values) ([]byte, error) {
	u := c.baseURL.JoinPath(path)

	// if method is GET then put args in URL otherwise in signed auth header
	if method == http.MethodGet {
		u.RawQuery = args.Encode()
		args = url.Values{}
	} else if method != http.MethodPost {
		return nil, fmt.Errorf("unsupported method: %s", method)
	}

	challenge, err := c.identify(ctx, c.storageProvider, args)
	if err != nil {
		return nil, fmt.Errorf("creating SPID challenge: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", challenge.String())

	log.Debugw("request", "method", method, "url", u.String(), "args", args)

	res, err := c.httpClient.Do(req)
	if err != nil {
		log.Errorw("sending request", "method", method, "url", u.String(), "error", err)
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		log.Errorw("unexpected response", "method", method, "url", u.String(), "status", res.StatusCode, "body", string(body))
		return nil, fmt.Errorf("unexpected response status: %d", res.StatusCode)
	} else {
		log.Debugw("response", "method", method, "url", u.String(), "status", res.StatusCode, "body", string(body))
	}

	return body, nil
}

func unmarshalResponseEnvelope[T apitypes.ResponsePayload](resp *apitypes.ResponseEnvelope[T], data []byte) error {
	err := json.Unmarshal(data, resp)
	if err != nil {
		return fmt.Errorf("unmarshaling response: %w", err)
	}
	if resp.ErrCode != 0 {
		return fmt.Errorf("API error %d (%s): %s", resp.ErrCode, resp.ErrSlug, strings.Join(resp.ErrLines, "\n"))
	}
	return nil
}

// NewLotusIdentifyFunc creates an IdentifyFunc that uses a Lotus node for
// challenge generation.
func NewLotusIdentifyFunc(lotusAPI spid_lotus.LotusBeaconGetterAndWalletSignerClient) IdentifyFunc {
	return func(ctx context.Context, addr filaddr.Address, args url.Values) (spid.Challenge, error) {
		return spid_lotus.New(ctx, lotusAPI, addr, args)
	}
}
