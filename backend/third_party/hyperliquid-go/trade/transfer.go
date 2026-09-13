package trade

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TransferResponse is the response shape returned by transfer-style
// signed actions (usdSend, spotSend, vaultTransfer, etc.) and the
// HIP-4 userOutcome action family. Hyperliquid encodes failure as
// {"status":"err","response":"<message>"}; Response captures the raw
// payload so callers can extract the reason without re-parsing the
// wire bytes.
type TransferResponse struct {
	Status   string          `json:"status"`
	TxHash   string          `json:"txHash,omitempty"`
	Error    string          `json:"error,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// TransferGroup exposes transfer-related actions in a sub-grouped
// namespace. Reach it via c.Transfer (initialised lazily).
type TransferGroup struct {
	t *Client
}

// SendUSD sends USDC on Hyperliquid to toAddr.
func (g *TransferGroup) SendUSD(toAddr string, amount float64) (*TransferResponse, error) {
	nonce := time.Now().UnixMilli()
	action := map[string]any{
		"type":        "usdSend",
		"destination": toAddr,
		"amount":      formatUsdAmount(amount),
		"time":        nonce,
	}
	var result TransferResponse
	if err := g.t.executeUserSignedAction(
		action, signing.UsdSendSignTypes,
		"HyperliquidTransaction:UsdSend", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// SendSpot sends a spot token to toAddr.
func (g *TransferGroup) SendSpot(toAddr, token string, amount float64) (*TransferResponse, error) {
	nonce := time.Now().UnixMilli()
	action := map[string]any{
		"type":        "spotSend",
		"destination": toAddr,
		"token":       token,
		"amount":      formatUsdAmount(amount),
		"time":        nonce,
	}
	var result TransferResponse
	if err := g.t.executeUserSignedAction(
		action, signing.SpotTransferSignTypes,
		"HyperliquidTransaction:SpotSend", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// DepositToVault deposits USDC into the given vault.
func (g *TransferGroup) DepositToVault(vaultAddr string, amount float64) (*TransferResponse, error) {
	return g.vaultTransfer(vaultAddr, true, signing.FloatToUsdInt(amount))
}

// WithdrawFromVault withdraws USDC from the given vault.
func (g *TransferGroup) WithdrawFromVault(vaultAddr string, amount float64) (*TransferResponse, error) {
	return g.vaultTransfer(vaultAddr, false, signing.FloatToUsdInt(amount))
}

// vaultTransfer signs and submits a vaultTransfer action for either a
// deposit or a withdrawal.
func (g *TransferGroup) vaultTransfer(vaultAddress string, isDeposit bool, usd int) (*TransferResponse, error) {
	timestamp := time.Now().UnixMilli()
	t := g.t

	action := signing.VaultUsdTransferAction{
		Type:         "vaultTransfer",
		VaultAddress: vaultAddress,
		IsDeposit:    isDeposit,
		Usd:          usd,
	}

	sig, err := signing.SignL1Action(
		t.privateKey,
		action,
		"",
		timestamp,
		t.expiresAfter,
		t.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := t.postAction(action, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result TransferResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// PerpToSpot transfers USDC from perp to spot for the signing account.
func (g *TransferGroup) PerpToSpot(amount float64) (*TransferResponse, error) {
	return g.usdClassTransfer(amount, false)
}

// SpotToPerp transfers USDC from spot to perp for the signing account.
func (g *TransferGroup) SpotToPerp(amount float64) (*TransferResponse, error) {
	return g.usdClassTransfer(amount, true)
}

// usdClassTransfer signs and submits a usdClassTransfer action.
func (g *TransferGroup) usdClassTransfer(amount float64, toPerp bool) (*TransferResponse, error) {
	t := g.t
	nonce := time.Now().UnixMilli()
	amountStr := formatUsdAmount(amount)
	if t.vault != "" {
		amountStr += " subaccount:" + t.vault
	}
	action := map[string]any{
		"type":   "usdClassTransfer",
		"amount": amountStr,
		"toPerp": toPerp,
		"nonce":  nonce,
	}
	var result TransferResponse
	if err := t.executeUserSignedAction(
		action, signing.UsdClassTransferSignTypes,
		"HyperliquidTransaction:UsdClassTransfer", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// MoveToDex moves tokens from the default perp wallet into a builder-
// deployed (HIP-3) perp dex. The destination address is the signing
// account; HIP-3 transfers are user-signed sendAsset actions whose
// sourceDex is the empty string for the default perp class.
func (g *TransferGroup) MoveToDex(dex, token string, amount float64) (*TransferResponse, error) {
	return g.sendAsset("", dex, token, amount)
}

// MoveFromDex moves tokens out of a builder-deployed perp dex back
// to the default perp wallet of the signing account.
func (g *TransferGroup) MoveFromDex(dex, token string, amount float64) (*TransferResponse, error) {
	return g.sendAsset(dex, "", token, amount)
}

// sendAsset is the underlying user-signed action for cross-DEX and
// cross-class transfers. sourceDex / destinationDex use the empty
// string for the default perp class and "spot" for the spot class;
// any other value is a builder-deployed perp dex name. The destination
// address is always the signing account — sub-account routing goes via
// the fromSubAccount field, which currently mirrors the trader's vault.
func (g *TransferGroup) sendAsset(sourceDex, destinationDex, token string, amount float64) (*TransferResponse, error) {
	t := g.t
	nonce := time.Now().UnixMilli()
	dest := t.effectiveAddr()
	if dest == "" {
		return nil, fmt.Errorf("sendAsset: no destination address available")
	}
	action := map[string]any{
		"type":           "sendAsset",
		"destination":    dest,
		"sourceDex":      sourceDex,
		"destinationDex": destinationDex,
		"token":          token,
		"amount":         formatUsdAmount(amount),
		"fromSubAccount": t.vault,
		"nonce":          nonce,
	}
	var result TransferResponse
	if err := t.executeUserSignedAction(
		action, signing.SendAssetSignTypes,
		"HyperliquidTransaction:SendAsset", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}
