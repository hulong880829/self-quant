package polymarket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCollateralBalanceUsesRoutePathForHMAC(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/balance-allowance" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		if request.URL.Query().Get("asset_type") != "COLLATERAL" {
			t.Fatalf("asset_type=%q", request.URL.Query().Get("asset_type"))
		}
		if request.URL.Query().Get("signature_type") != "1" {
			t.Fatalf("signature_type=%q", request.URL.Query().Get("signature_type"))
		}
		if request.Header.Get("POLY_SIGNATURE") == "" {
			t.Fatal("missing POLY_SIGNATURE")
		}
		_, _ = io.WriteString(writer, `{"balance":"1250000000","allowance":"0"}`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, 2*time.Second)
	client.now = func() time.Time { return time.Unix(1786186200, 0) }
	balance, err := client.CollateralBalance(context.Background(), Credentials{
		SignerAddress: "0xabc",
		APIKey:        "key",
		APISecret:     "c2VjcmV0LWtleQ",
		Passphrase:    "pass",
		SignatureType: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if balance != "1250" {
		t.Fatalf("balance=%q", balance)
	}
}

func TestCLOBClientConfiguresLayeredTransportTimeouts(t *testing.T) {
	client := NewCLOBClient("https://example.com", 8*time.Second)
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.http.Transport)
	}
	if client.http.Timeout != 8*time.Second ||
		transport.ResponseHeaderTimeout <= 0 ||
		transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf(
			"total=%s header=%s tls=%s",
			client.http.Timeout, transport.ResponseHeaderTimeout,
			transport.TLSHandshakeTimeout,
		)
	}
}

func TestL2SignatureUsesPaddedURLSafeBase64(t *testing.T) {
	signature, err := l2Signature(
		"c2VjcmV0LWtleQ",
		"1786186200",
		"GET",
		"/balance-allowance",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(signature, "=") {
		t.Fatalf("expected padded signature, got %q", signature)
	}
}

func TestL2SignatureVector(t *testing.T) {
	signature, err := l2Signature(
		"c2VjcmV0LWtleQ",
		"1786186200",
		"GET",
		"/balance-allowance",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := "BLrVUmkug-C-737dFNmTZ3pDSjXKOBHjwvrC7x8Ao0g="
	if signature != expected {
		t.Fatalf("signature=%q", signature)
	}
}

func TestCollateralBalanceHandles401(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := NewCLOBClient(server.URL, 2*time.Second)
	_, err := client.CollateralBalance(context.Background(), Credentials{
		SignerAddress: "0xabc",
		APIKey:        "key",
		APISecret:     "c2VjcmV0LWtleQ",
		Passphrase:    "pass",
		SignatureType: 0,
	})
	if err == nil {
		t.Fatal("expected auth error")
	}
	if requests != 1 {
		t.Fatalf("401 must not be retried, requests=%d", requests)
	}
}

func TestL2GETRetryRegeneratesTimestampAndSignature(t *testing.T) {
	var mu sync.Mutex
	timestamps := make([]string, 0, 2)
	signatures := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		timestamps = append(timestamps, request.Header.Get("POLY_TIMESTAMP"))
		signatures = append(signatures, request.Header.Get("POLY_SIGNATURE"))
		attempt := len(timestamps)
		mu.Unlock()
		if attempt == 1 {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(writer, `{"balance":"1000000"}`)
	}))
	defer server.Close()
	client := NewCLOBClient(server.URL, 2*time.Second)
	nowCalls := 0
	client.now = func() time.Time {
		nowCalls++
		return time.Unix(1786186200+int64(nowCalls), 0)
	}
	_, err := client.CollateralBalance(context.Background(), Credentials{
		SignerAddress: "0xabc", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(timestamps) != 2 || timestamps[0] == timestamps[1] {
		t.Fatalf("timestamps=%v", timestamps)
	}
	if signatures[0] == signatures[1] {
		t.Fatalf("signatures were reused: %v", signatures)
	}
}

func TestCollateralBalanceHandlesTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(writer, `{"balance":"0"}`)
	}))
	defer server.Close()
	client := NewCLOBClient(server.URL, 50*time.Millisecond)
	_, err := client.CollateralBalance(context.Background(), Credentials{
		SignerAddress: "0xabc",
		APIKey:        "key",
		APISecret:     "c2VjcmV0LWtleQ",
		Passphrase:    "pass",
		SignatureType: 0,
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
}

func TestPOSTTimeoutReportsWhetherRequestWasWritten(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		_, _ = io.ReadAll(request.Body)
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := NewCLOBClient(server.URL, 20*time.Millisecond)
	_, err := client.request(
		context.Background(), http.MethodPost, "/order",
		map[string]string{"value": "test"},
		&Credentials{
			SignerAddress: "0xabc", APIKey: "key",
			APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
		},
		nil,
	)
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || !transportErr.RequestWritten {
		t.Fatalf("err=%v transport=%+v", err, transportErr)
	}
	if requests != 1 {
		t.Fatalf("POST transport must not retry, requests=%d", requests)
	}
}

func TestListOpenOrdersPaginatesAndFilters(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/data/orders" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		if request.Header.Get("POLY_SIGNATURE") == "" {
			t.Fatal("missing POLY_SIGNATURE")
		}
		switch request.URL.Query().Get("next_cursor") {
		case "":
			_, _ = io.WriteString(writer, `{
				"next_cursor":"MTAw",
				"data":[
					{"id":"live","status":"ORDER_STATUS_LIVE","market":"condition-1",
					 "asset_id":"token-1","side":"BUY","original_size":"5000000",
					 "size_matched":"1250000","price":"0.55","outcome":"YES",
					 "order_type":"GTC","created_at":1786186200},
					{"id":"matched","status":"ORDER_STATUS_MATCHED","market":"condition-1",
					 "asset_id":"token-1","side":"BUY","original_size":"1000000",
					 "size_matched":"1000000","price":"0.55","outcome":"YES",
					 "order_type":"GTC","created_at":1786186200}
				]}`)
		case "MTAw":
			_, _ = io.WriteString(writer, `{
				"next_cursor":"LTE=",
				"data":[
					{"id":"empty","status":"ORDER_STATUS_LIVE","market":"condition-2",
					 "asset_id":"token-2","side":"SELL","original_size":"2000000",
					 "size_matched":"2000000","price":"0.45","outcome":"NO",
					 "order_type":"GTC","created_at":1786186300}
				]}`)
		default:
			t.Fatalf("cursor=%q", request.URL.Query().Get("next_cursor"))
		}
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, 2*time.Second)
	orders, err := client.ListOpenOrders(context.Background(), Credentials{
		SignerAddress: "0xabc", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	if len(orders) != 1 {
		t.Fatalf("orders=%+v", orders)
	}
	order := orders[0]
	if order.ID != "live" || order.OriginalSize != "5" ||
		order.MatchedSize != "1.25" || order.RemainingSize != "3.75" ||
		order.Side != "buy" || order.Outcome != "yes" {
		t.Fatalf("order=%+v", order)
	}
}

func TestListOpenOrdersAcceptsV2Array(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `[
			{"id":"live","status":"LIVE","market":"condition-1","asset_id":"token-1",
			 "side":"SELL","original_size":"10500000","size_matched":"500000","price":"0.61",
			 "outcome":"NO","order_type":"GTC","created_at":1786186200}
		]`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, 2*time.Second)
	orders, err := client.ListOpenOrders(context.Background(), Credentials{
		SignerAddress: "0xabc", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("orders=%+v", orders)
	}
	if orders[0].OriginalSize != "10.5" || orders[0].MatchedSize != "0.5" ||
		orders[0].RemainingSize != "10" {
		t.Fatalf("order=%+v", orders[0])
	}
}

func TestCancelOrderSignsExactBodyAndParsesResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/order" {
			t.Fatalf("request=%s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"orderID":"order-1"}` {
			t.Fatalf("body=%s", body)
		}
		expected, err := l2Signature(
			"c2VjcmV0LWtleQ", "1786186200", http.MethodDelete, "/order", string(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		if request.Header.Get("POLY_SIGNATURE") != expected {
			t.Fatalf("signature=%q", request.Header.Get("POLY_SIGNATURE"))
		}
		_, _ = io.WriteString(writer, `{"canceled":["order-1"],"not_canceled":{}}`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, time.Second)
	client.now = func() time.Time { return time.Unix(1786186200, 0) }
	result, err := client.CancelOrder(context.Background(), Credentials{
		SignerAddress: "0xabc", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	}, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "canceled" || result.OrderID != "order-1" {
		t.Fatalf("result=%+v", result)
	}
}

func TestV2OrderSignatureVector(t *testing.T) {
	order := map[string]any{
		"salt":          "479249096354",
		"maker":         "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1",
		"signer":        "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1",
		"tokenId":       "102936000000000000000000000000000000000000000000000000000000000000000000",
		"makerAmount":   "5200000",
		"takerAmount":   "10000000",
		"side":          "0",
		"signatureType": "0",
		"timestamp":     "1786186200000",
		"metadata":      "0x" + strings.Repeat("0", 64),
		"builder":       "0x" + strings.Repeat("0", 64),
	}
	signature, err := signOrder(
		"4f3edf983ac63ad7c7a0f4a1c2e8b7f5f6f0f4f0a3a5b6c7d8e9f00112233445",
		false,
		0,
		order,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := "0xb4e4f24f0359309a623db25ea479da00057f3c16691606750ed36d837c24460b23e0e889c5740d38ae18f40afa6bd8e7f22c419b88ccd84c265956ea166f21a51b"
	if signature != expected {
		t.Fatalf("signature=%q", signature)
	}
}

func TestBestQuotesDoesNotDependOnBookOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{
			"bids":[{"price":"0.42","size":"10"},{"price":"0.48","size":"10"}],
			"asks":[{"price":"0.57","size":"10"},{"price":"0.52","size":"10"}]
		}`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, time.Second)
	bid, ask, err := client.BestQuotes(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if bid != "0.48" || ask != "0.52" {
		t.Fatalf("bid=%s ask=%s", bid, ask)
	}
}

func TestPlaceOrderBuildsFAKAtBestAsk(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/book":
			_, _ = io.WriteString(writer, `{
				"bids":[{"price":"0.48","size":"20"}],
				"asks":[{"price":"0.55","size":"20"},{"price":"0.51","size":"20"}],
				"min_order_size":"5","tick_size":"0.01","neg_risk":false
			}`)
		case "/order":
			if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(writer, `{
				"success":true,"orderID":"order-1","status":"matched",
				"makingAmount":"10000000","takingAmount":"19607900"
			}`)
		default:
			t.Fatalf("path=%s", request.URL.Path)
		}
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, time.Second)
	client.now = func() time.Time { return time.UnixMilli(1786186200000) }
	submission, err := client.PlaceOrder(
		context.Background(),
		testOrderCredentials(0),
		Market{TickSize: "0.01"},
		"102936000000000000000000000000000000000000000000000000000000000000000000",
		"buy", "10", "usd", "book", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if submission.OrderType != "FAK" || submission.Status != "filled" {
		t.Fatalf("submission=%+v", submission)
	}
	order := posted["order"].(map[string]any)
	salt, saltIsNumber := order["salt"].(float64)
	if posted["orderType"] != "FAK" || posted["postOnly"] != false ||
		order["expiration"] != "0" ||
		order["makerAmount"] != "10000000" || order["takerAmount"] != "19607900" ||
		!saltIsNumber || salt < 1 || salt >= 1<<53 {
		t.Fatalf("posted=%+v", posted)
	}
}

func TestPlaceOrderBuildsGTCLimitWithShares(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/book" {
			_, _ = io.WriteString(writer, `{
				"bids":[{"price":"0.48","size":"20"}],
				"asks":[{"price":"0.52","size":"20"}],
				"min_order_size":"5","tick_size":"0.01","neg_risk":false
			}`)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(writer, `{
			"success":true,"orderID":"order-2","status":"live",
			"makingAmount":"2450000","takingAmount":"5000000"
		}`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, time.Second)
	submission, err := client.PlaceOrder(
		context.Background(),
		testOrderCredentials(0),
		Market{TickSize: "0.01"},
		"102936000000000000000000000000000000000000000000000000000000000000000000",
		"buy", "5", "shares", "limit", "0.49",
	)
	if err != nil {
		t.Fatal(err)
	}
	order := posted["order"].(map[string]any)
	if submission.OrderType != "GTC" || posted["orderType"] != "GTC" ||
		order["makerAmount"] != "2450000" || order["takerAmount"] != "5000000" {
		t.Fatalf("submission=%+v posted=%+v", submission, posted)
	}
}

func TestCLOBErrorIncludesSafeUpstreamMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/book" {
			_, _ = io.WriteString(writer, `{
				"bids":[{"price":"0.48","size":"20"}],
				"asks":[{"price":"0.52","size":"20"}],
				"min_order_size":"5","tick_size":"0.01"
			}`)
			return
		}
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"code":"INVALID_ORDER","errorMsg":"invalid signature"}`)
	}))
	defer server.Close()

	client := NewCLOBClient(server.URL, time.Second)
	_, err := client.PlaceOrder(
		context.Background(),
		testOrderCredentials(0),
		Market{TickSize: "0.01"},
		"102936000000000000000000000000000000000000000000000000000000000000000000",
		"buy", "10", "usd", "book", "",
	)
	var clobErr *CLOBError
	if !errors.As(err, &clobErr) ||
		clobErr.Code != "INVALID_ORDER" || clobErr.Message != "invalid signature" {
		t.Fatalf("err=%v", err)
	}
}

func TestDepositWalletSignatureIsERC7739Wrapped(t *testing.T) {
	order := map[string]any{
		"salt": "479249096354", "maker": "0x1111111111111111111111111111111111111111",
		"signer":      "0x1111111111111111111111111111111111111111",
		"tokenId":     "102936000000000000000000000000000000000000000000000000000000000000000000",
		"makerAmount": "5200000", "takerAmount": "10000000",
		"side": "0", "signatureType": "3", "timestamp": "1786186200000",
		"metadata": zeroBytes32, "builder": zeroBytes32,
	}
	signature, err := signOrder(
		"4f3edf983ac63ad7c7a0f4a1c2e8b7f5f6f0f4f0a3a5b6c7d8e9f00112233445",
		false, 3, order,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := "0x993fb1d00ccb70d07fd5ffcb9cb0b54da1ed0c056c8f34c1852ceb814093592833ef4e76389f7311d489d8729ac7f0139994a222e4def035a666b737d29313431b3264e159346253e26a64e00b69032db0e7d32f94628de3e6eecb50304d7af3d2bcddd937cfe712bea89af368bec4fb1bd7157f6437a664b3bc54a2bfa7e24b3d4f726465722875696e743235362073616c742c61646472657373206d616b65722c61646472657373207369676e65722c75696e7432353620746f6b656e49642c75696e74323536206d616b6572416d6f756e742c75696e743235362074616b6572416d6f756e742c75696e743820736964652c75696e7438207369676e6174757265547970652c75696e743235362074696d657374616d702c62797465733332206d657461646174612c62797465733332206275696c6465722900ba"
	if signature != expected {
		t.Fatalf("signature=%q", signature)
	}
}

func testOrderCredentials(signatureType int32) Credentials {
	return Credentials{
		SignerAddress: "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1",
		FunderAddress: "0x90F8bf6A479f320ead074411a4B0e7944Ea8c9C1",
		PrivateKey:    "4f3edf983ac63ad7c7a0f4a1c2e8b7f5f6f0f4f0a3a5b6c7d8e9f00112233445",
		APIKey:        "key",
		APISecret:     "c2VjcmV0LWtleQ",
		Passphrase:    "pass",
		SignatureType: signatureType,
	}
}
