// Package trade is the signed-action surface of the Hyperliquid SDK.
// Construct a *Client indirectly via the top-level hyperliquid.New
// (which forwards to trade.New); direct construction is reserved for
// tests (NewForTest).
package trade

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/Simon-Busch/hyperliquid-go/info"
	xtransport "github.com/Simon-Busch/hyperliquid-go/internal/transport"
	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/stream"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// Client is the signed-action surface. Construct it indirectly via
// hyperliquid.New, or via trade.New for a self-contained handle. Tests
// use NewForTest.
type Client struct {
	client       *xtransport.Client
	privateKey   *ecdsa.PrivateKey
	vault        string
	accountAddr  string
	dex          string // For HIP-3 builder-deployed perps
	info         *info.Client
	expiresAfter *int64

	// userState caches the most recent UserState snapshot used for
	// position-aware validation. It is refreshed by validate() on every
	// placement attempt unless SkipValidation is in effect, and may be
	// refreshed explicitly via RefreshState.
	userState   *info.UserState
	userStateMu sync.RWMutex
	streamer    ActionPoster

	// Transfer exposes balance and vault transfer actions.
	Transfer *TransferGroup
	// SubAccount exposes sub-account management.
	SubAccount *SubAccountGroup
	// Stake exposes HYPE staking actions.
	Stake *StakeGroup
	// MultiSig exposes multi-sig conversion and execution helpers.
	MultiSig *MultiSigGroup
	// Convert exposes spot-token conversion helpers (e.g. USDC <-> USDH).
	Convert *ConvertGroup
	// Outcome exposes HIP-4 user-outcome actions (split, merge,
	// mergeQuestion, negate) — mint and burn outcome shares against the
	// market's quote token (USDC since the USDH sunset) without crossing
	// the order book.
	Outcome *OutcomeGroup
}

// Config carries the inputs needed to build a *Client. All fields are
// optional except Client and Info; PrivateKey is required for signed
// actions but may be nil for handles only used to inspect cached state.
type Config struct {
	Client       *xtransport.Client
	PrivateKey   *ecdsa.PrivateKey
	Vault        string
	AccountAddr  string
	Dex          string
	Info         *info.Client
	ExpiresAfter *int64
}

// ActionPoster submits a signed L1 action. *stream.Client implements it.
type ActionPoster interface {
	PostActionContext(ctx context.Context, action any, signature signing.SignatureResult, nonce int64, vaultAddress string) (json.RawMessage, error)
}

// SetStream attaches the websocket action transport used by Place*WS.
func (c *Client) SetStream(poster ActionPoster) {
	c.streamer = poster
}

// New builds a *Client from cfg and wires up the subgroups.
func New(cfg Config) *Client {
	c := &Client{
		client:       cfg.Client,
		privateKey:   cfg.PrivateKey,
		vault:        cfg.Vault,
		accountAddr:  cfg.AccountAddr,
		dex:          cfg.Dex,
		info:         cfg.Info,
		expiresAfter: cfg.ExpiresAfter,
	}
	c.attachSubgroups()
	return c
}

// NewForTest returns a *Client with the supplied dependencies prefilled.
// Intended for unit tests that want to exercise validate / place /
// cancel logic without a live HTTP transport or signing key.
func NewForTest(client *xtransport.Client, infoC *info.Client, userState *info.UserState, accountAddr string) *Client {
	c := &Client{
		client:      client,
		info:        infoC,
		userState:   userState,
		accountAddr: accountAddr,
	}
	return c
}

// effectiveAddr returns the address used for position-state lookups. It
// prefers accountAddr (agent flow), falls back to vault, then derives the
// address from the configured private key.
func (c *Client) effectiveAddr() string {
	if c.accountAddr != "" {
		return c.accountAddr
	}
	if c.vault != "" {
		return c.vault
	}
	if c.privateKey == nil {
		return ""
	}
	return strings.ToLower(crypto.PubkeyToAddress(c.privateKey.PublicKey).Hex())
}

// RefreshState refreshes the cached UserState snapshot used by
// position-aware validation. The ctx parameter is reserved for future
// cancellation; the underlying HTTP call does not yet honour it.
func (c *Client) RefreshState(ctx context.Context) error {
	_ = ctx
	addr := c.effectiveAddr()
	if addr == "" {
		return fmt.Errorf("hyperliquid: no address available for RefreshState")
	}
	// When the trader is pinned to a HIP-3 builder dex, the position
	// state lives under that dex's clearinghouse view, not the default
	// perp dex. Without the dex argument the cache would always be empty
	// and the long/short safety rules in validate() would no-op.
	var state *info.UserState
	var err error
	if c.dex != "" {
		state, err = c.info.UserState(addr, c.dex)
	} else {
		state, err = c.info.UserState(addr)
	}
	if err != nil {
		return err
	}
	c.userStateMu.Lock()
	c.userState = state
	c.userStateMu.Unlock()
	return nil
}

// cachedUserState returns a snapshot of the cached UserState pointer
// without re-fetching. Callers must treat the result as read-only.
func (c *Client) cachedUserState() *info.UserState {
	c.userStateMu.RLock()
	defer c.userStateMu.RUnlock()
	return c.userState
}

// attachSubgroups initialises the Transfer/SubAccount/Stake/MultiSig/
// Convert subgroup fields.
func (c *Client) attachSubgroups() {
	c.Transfer = &TransferGroup{t: c}
	c.SubAccount = &SubAccountGroup{t: c}
	c.Stake = &StakeGroup{t: c}
	c.MultiSig = &MultiSigGroup{t: c}
	c.Convert = &ConvertGroup{t: c}
	c.Outcome = &OutcomeGroup{t: c}
}

// PerpDex returns the configured builder perp dex name (e.g. "flx"), or
// empty string for the default dex.
func (c *Client) PerpDex() string {
	return c.dex
}

// executeAction executes an action and unmarshals the response into the
// given result.
func (c *Client) executeAction(action any, result any) error {
	return c.dispatchAction(context.Background(), action, result, false)
}

func (c *Client) executeActionWS(ctx context.Context, action any, result any) error {
	return c.dispatchAction(ctx, action, result, true)
}

func (c *Client) dispatchAction(ctx context.Context, action any, result any, ws bool) error {
	timestamp := time.Now().UnixMilli()

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		c.vault,
		timestamp,
		c.expiresAfter,
		c.client != nil && c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return err
	}

	var resp []byte
	if ws {
		if c.streamer == nil {
			return fmt.Errorf("%w", stream.ErrNotConnected)
		}
		vault := ""
		if !userSignedActionTypes[actionTypeOf(action)] {
			vault = c.vault
		}
		raw, postErr := c.streamer.PostActionContext(ctx, action, sig, timestamp, vault)
		if postErr != nil {
			return postErr
		}
		resp = raw
	} else {
		resp, err = c.postAction(action, sig, timestamp)
		if err != nil {
			return err
		}
	}

	err = json.Unmarshal(resp, result)
	if err != nil {
		if ws {
			return fmt.Errorf("%w: failed to unmarshal response: %v", stream.ErrBadResponse, err)
		}
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return nil
}

// userSignedActionTypes are user-signed (EIP-712) actions for which
// /exchange expects vaultAddress: null in the envelope.
var userSignedActionTypes = map[string]bool{
	"usdClassTransfer":      true,
	"sendAsset":             true,
	"usdSend":               true,
	"spotSend":              true,
	"withdraw3":             true,
	"approveAgent":          true,
	"approveBuilderFee":     true,
	"tokenDelegate":         true,
	"convertToMultiSigUser": true,
}

func actionTypeOf(action any) string {
	if m, ok := action.(map[string]any); ok {
		if t, _ := m["type"].(string); t != "" {
			return t
		}
	}
	b, err := json.Marshal(action)
	if err != nil {
		return ""
	}
	var peek struct{ Type string }
	_ = json.Unmarshal(b, &peek)
	return peek.Type
}

func (c *Client) postAction(
	action any,
	signature signing.SignatureResult,
	nonce int64,
) ([]byte, error) {
	payload := map[string]any{
		"action":    action,
		"nonce":     nonce,
		"signature": signature,
	}
	if userSignedActionTypes[actionTypeOf(action)] {
		payload["vaultAddress"] = nil
	} else if c.vault != "" {
		payload["vaultAddress"] = c.vault
	}
	return c.client.Post("/exchange", payload)
}

// executeUserSignedAction signs a user-signed action with the proper
// HyperliquidSignTransaction EIP-712 domain and POSTs to /exchange.
func (c *Client) executeUserSignedAction(
	action map[string]any,
	payloadTypes []apitypes.Type,
	primaryType string,
	nonce int64,
	result any,
) error {
	sig, err := signing.SignUserSignedAction(
		c.privateKey, action, payloadTypes, primaryType,
		c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return err
	}
	resp, err := c.postAction(action, sig, nonce)
	if err != nil {
		return err
	}
	return json.Unmarshal(resp, result)
}

// GetAccountAddr returns the account address.
func (c *Client) GetAccountAddr() string {
	return c.accountAddr
}

// GetInfo returns the info instance.
func (c *Client) GetInfo() *info.Client {
	return c.info
}

// WarmUp pre-establishes the HTTP/2 connection so the first order doesn't
// pay the cold-start penalty (TCP + TLS + ALPN). Call once after
// creating the Client.
func (c *Client) WarmUp() error {
	return c.client.WarmUp()
}
