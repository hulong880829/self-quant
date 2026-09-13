package stream

import "testing"

// TestSubscriptionConstructors covers every typed Client subscription
// constructor. Each builds a SubscriptionFilter value matching the wire
// shape expected by the Hyperliquid WS API.
func TestSubscriptionConstructors(t *testing.T) {
	cases := []struct {
		name string
		got  SubscriptionFilter
		want SubscriptionFilter
	}{
		{"Trades", Trades("BTC"), SubscriptionFilter{Type: "trades", Coin: "BTC"}},
		{"Book", Book("ETH"), SubscriptionFilter{Type: "l2Book", Coin: "ETH"}},
		{"BBO", BBO("SOL"), SubscriptionFilter{Type: "bbo", Coin: "SOL"}},
		{"ActiveAssetCtx", ActiveAssetCtx("BTC"), SubscriptionFilter{Type: "activeAssetCtx", Coin: "BTC"}},
		{"Candles", Candles("BTC", "1m"), SubscriptionFilter{Type: "candle", Coin: "BTC", Interval: "1m"}},
		{"AllMids", AllMids(), SubscriptionFilter{Type: "allMids"}},
		{"AllMidsOn", AllMidsOn("flx"), SubscriptionFilter{Type: "allMids", Dex: "flx"}},
		{"UserEvents", UserEvents("0xabc"), SubscriptionFilter{Type: "userEvents", User: "0xabc"}},
		{"UserFills", UserFills("0xabc"), SubscriptionFilter{Type: "userFills", User: "0xabc"}},
		{"OrderUpdates", OrderUpdates("0xabc"), SubscriptionFilter{Type: "orderUpdates", User: "0xabc"}},
		{"UserFundings", UserFundings("0xabc"), SubscriptionFilter{Type: "userFundings", User: "0xabc"}},
		{"UserLedger", UserLedger("0xabc"), SubscriptionFilter{Type: "userNonFundingLedgerUpdates", User: "0xabc"}},
		{"WebData", WebData("0xabc"), SubscriptionFilter{Type: "webData2", User: "0xabc"}},
		{"Notifications", Notifications("0xabc"), SubscriptionFilter{Type: "notification", User: "0xabc"}},
		{"ActiveAssetData", ActiveAssetData("0xabc", "BTC"), SubscriptionFilter{Type: "activeAssetData", User: "0xabc", Coin: "BTC"}},
		{"UserTwapFills", UserTwapFills("0xabc"), SubscriptionFilter{Type: "userTwapSliceFills", User: "0xabc"}},
		{"UserTwapHistory", UserTwapHistory("0xabc"), SubscriptionFilter{Type: "userTwapHistory", User: "0xabc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %+v, want %+v", tc.got, tc.want)
			}
		})
	}
}

func TestSubscriptionKey(t *testing.T) {
	cases := []struct {
		name string
		sub  SubscriptionFilter
		want subKey
	}{
		{"allMids", AllMids(), subKey{typ: "allMids"}},
		{"trades BTC", Trades("BTC"), subKey{typ: "trades", coin: "BTC"}},
		{"userEvents 0xabc", UserEvents("0xabc"), subKey{typ: "userEvents", user: "0xabc"}},
		{"candle BTC 1h", Candles("BTC", "1h"), subKey{typ: "candle", coin: "BTC", interval: "1h"}},
		{"allMids flx", AllMidsOn("flx"), subKey{typ: "allMids", dex: "flx"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sub.key(); got != tc.want {
				t.Errorf("key() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
