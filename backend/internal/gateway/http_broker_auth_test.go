package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	accountv1 "selfquant/backend/gen/account/v1"
	adminv1 "selfquant/backend/gen/admin/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
	traderv1 "selfquant/backend/gen/trader/v1"
)

type brokerAdminServer struct {
	adminv1.UnimplementedAdminServiceServer
	status       string
	tokenVersion int64
	unavailable  bool
}

func (s *brokerAdminServer) GetBrokerAuthStateInternal(
	_ context.Context,
	request *adminv1.GetBrokerAuthStateInternalRequest,
) (*adminv1.GetBrokerAuthStateInternalResponse, error) {
	if s.unavailable {
		return nil, status.Error(codes.Unavailable, "down")
	}
	st := s.status
	if st == "" {
		st = "active"
	}
	return &adminv1.GetBrokerAuthStateInternalResponse{
		BrokerId: request.GetBrokerId(), Status: st, TokenVersion: s.tokenVersion,
	}, nil
}

type delegatingAccountServer struct {
	testAccountServer
	denyUser      string
	lastDelegated string
}

func (s *delegatingAccountServer) IssueBrokerDelegatedSessionInternal(
	_ context.Context,
	request *accountv1.IssueBrokerDelegatedSessionInternalRequest,
) (*accountv1.IssueBrokerDelegatedSessionInternalResponse, error) {
	s.lastServiceToken = request.GetServiceToken()
	if request.GetTargetUsername() == s.denyUser {
		return nil, status.Error(codes.NotFound, "not found")
	}
	s.lastDelegated = "delegated-" + request.GetTargetUsername()
	s.token = s.lastDelegated
	return &accountv1.IssueBrokerDelegatedSessionInternalResponse{
		SessionToken: s.lastDelegated,
		Username:     request.GetTargetUsername(),
		Permission:   "user",
	}, nil
}

type brokerTraderServer struct {
	traderv1.UnimplementedTraderServiceServer
	lastToken          string
	lastIdempotencyKey string
	placeErr           error
}

func (s *brokerTraderServer) ListInstruments(
	_ context.Context,
	request *traderv1.ListInstrumentsRequest,
) (*traderv1.ListInstrumentsResponse, error) {
	s.lastToken = request.GetToken()
	return &traderv1.ListInstrumentsResponse{
		Capabilities: &traderv1.VenueCapabilities{},
	}, nil
}

func (s *brokerTraderServer) PlaceOrder(
	_ context.Context,
	request *traderv1.PlaceOrderRequest,
) (*traderv1.PlaceOrderResponse, error) {
	s.lastToken = request.GetToken()
	s.lastIdempotencyKey = request.GetIdempotencyKey()
	if s.placeErr != nil {
		return nil, s.placeErr
	}
	return &traderv1.PlaceOrderResponse{
		Order: &traderv1.Order{Id: "ord-1", IdempotencyKey: request.GetIdempotencyKey()},
	}, nil
}

type brokerAuthFixture struct {
	router   http.Handler
	account  *delegatingAccountServer
	admin    *brokerAdminServer
	trader   *brokerTraderServer
	priv     ed25519.PrivateKey
}

func newBrokerAuthFixture(t *testing.T) *brokerAuthFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "broker.pub")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	account := &delegatingAccountServer{denyUser: "foreign-desk"}
	admin := &brokerAdminServer{status: "active", tokenVersion: 1}
	trader := &brokerTraderServer{}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, &testFundingServer{})
	accountv1.RegisterAccountServiceServer(server, account)
	adminv1.RegisterAdminServiceServer(server, admin)
	traderv1.RegisterTraderServiceServer(server, trader)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	router := NewRouter(
		fundingv1.NewFundingServiceClient(connection),
		accountv1.NewAccountServiceClient(connection),
		grpc_health_v1.NewHealthClient(connection),
		Options{
			SessionCookieName:    "sq_session",
			SessionCookieMaxAge:  time.Hour,
			GatewayInternalToken: "gateway-token",
			Admin:                adminv1.NewAdminServiceClient(connection),
			Trader:               traderv1.NewTraderServiceClient(connection),
			BrokerJWTPublicKey:   keyPath,
		},
	)
	return &brokerAuthFixture{
		router: router, account: account, admin: admin, trader: trader, priv: priv,
	}
}

func (f *brokerAuthFixture) jwt(brokerID string, version int64) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	now := time.Now().Unix()
	payload, _ := json.Marshal(brokerClaims{
		Issuer: "selfquant-admin", Subject: brokerID, Audience: brokerAudience,
		Role: "broker_admin", BrokerID: brokerID, TokenVersion: version,
		IssuedAt: now, ExpiresAt: now + 3600, JWTID: "test-jti",
	})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(f.priv, []byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *brokerAuthFixture) do(
	t *testing.T,
	method, path, token, account string,
	cookie bool,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if account != "" {
		request.Header.Set("X-SelfQuant-Account", account)
	}
	if cookie {
		request.AddCookie(&http.Cookie{Name: "sq_session", Value: "web-cookie"})
	}
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func TestBrokerAuthMatrixAccountAndTrader(t *testing.T) {
	fixture := newBrokerAuthFixture(t)
	token := fixture.jwt("broker-1", 1)
	accountPaths := []string{"/api/v1/trading-accounts"}
	traderPaths := []string{"/api/v1/trader/accounts/7/instruments"}
	for _, path := range append(append([]string{}, accountPaths...), traderPaths...) {
		rec := fixture.do(t, http.MethodGet, path, token, "desk-alpha", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s missing no-store", path)
		}
		if strings.Contains(rec.Body.String(), fixture.account.lastDelegated) {
			t.Fatalf("%s leaked delegated session", path)
		}
		if rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s must not set cookie: %s", path, rec.Header().Get("Set-Cookie"))
		}
	}
	if fixture.account.lastServiceToken != "gateway-token" {
		t.Fatalf("service token=%q", fixture.account.lastServiceToken)
	}

	missingBearer := fixture.do(t, http.MethodGet, accountPaths[0], "", "desk-alpha", false)
	if missingBearer.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status=%d", missingBearer.Code)
	}
	missingHeader := fixture.do(t, http.MethodGet, accountPaths[0], token, "", false)
	if missingHeader.Code != http.StatusBadRequest ||
		!strings.Contains(missingHeader.Body.String(), "X-SelfQuant-Account is required") {
		t.Fatalf("missing header status=%d body=%s", missingHeader.Code, missingHeader.Body.String())
	}
	both := fixture.do(t, http.MethodGet, accountPaths[0], token, "desk-alpha", true)
	if both.Code != http.StatusUnauthorized ||
		!strings.Contains(both.Body.String(), "cookie and bearer cannot be used together") {
		t.Fatalf("cookie+bearer status=%d body=%s", both.Code, both.Body.String())
	}
	badJWT := fixture.do(t, http.MethodGet, accountPaths[0], token+"x", "desk-alpha", false)
	if badJWT.Code != http.StatusUnauthorized {
		t.Fatalf("bad jwt status=%d", badJWT.Code)
	}
	versionMismatch := fixture.do(t, http.MethodGet, traderPaths[0], fixture.jwt("broker-1", 9), "desk-alpha", false)
	if versionMismatch.Code != http.StatusUnauthorized {
		t.Fatalf("token_version status=%d", versionMismatch.Code)
	}
	fixture.admin.status = "disabled"
	disabled := fixture.do(t, http.MethodGet, accountPaths[0], fixture.jwt("broker-disabled", 1), "desk-alpha", false)
	if disabled.Code != http.StatusUnauthorized {
		t.Fatalf("disabled status=%d", disabled.Code)
	}
	fixture.admin.status = "active"
	cross := fixture.do(t, http.MethodGet, accountPaths[0], fixture.jwt("broker-cross", 1), "foreign-desk", false)
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-broker status=%d body=%s", cross.Code, cross.Body.String())
	}
	fixture.admin.unavailable = true
	down := fixture.do(t, http.MethodGet, traderPaths[0], fixture.jwt("broker-down", 1), "desk-alpha", false)
	if down.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin down status=%d body=%s", down.Code, down.Body.String())
	}
}

func TestBrokerAuthUnavailableWithoutPublicKey(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{token: "tok"})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/trading-accounts", nil)
	request.Header.Set("Authorization", "Bearer not-a-jwt")
	request.Header.Set("X-SelfQuant-Account", "desk-alpha")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
