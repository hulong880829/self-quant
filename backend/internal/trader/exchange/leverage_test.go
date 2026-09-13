package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	corex "selfquant/backend/internal/exchange"
)

func TestBitgetSetLeverageUsesUTAV3(t *testing.T) {
	var method, path, body, query string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path, query = request.Method, request.URL.Path, request.URL.RawQuery
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{"code": "00000", "msg": "success"})
	}))
	defer server.Close()
	adapter := newBitget(server.Client(), server.URL)
	setter := adapter.(LeverageSetter)
	result, err := setter.SetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{
			Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			QuoteAsset: "USDT", SettleAsset: "USDT",
		},
		decimal.NewFromInt(7),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.CapacityKnown {
		t.Fatalf("result=%+v", result)
	}
	if method != http.MethodPost || path != "/api/v3/account/set-leverage" {
		t.Fatalf("method=%s path=%s", method, path)
	}
	if strings.Contains(path, "/api/v2/mix/account") {
		t.Fatalf("classic path used: %s", path)
	}
	if !strings.Contains(body, `"category":"USDT-FUTURES"`) ||
		!strings.Contains(body, `"marginMode":"crossed"`) ||
		!strings.Contains(body, `"leverage":"7"`) ||
		!strings.Contains(body, `"symbol":"BTCUSDT"`) {
		t.Fatalf("body=%s", body)
	}
	_ = query
}

func TestBitgetSetLeverageUSDCCategory(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{"code": "00000"})
	}))
	defer server.Close()
	result, err := newBitget(server.Client(), server.URL).(LeverageSetter).SetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{
			Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCUSDC",
			QuoteAsset: "USDC", SettleAsset: "USDC",
		},
		decimal.NewFromInt(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.CapacityKnown {
		t.Fatalf("result=%+v", result)
	}
	if !strings.Contains(body, `"category":"USDC-FUTURES"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestBitgetSetLeverageClassifiesErrors(t *testing.T) {
	t.Run("http400", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":"40001","msg":"bad"}`))
		}))
		defer server.Close()
		err := bitgetSet(t, server, decimal.NewFromInt(4))
		if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("businessCode", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"code": "40034", "msg": "leverage invalid"})
		}))
		defer server.Close()
		err := bitgetSet(t, server, decimal.NewFromInt(4))
		if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("http500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"code":"50000","msg":"down"}`))
		}))
		defer server.Close()
		err := bitgetSet(t, server, decimal.NewFromInt(4))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("badJSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"truncated"`))
		}))
		defer server.Close()
		err := bitgetSet(t, server, decimal.NewFromInt(4))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			time.Sleep(80 * time.Millisecond)
			_ = json.NewEncoder(writer).Encode(map[string]any{"code": "00000"})
		}))
		defer server.Close()
		client := server.Client()
		client.Timeout = 20 * time.Millisecond
		_, err := newBitget(client, server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
			Instrument{Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			decimal.NewFromInt(4),
		)
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
}

const (
	bitgetPreviewEndpoint         = "/api/v3/account/pre-set-leverage"
	bitgetPreviewASCIIQuery       = "category=USDT-FUTURES&leverage=4&marginMode=cross&symbol=BTCUSDT"
	bitgetPreviewChineseHTTPQuery = "category=USDT-FUTURES&leverage=4&marginMode=cross&symbol=%E9%BE%99%E8%99%BEUSDT"
	bitgetPreviewChineseSignQuery = "category=USDT-FUTURES&leverage=4&marginMode=cross&symbol=龙虾USDT"
)

func writeBitgetPreviewIfSigned(writer http.ResponseWriter, request *http.Request, signQuery string) {
	want := hmacBase64(
		"secret",
		request.Header.Get("ACCESS-TIMESTAMP")+http.MethodGet+bitgetPreviewEndpoint+"?"+signQuery,
	)
	if request.Header.Get("ACCESS-SIGN") != want {
		_ = json.NewEncoder(writer).Encode(map[string]any{"code": "40009", "msg": "sign signature error"})
		return
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"code": "00000",
		"data": map[string]string{
			"estMaxOpen": "2", "requiredMargin": "9999", "marginChange": "1.5",
		},
	})
}

func TestBitgetPreviewSetLeverageRequest(t *testing.T) {
	var method, path, query, timestamp, sign string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path, query = request.Method, request.URL.Path, request.URL.RawQuery
		timestamp = request.Header.Get("ACCESS-TIMESTAMP")
		sign = request.Header.Get("ACCESS-SIGN")
		writeBitgetPreviewIfSigned(writer, request, bitgetPreviewASCIIQuery)
	}))
	defer server.Close()
	preview, err := newBitget(server.Client(), server.URL).(LeverageSetPreviewer).PreviewSetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{
			Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			QuoteAsset: "USDT",
		},
		decimal.NewFromInt(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != bitgetPreviewEndpoint {
		t.Fatalf("method=%s path=%s", method, path)
	}
	if query != bitgetPreviewASCIIQuery {
		t.Fatalf("query=%s", query)
	}
	wantSign := hmacBase64("secret", timestamp+http.MethodGet+bitgetPreviewEndpoint+"?"+bitgetPreviewASCIIQuery)
	if sign != wantSign {
		t.Fatalf("ascii requestPath and signPath diverged")
	}
	if preview.EstMaxOpen != "2" || preview.EstMaxOpenUnit != EstMaxOpenUnitQuoteNotional ||
		!preview.MarginChange.Equal(decimal.RequireFromString("1.5")) ||
		!preview.RequiredMargin.Equal(decimal.NewFromInt(9999)) {
		t.Fatalf("preview=%+v", preview)
	}
}

func TestBitgetPreviewSetLeverageChineseSymbolSign(t *testing.T) {
	var method, path, query, timestamp, sign string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path, query = request.Method, request.URL.Path, request.URL.RawQuery
		timestamp = request.Header.Get("ACCESS-TIMESTAMP")
		sign = request.Header.Get("ACCESS-SIGN")
		writeBitgetPreviewIfSigned(writer, request, bitgetPreviewChineseSignQuery)
	}))
	defer server.Close()
	preview, err := newBitget(server.Client(), server.URL).(LeverageSetPreviewer).PreviewSetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{
			Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "龙虾USDT",
			QuoteAsset: "USDT", SettleAsset: "USDT",
		},
		decimal.NewFromInt(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != bitgetPreviewEndpoint {
		t.Fatalf("method=%s path=%s", method, path)
	}
	if query != bitgetPreviewChineseHTTPQuery {
		t.Fatalf("query=%s", query)
	}
	wantSign := hmacBase64("secret", timestamp+http.MethodGet+bitgetPreviewEndpoint+"?"+bitgetPreviewChineseSignQuery)
	if sign != wantSign {
		t.Fatalf("signed encoded query instead of unicode symbol")
	}
	if preview.EstMaxOpen != "2" || preview.EstMaxOpenUnit != EstMaxOpenUnitQuoteNotional ||
		!preview.MarginChange.Equal(decimal.RequireFromString("1.5")) ||
		!preview.RequiredMargin.Equal(decimal.NewFromInt(9999)) {
		t.Fatalf("preview=%+v", preview)
	}
}

func TestBitgetPreviewSetLeverageUSDCUnit(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"code": "00000",
			"data": map[string]string{
				"estMaxOpen": "250000000", "requiredMargin": "0", "marginChange": "0",
			},
		})
	}))
	defer server.Close()
	preview, err := newBitget(server.Client(), server.URL).(LeverageSetPreviewer).PreviewSetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{
			Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCPERP",
			QuoteAsset: "USDC", SettleAsset: "USDC",
		},
		decimal.NewFromInt(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "category=USDC-FUTURES") || !strings.Contains(query, "symbol=BTCPERP") {
		t.Fatalf("query=%s", query)
	}
	if preview.EstMaxOpen != "250000000" || preview.EstMaxOpenUnit != EstMaxOpenUnitQuoteNotional {
		t.Fatalf("preview=%+v", preview)
	}
}

func TestBitgetEstMaxOpenUnit(t *testing.T) {
	tests := []struct {
		name string
		inst Instrument
		want string
	}{
		{
			name: "usdtLinear",
			inst: Instrument{ContractType: "perpetual", QuoteAsset: "USDT", SettleAsset: "USDT"},
			want: EstMaxOpenUnitQuoteNotional,
		},
		{
			name: "usdtSettleEmpty",
			inst: Instrument{ContractType: "perpetual", QuoteAsset: "USDT"},
			want: EstMaxOpenUnitQuoteNotional,
		},
		{
			name: "usdcLinear",
			inst: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTCPERP",
				QuoteAsset: "USDC", SettleAsset: "USDC",
			},
			want: EstMaxOpenUnitQuoteNotional,
		},
		{
			name: "inverse",
			inst: Instrument{
				ContractType: "perpetual", QuoteAsset: "USDT", SettleAsset: "USDT",
				Metadata: map[string]any{"contractModel": "inverse"},
			},
		},
		{
			name: "coinMargined",
			inst: Instrument{ContractType: "perpetual", QuoteAsset: "USD", SettleAsset: "BTC"},
		},
		{
			name: "usdtQuoteCoinSettle",
			inst: Instrument{ContractType: "perpetual", QuoteAsset: "USDT", SettleAsset: "BTC"},
		},
		{
			name: "quoteSettleMismatch",
			inst: Instrument{ContractType: "perpetual", QuoteAsset: "USDT", SettleAsset: "USDC"},
		},
		{
			name: "spot",
			inst: Instrument{ContractType: "spot", QuoteAsset: "USDT", SettleAsset: "USDT"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := bitgetEstMaxOpenUnit(test.inst); got != test.want {
				t.Fatalf("unit=%q want=%q category=%s", got, test.want, bitgetCategory(test.inst))
			}
		})
	}
}

func TestBitgetUSDCPresetLeverageProbeIsQuoteNotional(t *testing.T) {
	raw, err := os.ReadFile("testdata/bitget_usdc_preset_leverage_redacted.json")
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Category   string `json:"category"`
		Symbol     string `json:"symbol"`
		EstMaxOpen string `json:"estMaxOpen"`
		MarkPrice  string `json:"markPrice"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Category != "USDC-FUTURES" || probe.Symbol != "BTCPERP" {
		t.Fatalf("probe=%+v", probe)
	}
	est, err := decimal.NewFromString(probe.EstMaxOpen)
	if err != nil || !est.IsPositive() {
		t.Fatalf("estMaxOpen=%q", probe.EstMaxOpen)
	}
	mark, err := decimal.NewFromString(probe.MarkPrice)
	if err != nil || !mark.IsPositive() {
		t.Fatalf("markPrice=%q", probe.MarkPrice)
	}
	if !est.GreaterThan(mark) {
		t.Fatalf("estMaxOpen %s is not larger than mark %s; likely base quantity", est, mark)
	}
	if probe.Conclusion != EstMaxOpenUnitQuoteNotional {
		t.Fatalf("conclusion=%q", probe.Conclusion)
	}
}

func TestBitgetPreviewSetLeverageRejectsBusinessCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"code": "40001", "msg": "bad"})
	}))
	defer server.Close()
	_, err := newBitget(server.Client(), server.URL).(LeverageSetPreviewer).PreviewSetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		decimal.NewFromInt(4),
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
}

func TestBinanceSetLeverageParsesMaxNotionalValue(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		result, err, log := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "BTCUSDT", "leverage": 4, "maxNotionalValue": "500000",
			})
		}))
		if err != nil || !result.CapacityKnown || !result.MaxNotional.Equal(decimal.NewFromInt(500000)) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertOnlyLeveragePOST(t, log, "/papi/v1/um/leverage")
	})
	t.Run("wrongSymbol", func(t *testing.T) {
		result, err, log := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "ETHUSDT", "leverage": 4, "maxNotionalValue": "500000",
			})
		}))
		if !errors.Is(err, ErrUncertain) || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertOnlyLeveragePOST(t, log, "/papi/v1/um/leverage")
	})
	t.Run("wrongLeverage", func(t *testing.T) {
		_, err, _ := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "BTCUSDT", "leverage": 20, "maxNotionalValue": "500000",
			})
		}))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missingMax", func(t *testing.T) {
		_, err, _ := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "BTCUSDT", "leverage": 4,
			})
		}))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("maxNotNumeric", func(t *testing.T) {
		_, err, _ := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "BTCUSDT", "leverage": 4, "maxNotionalValue": "abc",
			})
		}))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("maxNotPositive", func(t *testing.T) {
		_, err, _ := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "BTCUSDT", "leverage": 4, "maxNotionalValue": "0",
			})
		}))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("truncatedJSON", func(t *testing.T) {
		_, err, _ := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"symbol":"BTCUSDT"`))
		}))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		client := &http.Client{Timeout: 20 * time.Millisecond}
		_, err, log := binanceSetLeverage(t, client, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(80 * time.Millisecond)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"symbol": "BTCUSDT", "leverage": 4, "maxNotionalValue": "500000",
			})
		}))
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
		if log.gets != 0 {
			t.Fatalf("gets=%d paths=%v", log.gets, log.paths)
		}
	})
	t.Run("http400", func(t *testing.T) {
		_, err, _ := binanceSetLeverage(t, nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":-4028,"msg":"leverage invalid"}`))
		}))
		if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAsterSetLeverageParsesMaxNotional(t *testing.T) {
	t.Run("maxNotionalOnly", func(t *testing.T) {
		result, err, log := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 4, "maxNotional": "1000000",
		})
		if err != nil || !result.CapacityKnown || !result.MaxNotional.Equal(decimal.NewFromInt(1000000)) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertOnlyLeveragePOST(t, log, "/fapi/v1/leverage")
	})
	t.Run("maxNotionalValueOnly", func(t *testing.T) {
		result, err, log := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 4, "maxNotionalValue": "2000000",
		})
		if err != nil || !result.CapacityKnown || !result.MaxNotional.Equal(decimal.NewFromInt(2000000)) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertOnlyLeveragePOST(t, log, "/fapi/v1/leverage")
	})
	t.Run("bothEqual", func(t *testing.T) {
		result, err, _ := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 4,
			"maxNotional": "1500000", "maxNotionalValue": "1500000",
		})
		if err != nil || !result.CapacityKnown || !result.MaxNotional.Equal(decimal.NewFromInt(1500000)) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("bothDisagree", func(t *testing.T) {
		_, err, log := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 4,
			"maxNotional": "1500000", "maxNotionalValue": "2000000",
		})
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
		assertOnlyLeveragePOST(t, log, "/fapi/v1/leverage")
	})
	t.Run("missingMax", func(t *testing.T) {
		_, err, _ := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 4,
		})
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("invalidMax", func(t *testing.T) {
		_, err, _ := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 4, "maxNotional": "0",
		})
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrongSymbol", func(t *testing.T) {
		_, err, _ := asterSetLeverage(t, map[string]any{
			"symbol": "ETHUSDT", "leverage": 4, "maxNotional": "1000000",
		})
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrongLeverage", func(t *testing.T) {
		_, err, _ := asterSetLeverage(t, map[string]any{
			"symbol": "BTCUSDT", "leverage": 20, "maxNotional": "1000000",
		})
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestAsterAPIWalletSetLeverageUsesV3WithChineseSymbol(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
		chineseSymbol = "龙虾USDT"
		encodedSymbol = "symbol=%E9%BE%99%E8%99%BEUSDT"
	)
	key, err := parseHyperliquidKey(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	var method, path, contentType, posted string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path, contentType = request.Method, request.URL.Path, request.Header.Get("Content-Type")
		raw, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		posted = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"symbol": chineseSymbol, "leverage": 4, "maxNotionalValue": "1000000",
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(*asterAdapter)
	adapter.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	result, err := adapter.SetLeverage(
		context.Background(),
		Credentials{
			APIKey: walletAddress, APISecret: walletKey, CredentialKind: "aster_api_wallet",
		},
		Instrument{Exchange: "aster", ContractType: "perpetual", ExchangeSymbol: chineseSymbol},
		decimal.NewFromInt(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/fapi/v3/leverage" {
		t.Fatalf("method=%s path=%s", method, path)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type=%s", contentType)
	}
	if strings.Contains(posted, "龙虾") {
		t.Fatalf("raw unicode leaked into form body: %s", posted)
	}
	if !strings.Contains(posted, encodedSymbol) {
		t.Fatalf("body=%s", posted)
	}
	for _, name := range []string{"user=", "signer=", "nonce=", "timestamp=", "recvWindow="} {
		if !strings.Contains(posted, name) {
			t.Fatalf("missing %s body=%s", name, posted)
		}
	}
	message, signature, ok := strings.Cut(posted, "&signature=")
	if !ok || message == "" || signature == "" {
		t.Fatalf("body=%s", posted)
	}
	want, err := corex.SignAsterV3(key, message)
	if err != nil {
		t.Fatal(err)
	}
	if signature != want {
		t.Fatalf("signature mismatch")
	}
	if !result.CapacityKnown || !result.MaxNotional.Equal(decimal.NewFromInt(1000000)) {
		t.Fatalf("result=%+v", result)
	}
}

func TestAsterHMACSetLeverageKeepsV1(t *testing.T) {
	var method, path, apiKey, body string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path = request.Method, request.URL.Path
		apiKey = request.Header.Get("X-MBX-APIKEY")
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"symbol": "BTCUSDT", "leverage": 4, "maxNotionalValue": "1000000",
		})
	}))
	defer server.Close()
	result, err := newAster(server.Client(), server.URL).(LeverageSetter).SetLeverage(
		context.Background(),
		Credentials{APIKey: "hmac-key", APISecret: "hmac-secret", CredentialKind: "aster_hmac"},
		Instrument{Exchange: "aster", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		decimal.NewFromInt(4),
	)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/fapi/v1/leverage" {
		t.Fatalf("method=%s path=%s", method, path)
	}
	if apiKey != "hmac-key" {
		t.Fatalf("api-key=%s", apiKey)
	}
	for _, name := range []string{"user=", "signer=", "nonce="} {
		if strings.Contains(body, name) {
			t.Fatalf("api wallet param %s in hmac body=%s", name, body)
		}
	}
	if !result.CapacityKnown || !result.MaxNotional.Equal(decimal.NewFromInt(1000000)) {
		t.Fatalf("result=%+v", result)
	}
}

func TestGateSetLeverageUsesCrossMarginMode(t *testing.T) {
	t.Run("pathAndQuery", func(t *testing.T) {
		var method, path, query string
		var log requestLog
		server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, request *http.Request) {
			method, path, query = request.Method, request.URL.Path, request.URL.RawQuery
			_ = json.NewEncoder(writer).Encode(map[string]any{"leverage": "4"})
		}))
		defer server.Close()
		result, err := newGate(server.Client(), server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret"},
			Instrument{
				Exchange: "gate", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
				BaseAsset: "BTC", QuoteAsset: "USDT",
			},
			decimal.NewFromInt(4),
		)
		if err != nil || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if method != http.MethodPost || path != "/api/v4/futures/usdt/positions/BTC_USDT/set_leverage" {
			t.Fatalf("method=%s path=%s", method, path)
		}
		if !strings.Contains(query, "margin_mode=cross") || !strings.Contains(query, "leverage=4") {
			t.Fatalf("query=%s", query)
		}
		if strings.Contains(path, "/positions/") && strings.HasSuffix(path, "/leverage") {
			t.Fatalf("old leverage path used: %s", path)
		}
		if log.gets != 0 {
			t.Fatalf("gets=%d paths=%v", log.gets, log.paths)
		}
	})
	t.Run("metadataContract", func(t *testing.T) {
		var path string
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			path = request.URL.Path
			_ = json.NewEncoder(writer).Encode(map[string]any{"leverage": "4"})
		}))
		defer server.Close()
		_, err := newGate(server.Client(), server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret"},
			Instrument{
				Exchange: "gate", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
				Metadata: map[string]any{"contract": "BTC_USDT"},
			},
			decimal.NewFromInt(4),
		)
		if err != nil {
			t.Fatal(err)
		}
		if path != "/api/v4/futures/usdt/positions/BTC_USDT/set_leverage" {
			t.Fatalf("path=%s", path)
		}
	})
	t.Run("http400Label", func(t *testing.T) {
		err := gateSetLeverageStatus(t, http.StatusBadRequest, map[string]string{
			"label": "INVALID_LEVERAGE", "message": "leverage too high",
		})
		if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUncertain) || errors.Is(err, ErrOrderNotFound) {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(err.Error(), "INVALID_LEVERAGE") || !strings.Contains(err.Error(), "leverage too high") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("http200Label", func(t *testing.T) {
		err := gateSetLeverageStatus(t, http.StatusOK, map[string]string{
			"label": "POSITION_NOT_FOUND", "message": "no position",
		})
		if !errors.Is(err, ErrRejected) || errors.Is(err, ErrOrderNotFound) || errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(err.Error(), "POSITION_NOT_FOUND") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("http500Label", func(t *testing.T) {
		err := gateSetLeverageStatus(t, http.StatusInternalServerError, map[string]string{
			"label": "INTERNAL", "message": "down",
		})
		if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) || errors.Is(err, ErrOrderNotFound) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("timeoutLabel", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(80 * time.Millisecond)
			writer.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"label": "INTERNAL", "message": "late",
			})
		}))
		defer server.Close()
		client := server.Client()
		client.Timeout = 20 * time.Millisecond
		_, err := newGate(client, server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret"},
			Instrument{
				Exchange: "gate", ContractType: "perpetual",
				BaseAsset: "BTC", QuoteAsset: "USDT",
			},
			decimal.NewFromInt(4),
		)
		if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) || errors.Is(err, ErrOrderNotFound) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestSetLeverageWithoutCapacityDoesNotGet(t *testing.T) {
	t.Run("okx", func(t *testing.T) {
		var log requestLog
		server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"code": "0", "msg": "ok"})
		}))
		defer server.Close()
		result, err := newOKX(server.Client(), server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
			Instrument{Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP"},
			decimal.NewFromInt(4),
		)
		if err != nil || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if log.gets != 0 || log.posts != 1 {
			t.Fatalf("gets=%d posts=%d paths=%v", log.gets, log.posts, log.paths)
		}
		if !strings.Contains(strings.Join(log.paths, ","), "/api/v5/account/set-leverage") {
			t.Fatalf("paths=%v", log.paths)
		}
	})
	t.Run("bybit", func(t *testing.T) {
		var log requestLog
		server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"retCode": 0, "retMsg": "OK"})
		}))
		defer server.Close()
		result, err := newBybit(server.Client(), server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret"},
			Instrument{Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			decimal.NewFromInt(4),
		)
		if err != nil || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if log.gets != 0 || log.posts != 1 {
			t.Fatalf("gets=%d posts=%d paths=%v", log.gets, log.posts, log.paths)
		}
		if !strings.Contains(strings.Join(log.paths, ","), "/v5/position/set-leverage") {
			t.Fatalf("paths=%v", log.paths)
		}
	})
	t.Run("gate", func(t *testing.T) {
		var log requestLog
		server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"leverage": "4"})
		}))
		defer server.Close()
		result, err := newGate(server.Client(), server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{APIKey: "key", APISecret: "secret"},
			Instrument{
				Exchange: "gate", ContractType: "perpetual",
				BaseAsset: "BTC", QuoteAsset: "USDT",
			},
			decimal.NewFromInt(4),
		)
		if err != nil || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if log.gets != 0 || log.posts != 1 {
			t.Fatalf("gets=%d posts=%d paths=%v", log.gets, log.posts, log.paths)
		}
	})
	t.Run("hyperliquid", func(t *testing.T) {
		var log requestLog
		server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/info" {
				var payload struct {
					Type string `json:"type"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					http.Error(writer, "invalid payload", http.StatusBadRequest)
					return
				}
				switch payload.Type {
				case "meta":
					_, _ = writer.Write([]byte(
						`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`,
					))
				case "spotMeta":
					_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
				default:
					_, _ = writer.Write([]byte(`{}`))
				}
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"status": "ok"})
		}))
		defer server.Close()
		result, err := newHyperliquid(server.Client(), server.URL).(LeverageSetter).SetLeverage(
			context.Background(),
			Credentials{
				APIKey:         "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
				APISecret:      dexRecoveryTestKey,
				SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
			},
			Instrument{Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
			decimal.NewFromInt(4),
		)
		if result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if log.gets != 0 {
			t.Fatalf("gets=%d paths=%v", log.gets, log.paths)
		}
	})
}

func TestBybitSetLeverageClassifiesErrors(t *testing.T) {
	instrument := Instrument{Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"}
	credentials := Credentials{APIKey: "key", APISecret: "secret"}
	set := func(client *http.Client, base string) (LeverageApplyResult, error) {
		return newBybit(client, base).(LeverageSetter).SetLeverage(
			context.Background(), credentials, instrument, decimal.NewFromInt(4),
		)
	}

	t.Run("success", func(t *testing.T) {
		var log requestLog
		server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"retCode": 0, "retMsg": "OK"})
		}))
		defer server.Close()
		result, err := set(server.Client(), server.URL)
		if err != nil || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertOnlyLeveragePOST(t, log.snapshot(), "/v5/position/set-leverage")
	})
	t.Run("alreadySet", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 110043, "retMsg": "leverage not modified",
			})
		}))
		defer server.Close()
		result, err := set(server.Client(), server.URL)
		if err != nil || result.CapacityKnown {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if errors.Is(err, ErrRejected) {
			t.Fatalf("110043 classified as rejected: %v", err)
		}
	})
	t.Run("businessCode", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 110013, "retMsg": "leverage invalid",
			})
		}))
		defer server.Close()
		_, err := set(server.Client(), server.URL)
		if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUncertain) {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(err.Error(), "retCode=110013") ||
			!strings.Contains(err.Error(), "retMsg=leverage invalid") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("http500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"retCode":10016,"retMsg":"Server error"}`))
		}))
		defer server.Close()
		_, err := set(server.Client(), server.URL)
		if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(80 * time.Millisecond)
			_ = json.NewEncoder(writer).Encode(map[string]any{"retCode": 0, "retMsg": "OK"})
		}))
		defer server.Close()
		client := server.Client()
		client.Timeout = 20 * time.Millisecond
		_, err := set(client, server.URL)
		if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestVenuesDoNotImplementLeveragePreview(t *testing.T) {
	client := http.DefaultClient
	for _, adapter := range []Adapter{
		newOKX(client, ""), newBybit(client, ""), newGate(client, ""),
		newBinance(client, ""), newAster(client, ""), newHyperliquid(client, ""),
		newLighter(client, ""),
	} {
		if _, ok := adapter.(LeverageSetPreviewer); ok {
			t.Fatalf("%T should not implement LeverageSetPreviewer", adapter)
		}
	}
}

func bitgetSet(t *testing.T, server *httptest.Server, leverage decimal.Decimal) error {
	t.Helper()
	_, err := newBitget(server.Client(), server.URL).(LeverageSetter).SetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		Instrument{Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		leverage,
	)
	return err
}

type requestLog struct {
	mu          sync.Mutex
	gets, posts int
	paths       []string
}

func (l *requestLog) wrap(handler http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		l.mu.Lock()
		l.paths = append(l.paths, request.Method+" "+request.URL.Path)
		switch request.Method {
		case http.MethodGet:
			l.gets++
		case http.MethodPost:
			l.posts++
		}
		l.mu.Unlock()
		handler(writer, request)
	}
}

func (l *requestLog) snapshot() requestLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	return requestLog{
		gets: l.gets, posts: l.posts, paths: append([]string(nil), l.paths...),
	}
}

func assertOnlyLeveragePOST(t *testing.T, log requestLog, path string) {
	t.Helper()
	if log.gets != 0 || log.posts != 1 {
		t.Fatalf("gets=%d posts=%d paths=%v", log.gets, log.posts, log.paths)
	}
	if len(log.paths) != 1 || !strings.Contains(log.paths[0], path) {
		t.Fatalf("paths=%v want %s", log.paths, path)
	}
	if strings.Contains(strings.Join(log.paths, ","), "positionRisk") {
		t.Fatalf("positionRisk requested: %v", log.paths)
	}
}

func binanceSetLeverage(
	t *testing.T,
	client *http.Client,
	handler http.HandlerFunc,
) (LeverageApplyResult, error, requestLog) {
	t.Helper()
	var log requestLog
	server := httptest.NewServer(log.wrap(handler))
	t.Cleanup(server.Close)
	if client == nil {
		client = server.Client()
	}
	result, err := newBinance(client, server.URL).(LeverageSetter).SetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		Instrument{Exchange: "binance", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		decimal.NewFromInt(4),
	)
	return result, err, log.snapshot()
}

func asterSetLeverage(t *testing.T, body map[string]any) (LeverageApplyResult, error, requestLog) {
	t.Helper()
	var log requestLog
	server := httptest.NewServer(log.wrap(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(body)
	}))
	t.Cleanup(server.Close)
	result, err := newAster(server.Client(), server.URL).(LeverageSetter).SetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		Instrument{Exchange: "aster", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		decimal.NewFromInt(4),
	)
	return result, err, log.snapshot()
}

func gateSetLeverageStatus(t *testing.T, status int, body map[string]string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(body)
	}))
	t.Cleanup(server.Close)
	_, err := newGate(server.Client(), server.URL).(LeverageSetter).SetLeverage(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		Instrument{
			Exchange: "gate", ContractType: "perpetual",
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		decimal.NewFromInt(4),
	)
	return err
}
