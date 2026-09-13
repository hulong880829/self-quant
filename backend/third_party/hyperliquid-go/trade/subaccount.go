package trade

import (
	"encoding/json"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// CreateSubAccountResponse is returned by the createSubAccount action.
type CreateSubAccountResponse struct {
	Status string           `json:"status"`
	Data   *info.SubAccount `json:"data,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// SubAccountGroup exposes sub-account management on Client.
type SubAccountGroup struct {
	t *Client
}

// Create allocates a new sub-account under the current signer.
func (g *SubAccountGroup) Create(name string) (*CreateSubAccountResponse, error) {
	t := g.t
	timestamp := time.Now().UnixMilli()

	action := signing.CreateSubAccountAction{
		Type: "createSubAccount",
		Name: name,
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

	var result CreateSubAccountResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DepositUSD funds a sub-account from the parent's USDC balance.
func (g *SubAccountGroup) DepositUSD(subAddr string, amount float64) (*TransferResponse, error) {
	return g.transfer(subAddr, true, signing.FloatToUsdInt(amount))
}

// WithdrawUSD pulls USDC from a sub-account back to the parent.
func (g *SubAccountGroup) WithdrawUSD(subAddr string, amount float64) (*TransferResponse, error) {
	return g.transfer(subAddr, false, signing.FloatToUsdInt(amount))
}

// transfer signs and submits a subAccountTransfer action.
func (g *SubAccountGroup) transfer(subAccountUser string, isDeposit bool, usd int) (*TransferResponse, error) {
	t := g.t
	timestamp := time.Now().UnixMilli()

	action := signing.SubAccountTransferAction{
		Type:           "subAccountTransfer",
		SubAccountUser: subAccountUser,
		IsDeposit:      isDeposit,
		Usd:            usd,
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

// DepositSpot funds a sub-account's spot balance with token.
func (g *SubAccountGroup) DepositSpot(subAddr, token string, amount float64) (*TransferResponse, error) {
	return g.spotTransfer(subAddr, true, token, amount)
}

// WithdrawSpot pulls a spot token back from a sub-account.
func (g *SubAccountGroup) WithdrawSpot(subAddr, token string, amount float64) (*TransferResponse, error) {
	return g.spotTransfer(subAddr, false, token, amount)
}

// spotTransfer signs and submits a subAccountSpotTransfer action.
func (g *SubAccountGroup) spotTransfer(subAccountUser string, isDeposit bool, token string, amount float64) (*TransferResponse, error) {
	t := g.t
	timestamp := time.Now().UnixMilli()

	action := signing.SubAccountSpotTransferAction{
		Type:           "subAccountSpotTransfer",
		SubAccountUser: subAccountUser,
		IsDeposit:      isDeposit,
		Token:          token,
		Amount:         amount,
	}

	sig, err := signing.SignL1Action(
		t.privateKey,
		action,
		t.vault,
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
