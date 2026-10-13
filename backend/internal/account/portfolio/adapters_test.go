package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnifiedAdaptersParseNonZeroPositionsAndSignRequests(t *testing.T) {
	tests := []struct {
		name           string
		adapter        func(*http.Client, string) Adapter
		handler        func(*testing.T, http.ResponseWriter, *http.Request)
		spot, contract string
	}{
		{"binance", newBinance, binanceFixture, "1.5", "2"},
		{"okx", newOKX, okxFixture, "1.5", "2"},
		{"bitget", newBitget, bitgetFixture, "1.5", "2"},
		{"bybit", newBybit, bybitFixture, "1.5", "2"},
		{"gate", newGate, gateFixture, "-1.5", "-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				test.handler(t, w, r)
			}))
			defer server.Close()
			snapshot, err := test.adapter(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
				APIKey: "key", APISecret: "secret", Passphrase: "pass",
			})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.AccountEquityUSD == "" || snapshot.AvailableFundsUSD == "" {
				t.Fatal("account equity or available funds missing")
			}
			if len(snapshot.Positions) == 0 {
				t.Fatal("position missing")
			}
			if snapshot.Positions[0].NotionalUSD == "" || snapshot.Positions[0].Side == "" {
				t.Fatalf("invalid position: %+v", snapshot.Positions[0])
			}
			if snapshot.Positions[0].SpotSize != test.spot {
				t.Fatalf("spot size=%q snapshot=%+v", snapshot.Positions[0].SpotSize, snapshot)
			}
			if snapshot.Positions[0].SignedContractSize != test.contract {
				t.Fatalf("signed contract size=%q", snapshot.Positions[0].SignedContractSize)
			}
			if snapshot.Positions[0].LiquidationPrice != "8" {
				t.Fatalf("liquidation price=%q", snapshot.Positions[0].LiquidationPrice)
			}
		})
	}
}

func TestBinanceUsesActualEquityWithoutFallback(t *testing.T) {
	tests := []struct {
		name       string
		account    string
		wantEquity string
	}{
		{
			name: "actual equity wins",
			account: `{"accountEquity":"100","actualEquity":"101",` +
				`"totalAccountEquity":"102","totalAvailableBalance":"80"}`,
			wantEquity: "101",
		},
		{
			name: "missing actual equity stays empty",
			account: `{"accountEquity":"100","totalAccountEquity":"102",` +
				`"totalAvailableBalance":"80"}`,
			wantEquity: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/papi/v1/account" {
						writeFixture(w, test.account)
						return
					}
					writeFixture(w, `[]`)
				},
			))
			defer server.Close()
			snapshot, err := newBinance(server.Client(), server.URL).
				Snapshot(context.Background(), Credentials{
					APIKey: "key", APISecret: "secret",
				})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.AccountEquityUSD != test.wantEquity {
				t.Fatalf("account equity=%q want=%q",
					snapshot.AccountEquityUSD, test.wantEquity)
			}
		})
	}
}

func TestOKXCanonicalizesXStockSpotBalances(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v5/account/balance":
				writeFixture(w, `{"code":"0","data":[{"totalEq":"100","availEq":"80","details":[`+
					`{"ccy":"XGOOGL","cashBal":"0.58","liab":"0"},`+
					`{"ccy":"XRP","cashBal":"2","liab":"0"},`+
					`{"ccy":"XLM","cashBal":"3","liab":"0"}]}]}`)
			case "/api/v5/account/positions":
				writeFixture(w, `{"code":"0","data":[{"instId":"GOOGL-USDT-SWAP",`+
					`"pos":"0.58","posSide":"short","avgPx":"346.4779",`+
					`"markPx":"346.77","upl":"-0.1694","notionalUsd":"200.9174",`+
					`"mmr":"1"}]}`)
			default:
				http.NotFound(w, r)
			}
		},
	))
	defer server.Close()

	snapshot, err := newOKX(server.Client(), server.URL).Snapshot(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "pass"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for asset, want := range map[string]string{
		"GOOGL": "0.58",
		"XRP":   "2",
		"XLM":   "3",
	} {
		if got := snapshot.SpotBalances[asset]; got != want {
			t.Fatalf("spot balance %s=%q want=%q snapshot=%+v",
				asset, got, want, snapshot.SpotBalances)
		}
	}
	if _, exists := snapshot.SpotBalances["XGOOGL"]; exists {
		t.Fatalf("raw xStock key leaked: %+v", snapshot.SpotBalances)
	}
	if len(snapshot.Positions) != 1 ||
		snapshot.Positions[0].SpotSize != "0.58" ||
		snapshot.Positions[0].SignedContractSize != "-0.58" {
		t.Fatalf("GOOGL position was not paired: %+v", snapshot.Positions)
	}
}

func TestBitgetSnapshotAccountFields(t *testing.T) {
	const (
		defaultEquity = `"accountEquity":"100"`
		defaultEff    = `"effEquity":"80"`
		defaultIMR    = `"imr":"20"`
		defaultRatio  = `"mgnRatio":"0.0625"`
		assetsBTC     = `"assets":[{"coin":"BTC","balance":"2","debt":"0.5"}]`
	)
	wrap := func(fields ...string) string {
		return `{"code":"00000","data":{` + strings.Join(fields, ",") + `}}`
	}
	tests := []struct {
		name       string
		assets     string
		wantEquity string
		wantAvail  string
		wantRisk   string
	}{
		{
			name:       "official caliber",
			assets:     wrap(defaultEquity, defaultEff, defaultIMR, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "60",
			wantRisk:   "6.25",
		},
		{
			name:       "zero imr and mgnRatio",
			assets:     wrap(defaultEquity, defaultEff, `"imr":"0"`, `"mgnRatio":"0"`, assetsBTC),
			wantEquity: "100",
			wantAvail:  "80",
			wantRisk:   "0",
		},
		{
			name:       "imr greater than effEquity clamps available to 0",
			assets:     wrap(defaultEquity, defaultEff, `"imr":"90"`, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "0",
			wantRisk:   "6.25",
		},
		{
			name:       "negative effEquity clamps available to 0",
			assets:     wrap(defaultEquity, `"effEquity":"-10"`, `"imr":"0"`, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "0",
			wantRisk:   "6.25",
		},
		{
			name:       "mgnRatio above 1 is not truncated",
			assets:     wrap(defaultEquity, defaultEff, defaultIMR, `"mgnRatio":"1.2"`, assetsBTC),
			wantEquity: "100",
			wantAvail:  "60",
			wantRisk:   "120",
		},
		{
			name:       "missing mgnRatio leaves risk empty",
			assets:     wrap(defaultEquity, defaultEff, defaultIMR, assetsBTC),
			wantEquity: "100",
			wantAvail:  "60",
			wantRisk:   "",
		},
		{
			name:       "illegal mgnRatio leaves risk empty",
			assets:     wrap(defaultEquity, defaultEff, defaultIMR, `"mgnRatio":"n/a"`, assetsBTC),
			wantEquity: "100",
			wantAvail:  "60",
			wantRisk:   "",
		},
		{
			name:       "negative mgnRatio leaves risk empty",
			assets:     wrap(defaultEquity, defaultEff, defaultIMR, `"mgnRatio":"-0.01"`, assetsBTC),
			wantEquity: "100",
			wantAvail:  "60",
			wantRisk:   "",
		},
		{
			name:       "missing accountEquity leaves equity empty",
			assets:     wrap(defaultEff, defaultIMR, defaultRatio, assetsBTC),
			wantEquity: "",
			wantAvail:  "60",
			wantRisk:   "6.25",
		},
		{
			name:       "illegal accountEquity leaves equity empty",
			assets:     wrap(`"accountEquity":"n/a"`, defaultEff, defaultIMR, defaultRatio, assetsBTC),
			wantEquity: "",
			wantAvail:  "60",
			wantRisk:   "6.25",
		},
		{
			name:       "missing effEquity leaves available empty without equity fallback",
			assets:     wrap(defaultEquity, defaultIMR, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "",
			wantRisk:   "6.25",
		},
		{
			name:       "illegal effEquity leaves available empty without equity fallback",
			assets:     wrap(defaultEquity, `"effEquity":"n/a"`, defaultIMR, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "",
			wantRisk:   "6.25",
		},
		{
			name:       "missing imr leaves available empty without equity fallback",
			assets:     wrap(defaultEquity, defaultEff, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "",
			wantRisk:   "6.25",
		},
		{
			name:       "illegal imr leaves available empty without equity fallback",
			assets:     wrap(defaultEquity, defaultEff, `"imr":"n/a"`, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "",
			wantRisk:   "6.25",
		},
		{
			name:       "negative imr leaves available empty without equity fallback",
			assets:     wrap(defaultEquity, defaultEff, `"imr":"-1"`, defaultRatio, assetsBTC),
			wantEquity: "100",
			wantAvail:  "",
			wantRisk:   "6.25",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v3/account/assets" {
					writeFixture(w, test.assets)
					return
				}
				if r.URL.Query().Get("category") == "USDT-FUTURES" {
					writeFixture(w, `{"code":"00000","data":{"list":[{"symbol":"BTCUSDT","posSide":"long","total":"2","avgPrice":"10","markPrice":"12","unrealisedPnl":"4","positionBalance":"8","leverage":"3"}]}}`)
					return
				}
				writeFixture(w, `{"code":"00000","data":{"list":[]}}`)
			}))
			defer server.Close()
			snapshot, err := newBitget(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
				APIKey: "key", APISecret: "secret", Passphrase: "pass",
			})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.AccountEquityUSD != test.wantEquity {
				t.Fatalf("account equity=%q want=%q", snapshot.AccountEquityUSD, test.wantEquity)
			}
			if snapshot.AvailableFundsUSD != test.wantAvail {
				t.Fatalf("available=%q want=%q", snapshot.AvailableFundsUSD, test.wantAvail)
			}
			if snapshot.RiskPercent != test.wantRisk {
				t.Fatalf("risk=%q want=%q", snapshot.RiskPercent, test.wantRisk)
			}
			if len(snapshot.Positions) == 0 {
				t.Fatal("positions missing")
			}
		})
	}
}

func TestCanonicalOKXSpotAssetRequiresMatchingPerpetual(t *testing.T) {
	positionAssets := map[string]struct{}{
		"AAPL": {},
		"XRP":  {},
	}
	for asset, want := range map[string]string{
		"XAAPL": "AAPL",
		"xAAPL": "AAPL",
		"XRP":   "XRP",
		"XLM":   "XLM",
		"XAUT":  "XAUT",
	} {
		if got := canonicalOKXSpotAsset(asset, positionAssets); got != want {
			t.Fatalf("canonicalOKXSpotAsset(%q)=%q want=%q", asset, got, want)
		}
	}
}

func writeFixture(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, value)
}

func binanceFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-MBX-APIKEY") != "key" || r.URL.Query().Get("signature") == "" {
		t.Error("binance request is unsigned")
	}
	switch r.URL.Path {
	case "/papi/v1/account":
		writeFixture(w, `{"accountEquity":"99","actualEquity":"100","totalAccountEquity":"101","totalAvailableBalance":"80","accountMaintMargin":"5"}`)
	case "/papi/v1/balance":
		writeFixture(w, `[{"asset":"BTC","crossMarginAsset":"2","crossMarginBorrowed":"0.5"}]`)
	case "/papi/v1/um/positionRisk":
		writeFixture(w, `[{"symbol":"BTCUSDT","positionAmt":"2","entryPrice":"10","markPrice":"12","liquidationPrice":"8","unRealizedProfit":"4","notional":"24"}]`)
	default:
		writeFixture(w, `[]`)
	}
}

func okxFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("OK-ACCESS-SIGN") == "" {
		t.Error("okx request is unsigned")
	}
	if r.URL.Path == "/api/v5/account/balance" {
		writeFixture(w, `{"code":"0","data":[{"totalEq":"100","availEq":"80","details":[{"ccy":"BTC","cashBal":"2","liab":"0.5"}]}]}`)
	} else {
		writeFixture(w, `{"code":"0","data":[{"instId":"BTC-USDT-SWAP","pos":"2","posSide":"long","avgPx":"10","markPx":"12","liqPx":"8","upl":"4","notionalUsd":"24","mmr":"5"}]}`)
	}
}

func bitgetFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("ACCESS-SIGN") == "" {
		t.Error("bitget request is unsigned")
	}
	if r.URL.Path == "/api/v3/account/assets" {
		writeFixture(w, `{"code":"00000","data":{"accountEquity":"100","effEquity":"80","imr":"20","mgnRatio":"0.0625","assets":[{"coin":"BTC","balance":"2","debt":"0.5"}]}}`)
	} else if r.URL.Query().Get("category") == "USDT-FUTURES" {
		writeFixture(w, `{"code":"00000","data":{"list":[{"symbol":"BTCUSDT","posSide":"long","total":"2","avgPrice":"10","markPrice":"12","liquidationPrice":"8","unrealisedPnl":"4","positionBalance":"8","leverage":"3"}]}}`)
	} else {
		writeFixture(w, `{"code":"00000","data":{"list":[]}}`)
	}
}

func bybitFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-BAPI-SIGN") == "" {
		t.Error("bybit request is unsigned")
	}
	if r.URL.Path == "/v5/account/wallet-balance" {
		writeFixture(w, `{"retCode":0,"result":{"list":[{"totalAvailableBalance":"80","totalEquity":"100","totalMaintenanceMargin":"5","coin":[{"coin":"BTC","walletBalance":"2","spotBorrow":"0.5"}]}]}}`)
	} else if r.URL.Query().Get("category") == "linear" && r.URL.Query().Get("settleCoin") == "USDT" {
		writeFixture(w, `{"retCode":0,"result":{"list":[{"symbol":"BTCUSDT","side":"Buy","size":"2","avgPrice":"10","markPrice":"12","liqPrice":"8","positionValue":"24","unrealisedPnl":"4"}],"nextPageCursor":""}}`)
	} else {
		writeFixture(w, `{"retCode":0,"result":{"list":[],"nextPageCursor":""}}`)
	}
}

func gateFixture(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("SIGN") == "" || r.Header.Get("KEY") != "key" {
		t.Error("gate request is unsigned")
	}
	if r.Header.Get("X-Gate-Size-Decimal") != "1" {
		t.Error("gate decimal size header missing")
	}
	switch {
	case r.URL.Path == "/api/v4/unified/accounts":
		writeFixture(w, `{"available_margin":"80","total_margin_balance":"100","total_maintenance_margin":"5","balances":{"BTC":{"equity":"-1.5","borrowed":"1.5"}}}`)
	case strings.Contains(r.URL.Path, "/usdt/"):
		writeFixture(w, `[{"contract":"BTC_USDT","size":-2,"entry_price":"10","mark_price":12,"liq_price":"8","unrealised_pnl":"4","value":24}]`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		writeFixture(w, `{"label":"USER_NOT_FOUND","message":"please transfer funds first to create futures account"}`)
	}
}
