package trade

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// MultiSigConversionResponse is returned by the
// convertToMultiSigUser action.
type MultiSigConversionResponse struct {
	Status string `json:"status"`
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// MultiSigResponse is returned by the multiSig action envelope.
type MultiSigResponse struct {
	Status string `json:"status"`
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// MultiSigGroup exposes multi-sig conversion and execution actions on the
// Client. Reach it via c.MultiSig.
type MultiSigGroup struct {
	t *Client
}

// Convert converts the signing account to a multi-sig user authorising
// the supplied signer addresses. threshold is the number of signatures
// required to authorise a multi-sig action.
func (g *MultiSigGroup) Convert(authorized []string, threshold int) (*MultiSigConversionResponse, error) {
	nonce := time.Now().UnixMilli()
	sort.Strings(authorized)
	signersJSON, err := json.Marshal(map[string]any{
		"authorizedUsers": authorized,
		"threshold":       threshold,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal signers: %w", err)
	}
	action := map[string]any{
		"type":    "convertToMultiSigUser",
		"signers": string(signersJSON),
		"nonce":   nonce,
	}
	var result MultiSigConversionResponse
	if err := g.t.executeUserSignedAction(
		action, signing.ConvertToMultiSigUserSignTypes,
		"HyperliquidTransaction:ConvertToMultiSigUser", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// Execute submits a multi-sig action wrapping the inner action with the
// supplied signers and their hex signatures.
func (g *MultiSigGroup) Execute(action map[string]any, signers []string, signatures []string) (*MultiSigResponse, error) {
	timestamp := time.Now().UnixMilli()

	multiSigAction := map[string]any{
		"type":       "multiSig",
		"action":     action,
		"signers":    signers,
		"signatures": signatures,
	}

	sig, err := signing.SignL1Action(
		g.t.privateKey,
		multiSigAction,
		g.t.vault,
		timestamp,
		g.t.expiresAfter,
		g.t.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := g.t.postAction(multiSigAction, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result MultiSigResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
