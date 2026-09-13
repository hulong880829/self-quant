package exchange

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	corex "selfquant/backend/internal/exchange"
)

func TestAsterAPIWalletUsesV3OrderPath(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenPath = request.URL.Path
		if request.URL.Path == "/fapi/v3/order" {
			if request.URL.Query().Get("user") == "" || request.URL.Query().Get("signature") == "" {
				http.Error(writer, "missing v3 auth", http.StatusBadRequest)
				return
			}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"orderId": 11, "status": "NEW", "executedQty": "0", "clientOrderId": "sq1",
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(*asterAdapter)
	adapter.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	_, err := adapter.GetOrder(t.Context(), Credentials{
		APIKey: walletAddress, APISecret: walletKey, CredentialKind: "aster_api_wallet",
	}, QueryRequest{
		Instrument:    Instrument{Exchange: "aster", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		ClientOrderID: "client-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/fapi/v3/order" {
		t.Fatalf("path=%s", seenPath)
	}
}

func TestAsterHMACKeepsV1OrderPath(t *testing.T) {
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenPath = request.URL.Path
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"orderId": 11, "status": "NEW", "executedQty": "0",
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(*asterAdapter)
	_, err := adapter.GetOrder(t.Context(), Credentials{
		APIKey: "hmac-key", APISecret: "hmac-secret", CredentialKind: "aster_hmac",
	}, QueryRequest{
		Instrument:    Instrument{Exchange: "aster", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		ClientOrderID: "client-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/fapi/v1/order" {
		t.Fatalf("path=%s", seenPath)
	}
}

func TestAsterV3ParamStringAndSignatureAreDeterministic(t *testing.T) {
	params := map[string]string{"user": "0xabc", "nonce": "1", "signer": "0xdef"}
	if corex.AsterParamString(params) != "nonce=1&signer=0xdef&user=0xabc" {
		t.Fatalf("param string=%s", corex.AsterParamString(params))
	}
	key, err := parseHyperliquidKey("0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	first, err := corex.SignAsterV3(key, "nonce=1&signer=0xdef&user=0xabc")
	if err != nil {
		t.Fatal(err)
	}
	second, err := corex.SignAsterV3(key, "nonce=1&signer=0xdef&user=0xabc")
	if err != nil || first != second || !strings.HasPrefix(first, "0x") {
		t.Fatalf("sig=%s %s err=%v", first, second, err)
	}
}

func TestAsterParamStringURLEncodesNonASCIISymbol(t *testing.T) {
	encoded := corex.AsterParamString(map[string]string{"symbol": "龙虾USDT"})
	if encoded != "symbol=%E9%BE%99%E8%99%BEUSDT" {
		t.Fatalf("encoded=%s", encoded)
	}
	if strings.Contains(encoded, "龙虾") {
		t.Fatalf("raw unicode leaked into param string: %s", encoded)
	}
	ascii := corex.AsterParamString(map[string]string{"symbol": "BTCUSDT"})
	if ascii != "symbol=BTCUSDT" {
		t.Fatalf("ascii=%s", ascii)
	}
}

func TestAsterV3PlaceOrderURLEncodesChineseSymbolInBodyAndSignature(t *testing.T) {
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
	var posted string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/fapi/v3/order" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		if request.Method == http.MethodGet {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
			return
		}
		if request.Method != http.MethodPost {
			t.Fatalf("method=%s", request.Method)
		}
		raw, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		posted = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"orderId": 11, "status": "NEW", "executedQty": "0", "clientOrderId": "client-1",
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(*asterAdapter)
	adapter.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	_, err = adapter.PlaceOrder(t.Context(), Credentials{
		APIKey: walletAddress, APISecret: walletKey, CredentialKind: "aster_api_wallet",
	}, OrderRequest{
		Instrument: Instrument{
			Exchange: "aster", ContractType: "perpetual", ExchangeSymbol: chineseSymbol,
			QuantityStep: "0.001",
		},
		ClientOrderID: "client-1", Side: "buy", OrderType: "market", Quantity: "0.001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if posted == "" {
		t.Fatal("missing POST body")
	}
	if strings.Contains(posted, chineseSymbol) {
		t.Fatalf("raw unicode leaked into form body: %s", posted)
	}
	if !strings.Contains(posted, encodedSymbol) {
		t.Fatalf("body=%s", posted)
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
		t.Fatalf("signature=%s want=%s message=%s", signature, want, message)
	}
}

func TestAsterAPIWalletListBalancesUsesV3(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	key, err := parseHyperliquidKey(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	var method, path, query string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path, query = request.Method, request.URL.Path, request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode([]map[string]string{
			{"asset": "USDT", "balance": "10", "availableBalance": "8"},
		})
	}))
	defer server.Close()
	adapter := newAster(server.Client(), server.URL).(*asterAdapter)
	adapter.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	balances, err := adapter.ListBalances(t.Context(), Credentials{
		APIKey: walletAddress, APISecret: walletKey, CredentialKind: "aster_api_wallet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/fapi/v3/balance" {
		t.Fatalf("method=%s path=%s", method, path)
	}
	for _, name := range []string{"user=", "signer=", "nonce=", "timestamp=", "recvWindow="} {
		if !strings.Contains(query, name) {
			t.Fatalf("missing %s query=%s", name, query)
		}
	}
	message, signature, ok := strings.Cut(query, "&signature=")
	if !ok || message == "" || signature == "" {
		t.Fatalf("query=%s", query)
	}
	want, err := corex.SignAsterV3(key, message)
	if err != nil {
		t.Fatal(err)
	}
	if signature != want {
		t.Fatalf("signature mismatch")
	}
	if len(balances) != 1 || balances[0].Asset != "USDT" || balances[0].Total != "10" || balances[0].Available != "8" {
		t.Fatalf("balances=%+v", balances)
	}
}

func TestAsterHMACListBalancesKeepsV2(t *testing.T) {
	var method, path, apiKey, query string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method, path, query = request.Method, request.URL.Path, request.URL.RawQuery
		apiKey = request.Header.Get("X-MBX-APIKEY")
		_ = json.NewEncoder(writer).Encode([]map[string]string{
			{"asset": "USDT", "balance": "10", "availableBalance": "8"},
		})
	}))
	defer server.Close()
	balances, err := newAster(server.Client(), server.URL).(*asterAdapter).ListBalances(
		t.Context(),
		Credentials{APIKey: "hmac-key", APISecret: "hmac-secret", CredentialKind: "aster_hmac"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/fapi/v2/balance" {
		t.Fatalf("method=%s path=%s", method, path)
	}
	if apiKey != "hmac-key" {
		t.Fatalf("api-key=%s", apiKey)
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	signature := values.Get("signature")
	values.Del("signature")
	if signature == "" || hmacHex256("hmac-secret", values.Encode()) != signature {
		t.Fatalf("hmac signature mismatch query=%s", query)
	}
	if strings.Contains(query, "user=") || strings.Contains(query, "signer=") || strings.Contains(query, "nonce=") {
		t.Fatalf("api wallet params in hmac query=%s", query)
	}
	if len(balances) != 1 || balances[0].Asset != "USDT" || balances[0].Total != "10" {
		t.Fatalf("balances=%+v", balances)
	}
}
