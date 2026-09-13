package portfolio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountFeeRatesParseVenuePayloads(t *testing.T) {
	creds := Credentials{APIKey: "key", APISecret: "secret", Passphrase: "pass"}
	tests := []struct {
		name     string
		adapter  func(*http.Client, string) Adapter
		handler  func(http.ResponseWriter, *http.Request)
		spot     string
		contract string
	}{
		{
			name: "okx", adapter: newOKX, spot: "0.0008", contract: "0.0002",
			handler: func(w http.ResponseWriter, r *http.Request) {
				instType := r.URL.Query().Get("instType")
				maker, taker := "0.0008", "0.001"
				makerU, takerU := "0.0002", "0.0005"
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "0",
					"data": []map[string]string{{
						"instType": instType, "maker": maker, "taker": taker,
						"makerU": makerU, "takerU": takerU,
					}},
				})
			},
		},
		{
			name: "bybit", adapter: newBybit, spot: "0.001", contract: "0.0002",
			handler: func(w http.ResponseWriter, r *http.Request) {
				rate := "0.001"
				if strings.Contains(r.URL.RawQuery, "linear") {
					rate = "0.0002"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"retCode": 0, "retMsg": "OK",
					"result": map[string]any{"list": []map[string]string{{
						"symbol": "BTCUSDT", "makerFeeRate": rate, "takerFeeRate": rate,
					}}},
				})
			},
		},
		{
			name: "bitget", adapter: newBitget, spot: "0.0002", contract: "0.0006",
			handler: func(w http.ResponseWriter, r *http.Request) {
				maker, taker := "0.0006", "0.0008"
				if r.URL.Query().Get("category") == "SPOT" {
					maker, taker = "0.0002", "0.0004"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "00000", "msg": "success",
					"data": map[string]string{"makerFeeRate": maker, "takerFeeRate": taker},
				})
			},
		},
		{
			name: "gate", adapter: newGate, spot: "0.0015", contract: "0.00015",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"maker_fee": "0.002", "taker_fee": "0.002",
					"gt_discount": true, "gt_maker_fee": "0.0015", "gt_taker_fee": "0.0015",
					"futures_maker_fee": "0.00015", "futures_taker_fee": "0.0005",
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(test.handler))
			defer server.Close()
			reader, ok := test.adapter(server.Client(), server.URL).(FeeRateReader)
			if !ok {
				t.Fatal("adapter missing FeeRateReader")
			}
			rates, err := reader.AccountFeeRates(context.Background(), creds)
			if err != nil {
				t.Fatal(err)
			}
			if rates.Spot.Status != FeeStatusOK || rates.Spot.Maker != test.spot {
				t.Fatalf("spot=%+v want maker %s", rates.Spot, test.spot)
			}
			if rates.Contract.Status != FeeStatusOK || rates.Contract.Maker != test.contract {
				t.Fatalf("contract=%+v want maker %s", rates.Contract, test.contract)
			}
		})
	}
}

func TestBinanceAccountFeeRatesUseSpotAndPapiHosts(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "tradeFee") {
			_ = json.NewEncoder(w).Encode([]map[string]string{{
				"symbol": "BTCUSDT", "makerCommission": "0.001", "takerCommission": "0.001",
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"symbol": "BTCUSDT", "makerCommissionRate": "0.0002", "takerCommissionRate": "0.0004",
		})
	}))
	defer server.Close()
	adapter := &binanceAdapter{client: server.Client(), base: server.URL, spot: server.URL}
	rates, err := adapter.AccountFeeRates(context.Background(), Credentials{APIKey: "k", APISecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if rates.Spot.Maker != "0.001" || rates.Contract.Maker != "0.0002" {
		t.Fatalf("rates=%+v", rates)
	}
	if len(paths) != 2 {
		t.Fatalf("paths=%v", paths)
	}
}

func TestHyperliquidAccountFeeRatesUseEffectiveRates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"userAddRate": "0.000105", "userCrossRate": "0.000315",
			"userSpotAddRate": "0.00028", "userSpotCrossRate": "0.00049",
			"feeSchedule": "ignored",
		})
	}))
	defer server.Close()
	adapter := newHyperliquid(server.Client(), server.URL).(FeeRateReader)
	rates, err := adapter.AccountFeeRates(context.Background(), Credentials{APIKey: "0xabc"})
	if err != nil {
		t.Fatal(err)
	}
	if rates.Spot.Maker != "0.00028" || rates.Contract.Taker != "0.000315" {
		t.Fatalf("rates=%+v", rates)
	}
}

func TestAsterAccountFeeRatesSpotUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"symbol": "BTCUSDT", "makerCommissionRate": "0", "takerCommissionRate": "0.0004",
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(FeeRateReader)
	rates, err := adapter.AccountFeeRates(context.Background(), Credentials{
		APIKey: "hmac-key", APISecret: "hmac-secret", CredentialKind: "aster_hmac",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rates.Spot.Status != FeeStatusUnsupported {
		t.Fatalf("spot=%+v", rates.Spot)
	}
	if rates.Contract.Maker != "0" || rates.Contract.Taker != "0.0004" {
		t.Fatalf("contract=%+v", rates.Contract)
	}
}

func TestAsterAccountFeeRatesAPIWalletUsesV3CommissionRate(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{
			"symbol": "BTCUSDT", "makerCommissionRate": "0", "takerCommissionRate": "0.0004",
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(FeeRateReader)
	rates, err := adapter.AccountFeeRates(context.Background(), Credentials{
		APIKey:         testWalletAddress,
		APISecret:      testWalletKey,
		CredentialKind: "aster_api_wallet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/fapi/v3/commissionRate" {
		t.Fatalf("path=%s want /fapi/v3/commissionRate", path)
	}
	if rates.Spot.Status != FeeStatusUnsupported {
		t.Fatalf("spot=%+v", rates.Spot)
	}
	if rates.Contract.Maker != "0" || rates.Contract.Taker != "0.0004" {
		t.Fatalf("contract=%+v", rates.Contract)
	}
}

func TestGateAccountFeeRatesRejectZeroDefaultForMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"maker_fee": "0.001"})
	}))
	defer server.Close()
	adapter := newGate(server.Client(), server.URL).(FeeRateReader)
	if _, err := adapter.AccountFeeRates(context.Background(), Credentials{APIKey: "k", APISecret: "s"}); err == nil {
		t.Fatal("expected missing taker fee error")
	}
}

func TestOKXAccountFeeRatesBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "50013", "msg": "Invalid sign", "data": []any{},
		})
	}))
	defer server.Close()
	adapter := newOKX(server.Client(), server.URL).(FeeRateReader)
	if _, err := adapter.AccountFeeRates(context.Background(), Credentials{
		APIKey: "k", APISecret: "s", Passphrase: "p",
	}); err == nil {
		t.Fatal("expected business error")
	}
}

func TestBitgetAccountFeeRates429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"429","msg":"too many requests"}`))
	}))
	defer server.Close()
	adapter := newBitget(server.Client(), server.URL).(FeeRateReader)
	if _, err := adapter.AccountFeeRates(context.Background(), Credentials{APIKey: "k", APISecret: "s", Passphrase: "p"}); err == nil {
		t.Fatal("expected 429")
	}
}

func TestLighterRateFromTick(t *testing.T) {
	got, err := lighterRateFromTick("0")
	if err != nil || got != "0" {
		t.Fatalf("standard=%s err=%v", got, err)
	}
	got, err = lighterRateFromTick("40")
	if err != nil || got != "0.00004" {
		t.Fatalf("premium maker=%s err=%v", got, err)
	}
	got, err = lighterRateFromTick("280")
	if err != nil || got != "0.00028" {
		t.Fatalf("premium taker=%s err=%v", got, err)
	}
}

func TestRegistryAccountFeeRatesDoesNotUseSnapshotLimiter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0",
			"data": []map[string]string{{"maker": "0.001", "taker": "0.001", "makerU": "0.0002", "takerU": "0.0005"}},
		})
	}))
	defer server.Close()
	registry := NewRegistry(server.Client(), map[string]string{"okx": server.URL})
	rates, err := registry.AccountFeeRates(context.Background(), "okx", Credentials{APIKey: "k", APISecret: "s", Passphrase: "p"})
	if err != nil || rates.Spot.Maker != "0.001" {
		t.Fatalf("rates=%+v err=%v", rates, err)
	}
}
