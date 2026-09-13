package trade

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// DefaultResponse is the wire shape of a simple {"type":"default"}
// envelope returned by some Hyperliquid endpoints.
type DefaultResponse struct {
	Type string `json:"type"`
}

// Agent is the typed handle returned by ApproveAgent. Address is the
// 0x-prefixed agent EOA; PrivateKey is the freshly generated ECDSA key
// associated with that address — keep it secret.
type Agent struct {
	// Address is the lower-case 0x-prefixed hex of the agent EOA.
	Address string
	// PrivateKey is the ECDSA private key controlling Address.
	PrivateKey *ecdsa.PrivateKey
}

// ApprovalResponse is the response shape returned by approval-style
// actions (approveBuilderFee, evmUserModify, ...).
type ApprovalResponse struct {
	Status string `json:"status"`
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AgentApprovalResponse is returned by the approveAgent action.
// Hyperliquid encodes failure as {"status":"err","response":"<message>"};
// success as {"status":"ok","response":{...}}. The Response field
// captures whichever form was returned so callers can surface the
// rejection reason verbatim.
type AgentApprovalResponse struct {
	Status   string          `json:"status"`
	TxHash   string          `json:"txHash,omitempty"`
	Error    string          `json:"error,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// SetReferrerResponse is returned by the setReferrer action.
type SetReferrerResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ScheduleCancelResponse is returned by the scheduleCancel action.
type ScheduleCancelResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SetLeverage updates the leverage on coin. mode picks Cross or Isolated
// margin: Cross maps to isCross=true (shared collateral across
// positions), Isolated to isCross=false (per-position collateral).
// leverage is an integer multiple in the range allowed by the asset's
// margin table.
func (c *Client) SetLeverage(coin string, leverage int, mode types.MarginMode) (*info.UserState, error) {
	action := signing.UpdateLeverageAction{
		Type:     "updateLeverage",
		Asset:    c.info.AssetID(coin),
		IsCross:  mode == types.Cross,
		Leverage: leverage,
	}

	var result info.UserState
	if err := c.executeAction(action, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AdjustMargin adds or removes isolated-margin collateral on the position
// in coin. A positive amount increases collateral; a negative amount
// withdraws it. amount is in USDC (decimal).
func (c *Client) AdjustMargin(coin string, amount float64) (*types.APIResponse[DefaultResponse], error) {
	action := signing.UpdateIsolatedMarginAction{
		Type:  "updateIsolatedMargin",
		Asset: c.info.AssetID(coin),
		IsBuy: true,
		Ntli:  int64(math.Round(amount * 1e6)),
	}
	var result types.APIResponse[DefaultResponse]
	if err := c.executeAction(action, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SetExpiresAfter updates the expiration deadline stamped on every
// signed action the Client subsequently dispatches. A zero deadline
// (the zero value of time.Time) clears the field.
func (c *Client) SetExpiresAfter(deadline time.Time) {
	if deadline.IsZero() {
		c.expiresAfter = nil
		return
	}
	ms := deadline.UnixMilli()
	c.expiresAfter = &ms
}

// ScheduleCancelAll schedules cancellation of all open orders at
// deadline. A nil deadline clears any scheduled cancel and lets existing
// orders rest indefinitely. A non-nil deadline is converted to a
// Unix-millisecond timestamp before signing.
func (c *Client) ScheduleCancelAll(deadline *time.Time) (*ScheduleCancelResponse, error) {
	timestamp := time.Now().UnixMilli()

	var scheduleTime *int64
	if deadline != nil {
		ms := deadline.UnixMilli()
		scheduleTime = &ms
	}

	action := signing.ScheduleCancelAction{
		Type: "scheduleCancel",
		Time: scheduleTime,
	}

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		c.vault,
		timestamp,
		c.expiresAfter,
		c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.postAction(action, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result ScheduleCancelResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SetReferrer sets a referral code for the signing account.
func (c *Client) SetReferrer(code string) (*SetReferrerResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := signing.SetReferrerAction{
		Type: "setReferrer",
		Code: code,
	}

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		"",
		timestamp,
		c.expiresAfter,
		c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.postAction(action, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result SetReferrerResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UseBigBlocks enables or disables big-block evm-user mode.
func (c *Client) UseBigBlocks(enable bool) (*ApprovalResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := signing.UseBigBlocksAction{
		Type:           "evmUserModify",
		UsingBigBlocks: enable,
	}

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		"",
		timestamp,
		c.expiresAfter,
		c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.postAction(action, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result ApprovalResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ApproveAgent generates a fresh agent key, registers it with
// Hyperliquid under the optional name, and returns the resulting Agent.
// The empty string disables the agent-name field on the wire.
func (c *Client) ApproveAgent(name string) (Agent, error) {
	agentBytes := make([]byte, 32)
	if _, err := rand.Read(agentBytes); err != nil {
		return Agent{}, fmt.Errorf("generate agent key: %w", err)
	}
	agentKeyHex := hex.EncodeToString(agentBytes)
	pk, err := crypto.HexToECDSA(agentKeyHex)
	if err != nil {
		return Agent{}, fmt.Errorf("parse agent key: %w", err)
	}
	agentAddress := strings.ToLower(crypto.PubkeyToAddress(pk.PublicKey).Hex())

	nonce := time.Now().UnixMilli()
	action := map[string]any{
		"type":         "approveAgent",
		"agentAddress": agentAddress,
		"agentName":    name,
		"nonce":        nonce,
	}
	var result AgentApprovalResponse
	if err := c.executeUserSignedAction(
		action, signing.ApproveAgentSignTypes,
		"HyperliquidTransaction:ApproveAgent", nonce, &result,
	); err != nil {
		return Agent{}, err
	}
	if result.Status != "" && result.Status != "ok" {
		msg := result.Error
		if msg == "" {
			// On err the Response field is a JSON string with the reason.
			var reason string
			if err := json.Unmarshal(result.Response, &reason); err == nil && reason != "" {
				msg = reason
			}
		}
		if msg == "" {
			msg = result.Status
		}
		return Agent{}, fmt.Errorf("approveAgent rejected: %s", msg)
	}
	return Agent{Address: agentAddress, PrivateKey: pk}, nil
}

// ApproveBuilderFee approves a builder address to charge up to
// maxFeeRate. maxFeeRate must be a percent string like "0.1%".
func (c *Client) ApproveBuilderFee(builder string, maxFeeRate string) (*ApprovalResponse, error) {
	nonce := time.Now().UnixMilli()
	action := map[string]any{
		"type":       "approveBuilderFee",
		"maxFeeRate": maxFeeRate,
		"builder":    strings.ToLower(builder),
		"nonce":      nonce,
	}
	var result ApprovalResponse
	if err := c.executeUserSignedAction(
		action, signing.ApproveBuilderFeeSignTypes,
		"HyperliquidTransaction:ApproveBuilderFee", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}
