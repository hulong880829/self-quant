package portfolio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

const (
	testWalletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	testWalletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	otherWalletAddr   = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
)

func TestWalletDEXHelpers(t *testing.T) {
	for _, value := range []string{"", "  ", "0", "0.0", "0.00", "0.000"} {
		if !walletDEXZeroOrEmpty(value) {
			t.Fatalf("expected zero-or-empty %q", value)
		}
	}
	if walletDEXZeroOrEmpty("19.502755") || walletDEXZeroOrEmpty("abc") {
		t.Fatal("nonzero or invalid should not be treated as empty")
	}
	for _, asset := range []string{"USDC", "usdt", "USD"} {
		if !walletDEXStableUSD(asset) {
			t.Fatalf("expected stable %q", asset)
		}
	}
	if walletDEXStableUSD("BTC") || walletDEXStableUSD("HYPE") {
		t.Fatal("non-stable treated as USD")
	}
	if got := walletDEXAdd("0", "", "19.502755"); got != "19.502755" {
		t.Fatalf("add spot-only=%q", got)
	}
	if got := walletDEXAdd("1000", "250"); got != "1250" {
		t.Fatalf("add both=%q", got)
	}
	if got := walletDEXRiskPercent("5", "100"); got != "5" {
		t.Fatalf("risk=%q", got)
	}
	if got := walletDEXRiskPercent("5", "0"); got != "" {
		t.Fatalf("zero-equity risk=%q", got)
	}
	if got := walletDEXRiskPercent("5", ""); got != "" {
		t.Fatalf("empty-equity risk=%q", got)
	}
}

func TestHyperliquidSnapshotParsesPerpSpotAndHIP3(t *testing.T) {
	var sawXYZ bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Type string `json:"type"`
			User string `json:"user"`
			Dex  string `json:"dex"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		if payload.Type != "allMids" && payload.User != testWalletAddress {
			t.Errorf("user=%q", payload.User)
		}
		switch payload.Type {
		case "clearinghouseState":
			if payload.Dex == "xyz" {
				sawXYZ = true
				writeFixture(w, `{
					"marginSummary":{"accountValue":"250","totalMarginUsed":"10"},
					"withdrawable":"50",
					"assetPositions":[
						{"position":{"coin":"xyz:BTC","szi":"1","entryPx":"1","positionValue":"1","unrealizedPnl":"0"}}
					]
				}`)
				return
			}
			writeFixture(w, `{
				"marginSummary":{"accountValue":"1000","totalMarginUsed":"40"},
				"crossMaintenanceMarginUsed":"50",
				"withdrawable":"800",
				"assetPositions":[
					{"position":{"coin":"BTC","szi":"0.5","entryPx":"60000","positionValue":"31000","unrealizedPnl":"1000"}},
					{"position":{"coin":"ETH","szi":"-2","entryPx":"3000","positionValue":"6200","unrealizedPnl":"-200"}}
				]
			}`)
		case "spotClearinghouseState":
			writeFixture(w, `{"balances":[{"coin":"USDC","total":"250"},{"coin":"HYPE","total":"10"}]}`)
		case "allMids":
			writeFixture(w, `{}`)
		default:
			t.Errorf("unexpected info type %q", payload.Type)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot, err := newHyperliquid(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawXYZ {
		t.Fatal("expected clearinghouseState with dex=xyz")
	}
	if snapshot.AccountEquityUSD != "250" || snapshot.AvailableFundsUSD != "250" {
		t.Fatalf("equity/available=%+v", snapshot)
	}
	if snapshot.RiskPercent != walletDEXRiskPercent(walletDEXAdd("50", "10"), "250") {
		t.Fatalf("risk=%q", snapshot.RiskPercent)
	}
	if snapshot.SpotBalances["USDC"] != "250" || snapshot.SpotBalances["HYPE"] != "10" {
		t.Fatalf("spot=%+v", snapshot.SpotBalances)
	}
	if len(snapshot.Positions) != 3 {
		t.Fatalf("positions=%+v", snapshot.Positions)
	}
	byWire := map[string]Position{}
	for _, position := range snapshot.Positions {
		byWire[position.WireSymbol] = position
	}
	long := byWire["BTC"]
	short := byWire["ETH"]
	hip3 := byWire["xyz:BTC"]
	if long.Side != "long" || long.Size != "0.5" || long.WireSymbol != "BTC" ||
		long.BaseAsset != "BTC" || long.SignedContractSize != "0.5" || long.Kind != "cex" ||
		long.Key != "hyperliquid:BTCUSDC:long" {
		t.Fatalf("long=%+v", long)
	}
	if short.Side != "short" || short.Size != "2" || short.SignedContractSize != "-2" {
		t.Fatalf("short=%+v", short)
	}
	if hip3.Side != "long" || hip3.Size != "1" || hip3.BaseAsset != "BTC" ||
		hip3.WireSymbol != "xyz:BTC" || hip3.Key != "hyperliquid:xyz:BTC:long" {
		t.Fatalf("hip3=%+v", hip3)
	}
}

func TestHyperliquidSpotOnlyWhenPerpAccountValueIsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		switch payload.Type {
		case "clearinghouseState":
			writeFixture(w, `{"marginSummary":{"accountValue":"0"},"withdrawable":"0","assetPositions":[]}`)
		case "spotClearinghouseState":
			writeFixture(w, `{"balances":[{"coin":"USDC","token":0,"hold":"0","total":"19.502755"}]}`)
		default:
			writeFixture(w, `{}`)
		}
	}))
	defer server.Close()

	snapshot, err := newHyperliquid(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountEquityUSD != "19.502755" || snapshot.AvailableFundsUSD != "19.502755" {
		t.Fatalf("equity/available=%+v", snapshot)
	}
	if len(snapshot.Positions) != 0 {
		t.Fatalf("positions=%+v", snapshot.Positions)
	}
	if snapshot.RiskPercent != "" {
		t.Fatalf("risk=%q", snapshot.RiskPercent)
	}
}

func TestHyperliquidPerpOnlyAndSpotHoldAndMids(t *testing.T) {
	t.Run("perp only", func(t *testing.T) {
		snapshot := hyperliquidSnapshot(t, `{
			"marginSummary":{"accountValue":"400"},
			"withdrawable":"300",
			"assetPositions":[]
		}`, `{"balances":[]}`, `{}`)
		if snapshot.AccountEquityUSD != "0" || snapshot.AvailableFundsUSD != "0" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
		if len(snapshot.Positions) != 0 {
			t.Fatalf("positions=%+v", snapshot.Positions)
		}
	})
	t.Run("hold reduces available", func(t *testing.T) {
		snapshot := hyperliquidSnapshot(t, `{
			"marginSummary":{"accountValue":"0"},
			"withdrawable":"0",
			"assetPositions":[]
		}`, `{"balances":[{"coin":"USDC","token":0,"hold":"5","total":"20"}]}`, `{}`)
		if snapshot.AccountEquityUSD != "20" || snapshot.AvailableFundsUSD != "15" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("tokenToAvailableAfterMaintenance is ignored", func(t *testing.T) {
		snapshot := hyperliquidSnapshot(t, `{
			"marginSummary":{"accountValue":"0"},
			"withdrawable":"0",
			"assetPositions":[]
		}`, `{
			"balances":[{"coin":"USDC","token":0,"hold":"5","total":"20"}],
			"tokenToAvailableAfterMaintenance":[[0,"18.5"]]
		}`, `{}`)
		if snapshot.AccountEquityUSD != "20" || snapshot.AvailableFundsUSD != "15" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("non-stable priced from allMids", func(t *testing.T) {
		snapshot := hyperliquidSnapshot(t, `{
			"marginSummary":{"accountValue":"100"},
			"withdrawable":"80",
			"assetPositions":[]
		}`, `{"balances":[{"coin":"HYPE","token":150,"total":"10"},{"coin":"USDC","total":"25"}]}`,
			`{"HYPE":"2","@150":"2"}`)
		if snapshot.AccountEquityUSD != "45" || snapshot.AvailableFundsUSD != "25" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
		if snapshot.SpotBalances["HYPE"] != "10" {
			t.Fatalf("spot=%+v", snapshot.SpotBalances)
		}
	})
	t.Run("allMids failure does not zero snapshot", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				Type string `json:"type"`
				Dex  string `json:"dex"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload.Type == "allMids" {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			if payload.Type == "clearinghouseState" {
				if payload.Dex == "xyz" {
					writeFixture(w, hyperliquidEmptyXYZClearinghouse)
					return
				}
				writeFixture(w, `{"marginSummary":{"accountValue":"100"},"withdrawable":"80","assetPositions":[]}`)
				return
			}
			writeFixture(w, `{"balances":[{"coin":"HYPE","total":"10"},{"coin":"USDC","total":"25"}]}`)
		}))
		defer server.Close()
		snapshot, err := newHyperliquid(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
			APIKey: testWalletAddress, APISecret: testWalletKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.AccountEquityUSD != "25" || snapshot.AvailableFundsUSD != "25" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("numeric json amounts", func(t *testing.T) {
		snapshot := hyperliquidSnapshot(t, `{
			"marginSummary":{"accountValue":0},
			"withdrawable":0,
			"assetPositions":[]
		}`, `{"balances":[{"coin":"USDC","token":0,"hold":0,"total":19.5}]}`, `{}`)
		if snapshot.AccountEquityUSD != "19.5" || snapshot.AvailableFundsUSD != "19.5" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("zero equity risk stays empty", func(t *testing.T) {
		snapshot := hyperliquidSnapshot(t, `{
			"marginSummary":{"accountValue":"0","totalMarginUsed":"5"},
			"crossMaintenanceMarginUsed":"5",
			"withdrawable":"0",
			"assetPositions":[]
		}`, `{"balances":[]}`, `{}`)
		if snapshot.AccountEquityUSD != "0" || snapshot.RiskPercent != "" {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
}

func TestHyperliquidSnapshotFailsClosedOnUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()
	if _, err := newHyperliquid(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	}); err == nil {
		t.Fatal("expected clearinghouse failure")
	}
}

func TestHyperliquidSnapshotContinuesWhenHIP3ClearinghouseFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Type string `json:"type"`
			Dex  string `json:"dex"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		switch payload.Type {
		case "clearinghouseState":
			if payload.Dex == "xyz" {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			writeFixture(w, `{
				"marginSummary":{"accountValue":"400"},
				"withdrawable":"300",
				"assetPositions":[
					{"position":{"coin":"BTC","szi":"0.5","entryPx":"1","positionValue":"1","unrealizedPnl":"0"}}
				]
			}`)
		case "spotClearinghouseState":
			writeFixture(w, `{"balances":[]}`)
		default:
			writeFixture(w, `{}`)
		}
	}))
	defer server.Close()
	snapshot, err := newHyperliquid(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountEquityUSD != "0" || snapshot.AvailableFundsUSD != "0" {
		t.Fatalf("equity/available=%+v", snapshot)
	}
	if len(snapshot.Positions) != 1 || snapshot.Positions[0].WireSymbol != "BTC" {
		t.Fatalf("positions=%+v", snapshot.Positions)
	}
}

func TestHyperliquidUnifiedAccountIgnoresPerpAccountValue(t *testing.T) {
	snapshot := hyperliquidSnapshot(t, `{
		"marginSummary":{"accountValue":"162.76","totalMarginUsed":"40"},
		"crossMaintenanceMarginUsed":"50",
		"withdrawable":"106.68",
		"assetPositions":[
			{"position":{"coin":"CASHCAT","szi":"-1427","entryPx":"0.2022","positionValue":"299.2419","unrealizedPnl":"-10.749599"}}
		]
	}`, `{"balances":[{"coin":"USDC","token":0,"hold":"155.82","total":"231.28"}]}`, `{}`)
	if snapshot.AccountEquityUSD != "231.28" || snapshot.AvailableFundsUSD != "75.46" {
		t.Fatalf("equity/available=%+v", snapshot)
	}
	if snapshot.RiskPercent != walletDEXRiskPercent("50", "231.28") {
		t.Fatalf("risk=%q", snapshot.RiskPercent)
	}
	if snapshot.RiskPercent == walletDEXRiskPercent("50", "394.04") {
		t.Fatal("risk still used inflated perp+spot equity")
	}
	if len(snapshot.Positions) != 1 || snapshot.Positions[0].WireSymbol != "CASHCAT" {
		t.Fatalf("positions=%+v", snapshot.Positions)
	}
}

func TestAsterSnapshotSignsUserDataAndNeverOrders(t *testing.T) {
	var sawOrder bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "order") {
			sawOrder = true
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("user") != otherWalletAddr ||
			query.Get("signer") != testWalletAddress ||
			query.Get("nonce") == "" ||
			query.Get("signature") == "" {
			t.Errorf("missing v3 auth: %s", r.URL.RawQuery)
		}
		if !strings.HasPrefix(query.Get("signature"), "0x") {
			t.Errorf("signature=%q", query.Get("signature"))
		}
		switch r.URL.Path {
		case "/fapi/v3/account":
			writeFixture(w, `{
				"totalWalletBalance":"90",
				"totalMarginBalance":"100",
				"availableBalance":"80",
				"totalMaintMargin":"5",
				"assets":[{"asset":"USDT","walletBalance":"90"},{"asset":"BTC","walletBalance":"1.5"}]
			}`)
		case "/fapi/v3/positionRisk":
			writeFixture(w, `[
				{"symbol":"BTCUSDT","positionAmt":"2","positionSide":"BOTH","entryPrice":"10","markPrice":"12","unRealizedProfit":"4","notional":"24"},
				{"symbol":"ETHUSDT","positionAmt":"-1","positionSide":"SHORT","entryPrice":"20","markPrice":"18","unRealizedProfit":"2","notional":"-18"}
			]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot, err := newAster(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: otherWalletAddr, APISecret: testWalletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawOrder {
		t.Fatal("aster snapshot called an order endpoint")
	}
	if snapshot.AccountEquityUSD != "100" || snapshot.AvailableFundsUSD != "80" {
		t.Fatalf("equity/available=%+v", snapshot)
	}
	if snapshot.RiskPercent != "5" {
		t.Fatalf("risk=%q", snapshot.RiskPercent)
	}
	if snapshot.SpotBalances["USDT"] != "90" || snapshot.SpotBalances["BTC"] != "1.5" {
		t.Fatalf("spot=%+v", snapshot.SpotBalances)
	}
	if len(snapshot.Positions) != 2 {
		t.Fatalf("positions=%+v", snapshot.Positions)
	}
	if snapshot.Positions[0].Size != "2" || snapshot.Positions[0].SignedContractSize != "2" ||
		snapshot.Positions[0].Kind != "cex" {
		t.Fatalf("long=%+v", snapshot.Positions[0])
	}
	if snapshot.Positions[1].Side != "short" || snapshot.Positions[1].SignedContractSize != "-1" {
		t.Fatalf("short=%+v", snapshot.Positions[1])
	}

	key, err := crypto.HexToECDSA(strings.TrimPrefix(testWalletKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(key.PublicKey).Hex() != testWalletAddress {
		t.Fatal("test key does not match expected signer")
	}
}

func TestAsterEquityAndAvailableFallbacks(t *testing.T) {
	t.Run("zero totals sum stable assets", func(t *testing.T) {
		snapshot := asterSnapshot(t, `{
			"totalWalletBalance":"0",
			"totalMarginBalance":"0",
			"availableBalance":"0",
			"totalMaintMargin":"0",
			"assets":[
				{"asset":"USDT","walletBalance":"40","availableBalance":"35"},
				{"asset":"USDC","walletBalance":"10","availableBalance":"10"},
				{"asset":"BTC","walletBalance":"1.5","availableBalance":"1.5"}
			]
		}`, `[]`)
		if snapshot.AccountEquityUSD != "50" || snapshot.AvailableFundsUSD != "45" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
		if snapshot.RiskPercent != "" {
			t.Fatalf("risk=%q", snapshot.RiskPercent)
		}
	})
	t.Run("native equity is not double counted", func(t *testing.T) {
		snapshot := asterSnapshot(t, `{
			"totalWalletBalance":"90",
			"totalMarginBalance":"100",
			"availableBalance":"80",
			"assets":[{"asset":"USDT","walletBalance":"90","availableBalance":"80"}]
		}`, `[]`)
		if snapshot.AccountEquityUSD != "100" || snapshot.AvailableFundsUSD != "80" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("available is not replaced by equity", func(t *testing.T) {
		snapshot := asterSnapshot(t, `{
			"totalMarginBalance":"100",
			"availableBalance":"0",
			"assets":[{"asset":"USDT","walletBalance":"100","availableBalance":"0"}]
		}`, `[]`)
		if snapshot.AccountEquityUSD != "100" || snapshot.AvailableFundsUSD != "0" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("empty mark filled from notional", func(t *testing.T) {
		snapshot := asterSnapshot(t, `{
			"totalMarginBalance":"100",
			"availableBalance":"80",
			"assets":[]
		}`, `[{"symbol":"BTCUSDT","positionAmt":"2","positionSide":"BOTH","entryPrice":"10","markPrice":"","unRealizedProfit":"4","notional":"24"}]`)
		if len(snapshot.Positions) != 1 || snapshot.Positions[0].MarkPrice != "12" {
			t.Fatalf("positions=%+v", snapshot.Positions)
		}
	})
	t.Run("perp only no assets", func(t *testing.T) {
		snapshot := asterSnapshot(t, `{
			"totalMarginBalance":"70",
			"availableBalance":"60",
			"assets":[]
		}`, `[]`)
		if snapshot.AccountEquityUSD != "70" || snapshot.AvailableFundsUSD != "60" || len(snapshot.Positions) != 0 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
}

func TestAsterSnapshotFailsClosedOnPositionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fapi/v3/account" {
			writeFixture(w, `{"totalMarginBalance":"100","availableBalance":"80","assets":[]}`)
			return
		}
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()

	if _, err := newAster(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	}); err == nil {
		t.Fatal("expected positionRisk failure")
	}
}

func TestLighterSnapshotUsesOnlyRequestedAccountIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/account" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("by") != "index" ||
			r.URL.Query().Get("value") != "2" ||
			r.URL.Query().Get("active_only") != "true" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		writeFixture(w, `{
			"code":200,
			"accounts":[
				{
					"account_index":1,
					"collateral":"100",
					"available_balance":"80",
					"cross_maintenance_margin_requirement":"8",
					"positions":[
						{"symbol":"ETH","sign":1,"position":"1.5","avg_entry_price":"3000","position_value":"4650","unrealized_pnl":"20"}
					]
				},
				{
					"account_index":2,
					"collateral":"50",
					"available_balance":"20",
					"cross_maintenance_margin_requirement":"2",
					"positions":[
						{"symbol":"BTC","sign":-1,"position":"0.25","avg_entry_price":"60000","position_value":"15500","unrealized_pnl":"-10"}
					]
				}
			]
		}`)
	}))
	defer server.Close()

	accountIndex := int64(2)
	snapshot, err := newLighter(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey, AccountIndex: &accountIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountEquityUSD != "40" || snapshot.AvailableFundsUSD != "20" {
		t.Fatalf("equity/available=%+v", snapshot)
	}
	if snapshot.RiskPercent != "5" {
		t.Fatalf("risk=%q", snapshot.RiskPercent)
	}
	if len(snapshot.SpotBalances) != 0 {
		t.Fatalf("spot=%+v", snapshot.SpotBalances)
	}
	if len(snapshot.Positions) != 1 {
		t.Fatalf("positions=%+v", snapshot.Positions)
	}
	if snapshot.Positions[0].Side != "short" || snapshot.Positions[0].SignedContractSize != "-0.25" ||
		!strings.Contains(snapshot.Positions[0].Key, "2:") {
		t.Fatalf("short=%+v", snapshot.Positions[0])
	}
}

func TestLighterDoesNotUseAvailableAsEquity(t *testing.T) {
	t.Run("single account from account field", func(t *testing.T) {
		snapshot := lighterSnapshot(t, 7, `{
			"code":200,
			"account":{
				"account_index":7,
				"collateral":"40",
				"available_balance":"15",
				"positions":[{"symbol":"ETH","sign":1,"position":"1","avg_entry_price":"1","position_value":"2","unrealized_pnl":"5"}]
			}
		}`)
		if snapshot.AccountEquityUSD != "45" || snapshot.AvailableFundsUSD != "15" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("missing collateral uses upnl only", func(t *testing.T) {
		snapshot := lighterSnapshot(t, 1, `{
			"code":200,
			"accounts":[{
				"account_index":1,
				"available_balance":"0",
				"positions":[{"symbol":"ETH","sign":1,"position":"1","avg_entry_price":"1","position_value":"2","unrealized_pnl":"8"}]
			}]
		}`)
		if snapshot.AccountEquityUSD != "8" || snapshot.AvailableFundsUSD != "0" {
			t.Fatalf("equity/available=%+v", snapshot)
		}
	})
	t.Run("collateral only no positions", func(t *testing.T) {
		snapshot := lighterSnapshot(t, 1, `{
			"code":200,
			"accounts":[{"account_index":1,"collateral":"33","available_balance":"30","positions":[]}]
		}`)
		if snapshot.AccountEquityUSD != "33" || snapshot.AvailableFundsUSD != "30" || len(snapshot.Positions) != 0 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("zero equity risk stays empty", func(t *testing.T) {
		snapshot := lighterSnapshot(t, 1, `{
			"code":200,
			"accounts":[{"account_index":1,"collateral":"0","available_balance":"0","cross_maintenance_margin_requirement":"4","positions":[]}]
		}`)
		if snapshot.AccountEquityUSD != "0" || snapshot.RiskPercent != "" {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
}

func TestLighterSnapshotFailsClosedOnUpstreamCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(w, `{"code":400,"message":"not found","accounts":[]}`)
	}))
	defer server.Close()
	accountIndex := int64(1)
	if _, err := newLighter(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey, AccountIndex: &accountIndex,
	}); err == nil {
		t.Fatal("expected failure")
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(w, `{"code":200,"accounts":[]}`)
	}))
	defer empty.Close()
	if _, err := newLighter(empty.Client(), empty.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey, AccountIndex: &accountIndex,
	}); err == nil {
		t.Fatal("expected no-accounts failure")
	}
	if _, err := newLighter(empty.Client(), empty.URL).Snapshot(
		context.Background(), Credentials{APIKey: testWalletAddress},
	); err == nil {
		t.Fatal("expected missing account index failure")
	}
	mismatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(w, `{"code":200,"accounts":[{"account_index":2,"collateral":"1"}]}`)
	}))
	defer mismatch.Close()
	if _, err := newLighter(mismatch.Client(), mismatch.URL).Snapshot(
		context.Background(), Credentials{APIKey: testWalletAddress, AccountIndex: &accountIndex},
	); err == nil {
		t.Fatal("expected mismatched account index failure")
	}
}

func hyperliquidSnapshot(t *testing.T, perp, spot, mids string) Snapshot {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Type string `json:"type"`
			Dex  string `json:"dex"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		switch payload.Type {
		case "clearinghouseState":
			if payload.Dex == "xyz" {
				writeFixture(w, hyperliquidEmptyXYZClearinghouse)
				return
			}
			writeFixture(w, perp)
		case "spotClearinghouseState":
			writeFixture(w, spot)
		case "allMids":
			writeFixture(w, mids)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	snapshot, err := newHyperliquid(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

const hyperliquidEmptyXYZClearinghouse = `{"marginSummary":{"accountValue":"0"},"withdrawable":"0","assetPositions":[]}`

func asterSnapshot(t *testing.T, account, positions string) Snapshot {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v3/account":
			writeFixture(w, account)
		case "/fapi/v3/positionRisk":
			writeFixture(w, positions)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	snapshot, err := newAster(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func lighterSnapshot(t *testing.T, accountIndex int64, body string) Snapshot {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(w, body)
	}))
	t.Cleanup(server.Close)
	snapshot, err := newLighter(server.Client(), server.URL).Snapshot(context.Background(), Credentials{
		APIKey: testWalletAddress, APISecret: testWalletKey, AccountIndex: &accountIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
