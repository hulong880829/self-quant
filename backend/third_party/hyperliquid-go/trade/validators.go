package trade

import (
	"encoding/json"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// ValidatorResponse is returned by CValidator and CSigner actions.
type ValidatorResponse struct {
	Status string `json:"status"`
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CSigner Methods

// CSignerUnjailSelf unjails self as consensus signer.
func (c *Client) CSignerUnjailSelf() (*ValidatorResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type": "cSignerUnjailSelf",
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

	var result ValidatorResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CSignerJailSelf jails self as consensus signer.
func (c *Client) CSignerJailSelf() (*ValidatorResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type": "cSignerJailSelf",
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

	var result ValidatorResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CSignerInner executes inner consensus signer action.
func (c *Client) CSignerInner(innerAction map[string]any) (*ValidatorResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":        "cSignerInner",
		"innerAction": innerAction,
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

	var result ValidatorResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CValidator Methods

// CValidatorRegister registers as consensus validator.
func (c *Client) CValidatorRegister(validatorProfile map[string]any) (*ValidatorResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":             "cValidatorRegister",
		"validatorProfile": validatorProfile,
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

	var result ValidatorResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CValidatorChangeProfile changes validator profile.
func (c *Client) CValidatorChangeProfile(newProfile map[string]any) (*ValidatorResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":       "cValidatorChangeProfile",
		"newProfile": newProfile,
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

	var result ValidatorResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CValidatorUnregister unregisters as consensus validator.
func (c *Client) CValidatorUnregister() (*ValidatorResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type": "cValidatorUnregister",
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

	var result ValidatorResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
