package exchange

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corex "selfquant/backend/internal/exchange"
)

func TestLighterDiscoverTreatsZeroAsValidIndex(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	pub, err := corex.LighterPublicKeyHex(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/account":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200, "accounts": []map[string]any{{"account_index": 0}},
			})
		case "/api/v1/apikeys":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code":     200,
				"api_keys": []map[string]any{{"api_key_index": 0, "public_key": pub}},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter := newLighter(server.Client(), server.URL).(*lighterAdapter)
	credentials := Credentials{APIKey: walletAddress, APISecret: walletKey}
	if err := adapter.ensureIndexes(t.Context(), &credentials); err != nil {
		t.Fatal(err)
	}
	if credentials.AccountIndex == nil || *credentials.AccountIndex != 0 ||
		credentials.APIKeyIndex == nil || *credentials.APIKeyIndex != 0 {
		t.Fatalf("credentials=%+v", credentials)
	}
}

func TestLighterDiscoverDoesNotTreatZeroAsUnset(t *testing.T) {
	zeroAccount, zeroKey := int64(0), int32(0)
	adapter := newLighter(&http.Client{}, "http://127.0.0.1").(*lighterAdapter)
	credentials := Credentials{AccountIndex: &zeroAccount, APIKeyIndex: &zeroKey}
	if err := adapter.ensureIndexes(t.Context(), &credentials); err != nil {
		t.Fatal(err)
	}
	if *credentials.AccountIndex != 0 || *credentials.APIKeyIndex != 0 {
		t.Fatalf("indexes were rediscovered: %+v", credentials)
	}
}

func TestLighterDiscoverRejectsAmbiguousKeys(t *testing.T) {
	const walletKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	pub, err := corex.LighterPublicKeyHex(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/account":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200, "accounts": []map[string]any{{"account_index": 3}},
			})
		case "/api/v1/apikeys":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200,
				"api_keys": []map[string]any{
					{"api_key_index": 0, "public_key": pub},
					{"api_key_index": 1, "public_key": pub},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter := newLighter(server.Client(), server.URL).(*lighterAdapter)
	credentials := Credentials{APIKey: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", APISecret: walletKey}
	if err := adapter.ensureIndexes(t.Context(), &credentials); err == nil {
		t.Fatal("expected ambiguous match to fail")
	}
}
