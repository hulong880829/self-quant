package exchange

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBinanceAccountProfileCompliantIsNoOp(t *testing.T) {
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-MBX-APIKEY") != "key" ||
			!strings.Contains(request.URL.RawQuery, "signature=") {
			t.Fatalf("missing Binance authentication: headers=%v query=%s", request.Header, request.URL.RawQuery)
		}
		if request.Method != http.MethodGet {
			writes++
		}
		switch request.URL.Path {
		case "/papi/v1/account":
			_, _ = writer.Write([]byte(`{}`))
		case "/papi/v1/um/positionSide/dual":
			_, _ = writer.Write([]byte(`{"dualSidePosition":false}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager := newBinance(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		AccountProfileRequest{},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusCompliant || writes != 0 {
		t.Fatalf("result=%+v writes=%d err=%v", result, writes, err)
	}
	if len(result.Steps) != 3 {
		t.Fatalf("steps=%+v", result.Steps)
	}
}

func TestOKXAccountProfileAppliesPresetPrecheckLevelAndPosition(t *testing.T) {
	accountLevel := "1"
	positionMode := "long_short_mode"
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		seen = append(seen, request.Method+" "+request.URL.RequestURI()+" "+string(body))
		if request.Header.Get("OK-ACCESS-KEY") != "key" ||
			request.Header.Get("OK-ACCESS-SIGN") == "" {
			t.Fatalf("missing OKX authentication: %v", request.Header)
		}
		switch request.URL.Path {
		case "/api/v5/account/config":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": "0",
				"data": []map[string]string{{
					"acctLv": accountLevel, "posMode": positionMode,
				}},
			})
		case "/api/v5/account/set-account-switch-preset":
			_, _ = writer.Write([]byte(`{"code":"0","data":[{}]}`))
		case "/api/v5/account/set-account-switch-precheck":
			if request.URL.Query().Get("acctLv") != "3" {
				t.Fatalf("precheck query=%s", request.URL.RawQuery)
			}
			_, _ = writer.Write([]byte(`{"code":"0","data":[{"unmatchedInfo":[]}]}`))
		case "/api/v5/account/set-account-level":
			accountLevel = "3"
			_, _ = writer.Write([]byte(`{"code":"0","data":[{}]}`))
		case "/api/v5/account/set-position-mode":
			positionMode = "net_mode"
			_, _ = writer.Write([]byte(`{"code":"0","data":[{}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager := newOKX(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"},
		AccountProfileRequest{},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusApplied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{
		"POST /api/v5/account/set-account-switch-preset",
		"GET /api/v5/account/set-account-switch-precheck?acctLv=3",
		"POST /api/v5/account/set-account-level",
		"POST /api/v5/account/set-position-mode",
		`"posMode":"net_mode"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in requests:\n%s", want, joined)
		}
	}
}

func TestOKXAccountProfileStopsOnPrecheckFailure(t *testing.T) {
	var levelWrite bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v5/account/config":
			_, _ = writer.Write([]byte(`{"code":"0","data":[{"acctLv":"1","posMode":"net_mode"}]}`))
		case "/api/v5/account/set-account-switch-preset":
			_, _ = writer.Write([]byte(`{"code":"0","data":[{}]}`))
		case "/api/v5/account/set-account-switch-precheck":
			_, _ = writer.Write([]byte(
				`{"code":"0","data":[{"unmatchedInfo":[{"name":"open orders"}]}]}`,
			))
		case "/api/v5/account/set-account-level":
			levelWrite = true
			_, _ = writer.Write([]byte(`{"code":"0"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager := newOKX(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(), Credentials{APISecret: "secret"}, AccountProfileRequest{},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusManualRequired || levelWrite {
		t.Fatalf("result=%+v levelWrite=%v err=%v", result, levelWrite, err)
	}
}

func TestBybitAccountProfileSetsOneWayBySettlementCoin(t *testing.T) {
	var switchBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		switch request.URL.Path {
		case "/v5/account/info":
			_, _ = writer.Write([]byte(
				`{"retCode":0,"result":{"unifiedMarginStatus":5,"marginMode":"REGULAR_MARGIN"}}`,
			))
		case "/v5/position/switch-mode":
			switchBody = string(body)
			_, _ = writer.Write([]byte(`{"retCode":0,"retMsg":"OK","result":{}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager := newBybit(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		AccountProfileRequest{Instruments: []Instrument{{
			ContractType: "perpetual", SettleAsset: "USDT",
		}}},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusApplied ||
		!strings.Contains(switchBody, `"coin":"USDT"`) ||
		!strings.Contains(switchBody, `"mode":0`) {
		t.Fatalf("result=%+v body=%s err=%v", result, switchBody, err)
	}
}

func TestBybitAccountProfileReturnsPendingUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v5/account/info":
			_, _ = writer.Write([]byte(
				`{"retCode":0,"result":{"unifiedMarginStatus":1,"marginMode":"ISOLATED_MARGIN"}}`,
			))
		case "/v5/account/upgrade-to-uta":
			_, _ = writer.Write([]byte(
				`{"retCode":0,"result":{"unifiedUpdateStatus":"PROCESS"}}`,
			))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager := newBybit(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(), Credentials{APISecret: "secret"}, AccountProfileRequest{},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusPending {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestBitgetAccountProfileCompliantIsNoOp(t *testing.T) {
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writes++
		}
		if request.URL.Path != "/api/v3/account/settings" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(
			`{"code":"00000","data":{"accountMode":"unified","accountLevel":"advanced","assetMode":"multi_assets","holdMode":"one_way_mode"}}`,
		))
	}))
	defer server.Close()
	manager := newBitget(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(), Credentials{APISecret: "secret"}, AccountProfileRequest{},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusCompliant || writes != 0 {
		t.Fatalf("result=%+v writes=%d err=%v", result, writes, err)
	}
}

func TestBitgetAccountProfileReturnsPendingUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v3/account/settings":
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":"40001","msg":"classic account"}`))
		case "/api/v2/spot/account/upgrade-status":
			_, _ = writer.Write([]byte(
				`{"code":"00000","data":{"status":"process","reason":""}}`,
			))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager := newBitget(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(), Credentials{APISecret: "secret"}, AccountProfileRequest{},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusPending {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestGateAccountProfileCompliantIsNoOp(t *testing.T) {
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writes++
		}
		switch request.URL.Path {
		case "/api/v4/unified/unified_mode":
			_, _ = writer.Write([]byte(`{"mode":"multi_currency","settings":{"usdt_futures":true}}`))
		case "/api/v4/futures/usdt/accounts":
			_, _ = writer.Write([]byte(`{"position_mode":"single","in_dual_mode":false}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager := newGate(server.Client(), server.URL).(AccountProfileManager)
	result, err := manager.ApplyAccountProfile(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		AccountProfileRequest{Instruments: []Instrument{{
			ContractType: "perpetual", SettleAsset: "USDT",
		}}},
	)
	if err != nil || result.OverallStatus != AccountProfileStatusCompliant || writes != 0 {
		t.Fatalf("result=%+v writes=%d err=%v", result, writes, err)
	}
}
