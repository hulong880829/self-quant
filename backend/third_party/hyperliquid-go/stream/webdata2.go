package stream

import (
	"encoding/json"
	"fmt"

	"github.com/Simon-Busch/hyperliquid-go/info"
)

// WebData2 is the typed payload carried in the `data` field of a
// webData2 subscription frame (see WebData). It is the aggregate
// per-user snapshot the Hyperliquid frontend consumes: account state,
// resting orders, agent (API wallet) binding and the perp universe.
//
// The account snapshot lives under ClearinghouseState — NOT a field
// named "userState". That key is the same /info clearinghouseState model
// the REST side returns, so it reuses info.UserState verbatim rather than
// redefining it; mis-tagging this field as "userState" decodes to an
// empty value and silently drops every update.
//
// Optional fields are tagged omitempty and may be zero on frames that
// omit them. Monetary quantities are strings, matching the rest of the
// SDK, to preserve exact precision.
type WebData2 struct {
	// ClearinghouseState is the perpetuals account summary (margin
	// summaries, withdrawable, open positions). Same shape as the REST
	// /info {"type":"clearinghouseState"} response.
	ClearinghouseState info.UserState `json:"clearinghouseState"`

	// OpenOrders is the user's resting orders at snapshot time.
	OpenOrders []info.OpenOrder `json:"openOrders,omitempty"`

	// Meta is the perp universe metadata in effect for this snapshot.
	Meta info.Meta `json:"meta"`

	// LeadingVaults lists vaults the user leads. The element shape is
	// left raw so consumers can decode it on demand without this package
	// taking a dependency on an unstable vault model.
	LeadingVaults []json.RawMessage `json:"leadingVaults,omitempty"`

	// TotalVaultEquity is the user's aggregate vault equity (decimal string).
	TotalVaultEquity string `json:"totalVaultEquity,omitempty"`

	// AgentAddress is the bound agent (API wallet) address, when one is set.
	AgentAddress string `json:"agentAddress,omitempty"`

	// AgentValidUntil is the agent binding's expiry as a unix-millis timestamp.
	AgentValidUntil int64 `json:"agentValidUntil,omitempty"`

	// CumLedger is the cumulative ledger value for the user (decimal string).
	CumLedger string `json:"cumLedger,omitempty"`

	// User is the address this snapshot belongs to, when the frame includes it.
	User string `json:"user,omitempty"`
}

// DecodeWebData2 unmarshals the raw data payload of a webData2 frame into
// a typed WebData2. It returns an error only when msg.Data is not valid
// webData2 JSON; absent optional fields are left at their zero value.
//
// Pair it with a WebData subscription:
//
//	c.Stream.Subscribe(stream.WebData(addr), func(m stream.WSMessage) {
//	    wd, err := stream.DecodeWebData2(m)
//	    if err != nil { return }
//	    // wd.ClearinghouseState, wd.OpenOrders, wd.Meta, ...
//	})
func DecodeWebData2(msg WSMessage) (*WebData2, error) {
	var wd WebData2
	if err := json.Unmarshal(msg.Data, &wd); err != nil {
		return nil, fmt.Errorf("decode webData2: %w", err)
	}
	return &wd, nil
}
