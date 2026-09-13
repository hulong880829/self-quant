package stream

// Trades returns a subscription filter for the trades feed of coin.
func Trades(coin string) SubscriptionFilter {
	return SubscriptionFilter{Type: "trades", Coin: coin}
}

// Book returns a subscription filter for the L2 order book of coin.
func Book(coin string) SubscriptionFilter {
	return SubscriptionFilter{Type: "l2Book", Coin: coin}
}

// BBO returns a subscription filter for the best bid/offer of coin.
func BBO(coin string) SubscriptionFilter {
	return SubscriptionFilter{Type: "bbo", Coin: coin}
}

// ActiveAssetCtx returns a subscription filter for the active-asset-context
// feed of coin. The channel name matches the WS channel "activeAssetCtx".
func ActiveAssetCtx(coin string) SubscriptionFilter {
	return SubscriptionFilter{Type: "activeAssetCtx", Coin: coin}
}

// Candles returns a subscription filter for coin candles at the supplied
// interval (e.g. "1m", "5m", "1h").
func Candles(coin, interval string) SubscriptionFilter {
	return SubscriptionFilter{Type: "candle", Coin: coin, Interval: interval}
}

// AllMids returns a subscription filter for the global all-mids feed.
func AllMids() SubscriptionFilter {
	return SubscriptionFilter{Type: "allMids"}
}

// AllMidsOn returns a subscription filter for the all-mids feed pinned to a
// HIP-3 dex.
func AllMidsOn(dex string) SubscriptionFilter {
	return SubscriptionFilter{Type: "allMids", Dex: dex}
}

// UserEvents returns a subscription filter for the per-user events stream.
func UserEvents(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "userEvents", User: addr}
}

// UserFills returns a subscription filter for the per-user fills stream.
func UserFills(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "userFills", User: addr}
}

// OrderUpdates returns a subscription filter for the per-user order updates
// stream.
func OrderUpdates(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "orderUpdates", User: addr}
}

// UserFundings returns a subscription filter for the per-user funding stream.
func UserFundings(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "userFundings", User: addr}
}

// UserLedger returns a subscription filter for the per-user non-funding
// ledger.
func UserLedger(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "userNonFundingLedgerUpdates", User: addr}
}

// WebData returns a subscription filter for the per-user web-data feed
// (formerly WebData2).
func WebData(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "webData2", User: addr}
}

// Notifications returns a subscription filter for the per-user notifications
// stream.
func Notifications(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "notification", User: addr}
}

// ActiveAssetData returns a subscription filter for the active-asset-data
// feed for the (addr, coin) pair. The channel name matches the WS channel
// "activeAssetData"; ActiveAssetCtx is its (coin-only) sibling.
func ActiveAssetData(addr, coin string) SubscriptionFilter {
	return SubscriptionFilter{Type: "activeAssetData", User: addr, Coin: coin}
}

// UserTwapFills returns a subscription filter for the per-user TWAP fills
// stream.
func UserTwapFills(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "userTwapSliceFills", User: addr}
}

// UserTwapHistory returns a subscription filter for the per-user TWAP history
// stream.
func UserTwapHistory(addr string) SubscriptionFilter {
	return SubscriptionFilter{Type: "userTwapHistory", User: addr}
}
