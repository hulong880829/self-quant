package trade

import (
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
)

// StakeGroup exposes HYPE staking actions on Client.
type StakeGroup struct {
	t *Client
}

// Delegate stakes wei units of HYPE to validator.
func (g *StakeGroup) Delegate(validator string, wei int) (*TransferResponse, error) {
	return g.tokenDelegate(validator, wei, false)
}

// Undelegate unstakes wei units of HYPE from validator.
func (g *StakeGroup) Undelegate(validator string, wei int) (*TransferResponse, error) {
	return g.tokenDelegate(validator, wei, true)
}

// tokenDelegate signs and submits a tokenDelegate action.
func (g *StakeGroup) tokenDelegate(validator string, wei int, isUndelegate bool) (*TransferResponse, error) {
	t := g.t
	nonce := time.Now().UnixMilli()
	action := map[string]any{
		"type":         "tokenDelegate",
		"validator":    validator,
		"wei":          int64(wei),
		"isUndelegate": isUndelegate,
		"nonce":        nonce,
	}
	var result TransferResponse
	if err := t.executeUserSignedAction(
		action, signing.TokenDelegateSignTypes,
		"HyperliquidTransaction:TokenDelegate", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}
