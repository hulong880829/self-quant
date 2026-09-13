package trade

import (
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
)

// Withdraw off-ramps USDC from L1 to destination.
func (c *Client) Withdraw(amount float64, destination string) (*TransferResponse, error) {
	nonce := time.Now().UnixMilli()
	action := map[string]any{
		"type":        "withdraw3",
		"destination": destination,
		"amount":      formatUsdAmount(amount),
		"time":        nonce,
	}
	var result TransferResponse
	if err := c.executeUserSignedAction(
		action, signing.WithdrawSignTypes,
		"HyperliquidTransaction:Withdraw", nonce, &result,
	); err != nil {
		return nil, err
	}
	return &result, nil
}
