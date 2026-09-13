// Package hyperliquid is the top-level facade for the Hyperliquid Go SDK.
//
// Most users construct a single [Client] via [New] and reach the three
// surfaces through fields:
//
//	c, err := hyperliquid.New(
//	    hyperliquid.WithMainnet(),
//	    hyperliquid.WithPrivateKey(pk),
//	    hyperliquid.WithLogger(myLogger),
//	)
//	if err != nil {
//	    return err
//	}
//
//	// Read-only queries.
//	state, err := c.Info.UserState(addr)
//
//	// Signed actions.
//	res, err := c.Trade.PlaceGTC("ETH", types.Buy, 0.1, 3000)
//
//	// Websocket subscriptions.
//	sub, err := c.Stream.Subscribe(stream.Trades("ETH"), handler)
//
// The SDK is split into focused subpackages, all of which can be imported
// directly when finer-grained dependencies are preferable to the facade:
//
//   - [github.com/Simon-Busch/hyperliquid-go/info]    — read-only queries
//     (markets, orders, fills, staking, metadata).
//   - [github.com/Simon-Busch/hyperliquid-go/trade]   — signed actions
//     (place, modify, cancel, transfer, deploy, validator ops).
//   - [github.com/Simon-Busch/hyperliquid-go/stream]  — websocket
//     subscriptions and POST-over-WS.
//   - [github.com/Simon-Busch/hyperliquid-go/signing] — EIP-712 signing
//     helpers for custody integrations and offline signers.
//   - [github.com/Simon-Busch/hyperliquid-go/types]   — shared domain
//     types (Side, OrderSpec, Result, ValidationError, …).
//
// The Trade surface returns [github.com/Simon-Busch/hyperliquid-go/types.Result]
// / [github.com/Simon-Busch/hyperliquid-go/types.BatchResult] /
// [github.com/Simon-Busch/hyperliquid-go/types.CancelResult] /
// [github.com/Simon-Busch/hyperliquid-go/types.BatchCancelResult] for the
// order verbs, and trade-package response types
// (e.g. [github.com/Simon-Busch/hyperliquid-go/trade.TransferResponse],
// [github.com/Simon-Busch/hyperliquid-go/trade.ApprovalResponse],
// [github.com/Simon-Busch/hyperliquid-go/trade.ValidatorResponse]) for
// the account-management verbs. Validation failures return
// *[github.com/Simon-Busch/hyperliquid-go/types.ValidationError];
// server-side HTTP failures return
// *[github.com/Simon-Busch/hyperliquid-go/types.APIError].
//
// The facade in this package is the recommended entry point for typical
// use; advanced callers can import any subpackage directly.
package hyperliquid
