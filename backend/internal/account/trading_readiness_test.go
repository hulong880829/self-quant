package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfquant/backend/internal/exchange"
)

func asterReadinessFromAccountOK(writeSupported bool) TradingReadiness {
	ready := TradingReadiness{
		CredentialsPresent:  true,
		CredentialsVerified: true,
		TradingMode:         CredentialKindAsterAPIWallet,
		TradingStatus:       TradingStatusChecking,
	}
	if !writeSupported {
		return failReadiness(ready, TradingStatusUnsupportedAuthMode, "Aster write path is not available")
	}
	return readyReadiness(ready)
}

func TestAsterReadOnlySuccessDoesNotImplyReady(t *testing.T) {
	ready := asterReadinessFromAccountOK(false)
	if ready.TradingReady || !ready.CredentialsVerified {
		t.Fatalf("readiness=%+v", ready)
	}
}

func TestInspectHyperliquidMainWalletReady(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "hyperliquid", AccountName: "hy",
		APIKey: walletAddress, APISecret: walletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.InspectTradingReadiness(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.TradingReady || !ready.CredentialsVerified || ready.TradingStatus != TradingStatusReady {
		t.Fatalf("readiness=%+v", ready)
	}
}

func TestInspectHyperliquidUnauthorizedAgent(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		otherAddress  = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/info" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode([]map[string]any{})
	}))
	defer server.Close()
	service, _ := newTradingTestService(t)
	service.WithReadiness(server.Client(), map[string]string{"hyperliquid": server.URL})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "hyperliquid", AccountName: "hy",
		APIKey: otherAddress, APISecret: walletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.InspectTradingReadiness(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.TradingReady || ready.TradingUnavailableCode != TradingStatusWalletUnauthorized {
		t.Fatalf("readiness=%+v", ready)
	}
}

func TestInspectAsterAPIWalletReadThenWrite(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/fapi/v3/account" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Query().Get("user") == "" || request.URL.Query().Get("signer") == "" ||
			request.URL.Query().Get("signature") == "" {
			http.Error(writer, "missing auth", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"totalWalletBalance": "1"})
	}))
	defer server.Close()
	service, _ := newTradingTestService(t)
	service.WithReadiness(server.Client(), map[string]string{"aster": server.URL})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "aster", AccountName: "aster",
		APIKey: walletAddress, APISecret: walletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.InspectTradingReadiness(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.CredentialsVerified || !ready.TradingReady {
		t.Fatalf("readiness=%+v", ready)
	}
}

func TestInspectLighterDiscoversZeroIndexes(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	pub, err := exchange.LighterPublicKeyHex(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/account":
			if request.URL.Query().Get("by") != "l1_address" {
				http.Error(writer, "bad by", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200,
				"accounts": []map[string]any{{"account_index": 0, "index": 0}},
			})
		case "/api/v1/apikeys":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200,
				"api_keys": []map[string]any{
					{"api_key_index": 0, "public_key": pub},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service, store := newTradingTestService(t)
	service.WithReadiness(server.Client(), map[string]string{"lighter": server.URL})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "lighter", AccountName: "lg",
		APIKey: walletAddress, APISecret: walletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.records[0].AccountIndex != nil || store.records[0].APIKeyIndex != nil {
		t.Fatal("indexes should start unset")
	}
	ready, err := service.InspectTradingReadiness(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.TradingReady || ready.ResolvedAccountIndex == nil || *ready.ResolvedAccountIndex != 0 ||
		ready.ResolvedAPIKeyIndex == nil || *ready.ResolvedAPIKeyIndex != 0 {
		t.Fatalf("readiness=%+v", ready)
	}
	if store.records[0].AccountIndex == nil || *store.records[0].AccountIndex != 0 ||
		store.records[0].APIKeyIndex == nil || *store.records[0].APIKeyIndex != 0 {
		t.Fatalf("persisted indexes=%+v", store.records[0])
	}
}

func TestInspectLighterAmbiguousKeys(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	pub, err := exchange.LighterPublicKeyHex(walletKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/account":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200,
				"accounts": []map[string]any{{"account_index": 1}},
			})
		case "/api/v1/apikeys":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": 200,
				"api_keys": []map[string]any{
					{"api_key_index": 0, "public_key": pub},
					{"api_key_index": 2, "public_key": pub},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service, _ := newTradingTestService(t)
	service.WithReadiness(server.Client(), map[string]string{"lighter": server.URL})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "lighter", AccountName: "lg",
		APIKey: walletAddress, APISecret: walletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.InspectTradingReadiness(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.TradingReady || ready.TradingUnavailableCode != TradingStatusAPIWalletNotFound {
		t.Fatalf("readiness=%+v", ready)
	}
}

func TestInspectTimeoutIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(writer).Encode([]map[string]any{})
	}))
	defer server.Close()
	service, _ := newTradingTestService(t)
	service.WithReadiness(&http.Client{Timeout: time.Millisecond}, map[string]string{"hyperliquid": server.URL})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "hyperliquid", AccountName: "hy",
		APIKey: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		APISecret: "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.InspectTradingReadiness(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.TradingReady || (ready.TradingStatus != TradingStatusUnknown &&
		ready.TradingUnavailableCode != TradingStatusUnknown &&
		!strings.Contains(ready.TradingUnavailableReason, "timed out") &&
		ready.TradingUnavailableCode != TradingStatusVenueUnavailable) {
		t.Fatalf("readiness=%+v", ready)
	}
}
