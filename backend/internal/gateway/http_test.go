package gateway

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"google.golang.org/protobuf/types/known/timestamppb"
	accountv1 "selfquant/backend/gen/account/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
	reportv1 "selfquant/backend/gen/report/v1"
)

type testFundingServer struct {
	fundingv1.UnimplementedFundingServiceServer
	err error
}

func (s *testFundingServer) ListFundingRates(
	_ context.Context,
	_ *fundingv1.ListFundingRatesRequest,
) (*fundingv1.ListFundingRatesResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	now := time.Now().UTC()
	return &fundingv1.ListFundingRatesResponse{
		Items: []*fundingv1.FundingRate{{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT",
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			PositionQuantity: "100", PositionNotionalUsd: "6000000",
			Turnover_24HUsd: "120000000", FundingRate: "0.0001",
			AnnualizedRate: "0.1095", FundingIntervalSeconds: 28800,
			NextFundingAt: timestamppb.New(now.Add(time.Hour)),
			MarkPrice:     "60000", IndexPrice: "59990", LastPrice: "60001",
			PriceChange_24H: "0.01", Cumulative_24H: "0.0003",
			Cumulative_7D: "0.0021", SourceUpdatedAt: timestamppb.New(now),
			History: []*fundingv1.FundingHistoryPoint{{
				Rate: "0.0001", SettledAt: timestamppb.New(now.Add(-8 * time.Hour)),
			}},
		}},
		Total: 1, SnapshotVersion: "123", ServerTime: timestamppb.New(now),
	}, nil
}

func (s *testFundingServer) ListFundingSpreads(
	_ context.Context,
	_ *fundingv1.ListFundingSpreadsRequest,
) (*fundingv1.ListFundingSpreadsResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	now := time.Now().UTC()
	return &fundingv1.ListFundingSpreadsResponse{
		Items: []*fundingv1.FundingSpread{{
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			LongLeg: &fundingv1.FundingSpreadLeg{
				Exchange: "binance", ExchangeSymbol: "BTCUSDT",
				EffectiveFundingRate: "0.0001", FundingIntervalSeconds: 28800,
				NextFundingAt:       timestamppb.New(now.Add(time.Hour)),
				PositionNotionalUsd: "5000000", Turnover_24HUsd: "20000000",
				LastPrice: "60000", SourceUpdatedAt: timestamppb.New(now),
			},
			ShortLeg: &fundingv1.FundingSpreadLeg{
				Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP",
				EffectiveFundingRate: "0.0002", FundingIntervalSeconds: 28800,
				NextFundingAt:       timestamppb.New(now.Add(time.Hour)),
				PositionNotionalUsd: "4000000", Turnover_24HUsd: "30000000",
				LastPrice: "60010", SourceUpdatedAt: timestamppb.New(now),
			},
			SingleSpreadAnnualized: "0.1095",
			Spread_24HAnnualized:   "0.073", Spread_7DAnnualized: "0.052",
			MinPositionNotionalUsd: "4000000", MinTurnover_24HUsd: "20000000",
			UpdatedAt: timestamppb.New(now),
		}},
		Total: 1, SnapshotVersion: "123", ServerTime: timestamppb.New(now),
	}, nil
}

func (s *testFundingServer) ListFundingOpportunities(
	_ context.Context,
	request *fundingv1.ListFundingOpportunitiesRequest,
) (*fundingv1.ListFundingOpportunitiesResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if request.GetPeriod() != "1h" {
		return nil, status.Error(codes.InvalidArgument, "invalid period")
	}
	now := time.Now().UTC()
	return &fundingv1.ListFundingOpportunitiesResponse{
		Items: []*fundingv1.FundingOpportunityRanking{{
			Rank: 1, GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			Period: "1h",
			LongLeg: &fundingv1.FundingSpreadLeg{
				Exchange: "binance", ExchangeSymbol: "BTCUSDT",
				EffectiveFundingRate: "0.0001", FundingIntervalSeconds: 28800,
				NextFundingAt:       timestamppb.New(now.Add(time.Hour)),
				PositionNotionalUsd: "5000000", Turnover_24HUsd: "20000000",
				SourceUpdatedAt: timestamppb.New(now),
			},
			ShortLeg: &fundingv1.FundingSpreadLeg{
				Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP",
				EffectiveFundingRate: "0.0002", FundingIntervalSeconds: 28800,
				NextFundingAt:       timestamppb.New(now.Add(time.Hour)),
				PositionNotionalUsd: "4000000", Turnover_24HUsd: "30000000",
				SourceUpdatedAt: timestamppb.New(now),
			},
			CurrentMidSpreadBps: "12", CurrentExecutableSpreadBps: "10",
			TargetSpreadBps: "2", PeriodExpectedReturn: "0.001",
			FundingExpectedAnnualized: "0.2", SpreadExpectedAnnualized: "0.3",
			CombinedExpectedAnnualized: "0.5", FirstPassageProbability: "0.8",
			ProfitProbability: "0.75", ExpectedExitMinutes: "32", P5Return: "-0.0002",
			MinPositionNotionalUsd: "4000000", MinTurnover_24HUsd: "20000000",
			Coverage: "0.98", Confidence: "0.85", ModelState: "ready",
			UpdatedAt: timestamppb.New(now),
		}},
		Total: 1, SnapshotVersion: "rank-123", ServerTime: timestamppb.New(now),
		CalculatedAt: timestamppb.New(now), Stale: false,
	}, nil
}

func (s *testFundingServer) GetFundingHistory(
	_ context.Context,
	_ *fundingv1.GetFundingHistoryRequest,
) (*fundingv1.GetFundingHistoryResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &fundingv1.GetFundingHistoryResponse{
		Items: []*fundingv1.FundingHistoryPoint{{
			Rate: "0.0001", SettledAt: timestamppb.New(time.Now().UTC()),
		}},
	}, nil
}

type testAccountServer struct {
	accountv1.UnimplementedAccountServiceServer
	loginErr    error
	validateErr error
	token       string
	accounts    []*accountv1.TradingAccount
	nextID      int64
}

func (s *testAccountServer) Login(
	_ context.Context,
	request *accountv1.LoginRequest,
) (*accountv1.LoginResponse, error) {
	if s.loginErr != nil {
		return nil, s.loginErr
	}
	if request.GetUsername() != "admin" || request.GetPassword() != "admin123" {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	token := s.token
	if token == "" {
		token = "signed-token"
	}
	return &accountv1.LoginResponse{
		Token: token, Username: "admin", Permission: "admin",
	}, nil
}

func (s *testAccountServer) ValidateSession(
	_ context.Context,
	request *accountv1.ValidateSessionRequest,
) (*accountv1.ValidateSessionResponse, error) {
	if s.validateErr != nil {
		return nil, s.validateErr
	}
	expected := s.token
	if expected == "" {
		expected = "signed-token"
	}
	if request.GetToken() != expected {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	return &accountv1.ValidateSessionResponse{
		Username: "admin", Permission: "admin",
	}, nil
}

func (s *testAccountServer) requireToken(token string) error {
	expected := s.token
	if expected == "" {
		expected = "signed-token"
	}
	if token != expected {
		return status.Error(codes.Unauthenticated, "invalid session")
	}
	return nil
}

func (s *testAccountServer) ListTradingAccounts(
	_ context.Context,
	request *accountv1.ListTradingAccountsRequest,
) (*accountv1.ListTradingAccountsResponse, error) {
	if err := s.requireToken(request.GetToken()); err != nil {
		return nil, err
	}
	return &accountv1.ListTradingAccountsResponse{Items: s.accounts}, nil
}

func (s *testAccountServer) GetTradingAccountSnapshot(
	_ context.Context,
	request *accountv1.GetTradingAccountSnapshotRequest,
) (*accountv1.GetTradingAccountSnapshotResponse, error) {
	if err := s.requireToken(request.GetToken()); err != nil {
		return nil, err
	}
	return &accountv1.GetTradingAccountSnapshotResponse{
		Snapshot: &accountv1.TradingAccountSnapshot{
			TradingAccountId: request.GetTradingAccountId(), Exchange: "binance",
			AccountEquityUsd: "100", AvailableFundsUsd: "80", RiskPercent: "5", SourceUpdatedAt: timestamppb.Now(),
			Positions: []*accountv1.PortfolioPosition{{
				Key: "btc", Kind: "cex", Symbol: "BTCUSDT", Side: "long",
				NotionalUsd: "12", SpotSize: "1.5", SignedContractSize: "2",
			}},
		},
		ServerTime: timestamppb.Now(),
	}, nil
}

func (s *testAccountServer) GetProductGroupSnapshot(
	_ context.Context,
	request *accountv1.GetProductGroupSnapshotRequest,
) (*accountv1.GetProductGroupSnapshotResponse, error) {
	if err := s.requireToken(request.GetToken()); err != nil {
		return nil, err
	}
	return &accountv1.GetProductGroupSnapshotResponse{
		Snapshot: &accountv1.ProductGroupSnapshot{
			ProductName: request.GetProductName(), AccountCount: 2, SourceUpdatedAt: timestamppb.Now(),
			AccountEquityUsd: "200", AvailableFundsUsd: "160",
			Positions: []*accountv1.ProductGroupPosition{{
				Symbol: "BTC", TotalNotionalUsd: "24", SpotSize: "1.5", ContractSize: "-2",
			}},
		},
		ServerTime: timestamppb.Now(),
	}, nil
}

func (s *testAccountServer) CreateTradingAccount(
	_ context.Context,
	request *accountv1.CreateTradingAccountRequest,
) (*accountv1.CreateTradingAccountResponse, error) {
	if err := s.requireToken(request.GetToken()); err != nil {
		return nil, err
	}
	if request.GetProductName() == "" || request.GetExchange() == "" ||
		request.GetAccountName() == "" || request.GetApiKey() == "" ||
		request.GetApiSecret() == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid trading account")
	}
	s.nextID++
	now := timestamppb.New(time.Now().UTC())
	created := &accountv1.TradingAccount{
		Id: s.nextID, ProductName: request.GetProductName(),
		Exchange:    strings.ToLower(request.GetExchange()),
		AccountName: request.GetAccountName(), ApiKeyMasked: "abcd****mnop",
		HasPassphrase: request.GetPassphrase() != "", CreatedAt: now, UpdatedAt: now,
	}
	s.accounts = append(s.accounts, created)
	return &accountv1.CreateTradingAccountResponse{Account: created}, nil
}

func (s *testAccountServer) DeleteTradingAccount(
	_ context.Context,
	request *accountv1.DeleteTradingAccountRequest,
) (*accountv1.DeleteTradingAccountResponse, error) {
	if err := s.requireToken(request.GetToken()); err != nil {
		return nil, err
	}
	for index, item := range s.accounts {
		if item.GetId() == request.GetId() {
			s.accounts = append(s.accounts[:index], s.accounts[index+1:]...)
			return &accountv1.DeleteTradingAccountResponse{}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "trading account not found")
}

func testRouter(
	t *testing.T,
	fundingServer fundingv1.FundingServiceServer,
	accountServer accountv1.AccountServiceServer,
) http.Handler {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, fundingServer)
	if accountServer == nil {
		accountServer = &testAccountServer{}
	}
	accountv1.RegisterAccountServiceServer(server, accountServer)
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
	return NewRouter(
		fundingv1.NewFundingServiceClient(connection),
		accountv1.NewAccountServiceClient(connection),
		grpc_health_v1.NewHealthClient(connection),
		Options{SessionCookieName: "sq_session", SessionCookieMaxAge: time.Hour},
	)
}

func TestFundingSnapshotContract(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/funding-rates", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Meta.Total != 1 || len(response.Data) != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.Data[0]["currentFundingRate"] != "0.0001" {
		t.Fatalf("decimal contract changed: %+v", response.Data[0])
	}
	if response.Data[0]["nextFundingRate"] != nil {
		t.Fatalf("missing prediction must be null: %+v", response.Data[0])
	}
	if _, exists := response.Data[0]["fundingHistory"]; exists {
		t.Fatalf("list response must omit history: %+v", response.Data[0])
	}
	if recorder.Header().Get("ETag") != `"123"` {
		t.Fatalf("etag=%q", recorder.Header().Get("ETag"))
	}
}

func TestFundingSpreadSnapshotContractAndETag(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/funding-spreads", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Meta.Total != 1 || len(response.Data) != 1 ||
		response.Data[0]["spreadAnnualized"] != "0.1095" {
		t.Fatalf("response=%+v", response)
	}
	longLeg, ok := response.Data[0]["longLeg"].(map[string]any)
	if !ok || longLeg["exchange"] != "Binance" ||
		longLeg["settlementIntervalHours"] != float64(8) {
		t.Fatalf("longLeg=%+v", response.Data[0]["longLeg"])
	}
	if recorder.Header().Get("ETag") != `"123"` {
		t.Fatalf("etag=%q", recorder.Header().Get("ETag"))
	}

	notModified := httptest.NewRequest(http.MethodGet, "/api/v1/funding-spreads", nil)
	notModified.Header.Set("If-None-Match", `"123"`)
	notModifiedRecorder := httptest.NewRecorder()
	router.ServeHTTP(notModifiedRecorder, notModified)
	if notModifiedRecorder.Code != http.StatusNotModified ||
		notModifiedRecorder.Body.Len() != 0 {
		t.Fatalf(
			"status=%d body=%q",
			notModifiedRecorder.Code, notModifiedRecorder.Body.String(),
		)
	}
}

func TestFundingOpportunityContractAndETag(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	target := "/api/v1/funding-opportunities?period=1h&minLegNotionalUsd=1000000&minLegVolume24hUsd=1000000"
	request := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Meta.Total != 1 || len(response.Data) != 1 ||
		response.Data[0]["combinedExpectedAnnualized"] != "0.5" {
		t.Fatalf("response=%+v", response)
	}
	etag := recorder.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing etag")
	}
	notModified := httptest.NewRequest(http.MethodGet, target, nil)
	notModified.Header.Set("If-None-Match", etag)
	notModifiedRecorder := httptest.NewRecorder()
	router.ServeHTTP(notModifiedRecorder, notModified)
	if notModifiedRecorder.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%s", notModifiedRecorder.Code, notModifiedRecorder.Body.String())
	}
}

func TestFundingSnapshotReturnsNotModified(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/funding-rates", nil)
	request.Header.Set("If-None-Match", `"123"`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("ETag") != `"123"` {
		t.Fatalf("etag=%q", recorder.Header().Get("ETag"))
	}
}

func TestFundingSnapshotSupportsGzip(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/funding-rates", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if recorder.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("content encoding=%q", recorder.Header().Get("Content-Encoding"))
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
}

func TestFundingCORSAllowsETag(t *testing.T) {
	t.Setenv("GATEWAY_CORS_ORIGIN", "http://example.test")
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/funding-rates", nil)
	request.Header.Set("Origin", "http://example.test")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d", recorder.Code)
	}
	if !strings.Contains(
		recorder.Header().Get("Access-Control-Allow-Headers"),
		"If-None-Match",
	) {
		t.Fatalf("allow headers=%q", recorder.Header().Get("Access-Control-Allow-Headers"))
	}
	if recorder.Header().Get("Access-Control-Expose-Headers") != "ETag" {
		t.Fatalf("expose headers=%q", recorder.Header().Get("Access-Control-Expose-Headers"))
	}
	if recorder.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("credentials=%q", recorder.Header().Get("Access-Control-Allow-Credentials"))
	}
	if !strings.Contains(recorder.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("allow methods=%q", recorder.Header().Get("Access-Control-Allow-Methods"))
	}
}

func TestFundingHistoryContract(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/funding-rates/binance/BTCUSDT/history?limit=10",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0]["rate"] != "0.0001" {
		t.Fatalf("response=%+v", response)
	}
}

func TestFundingUnavailableMapping(t *testing.T) {
	router := testRouter(t, &testFundingServer{
		err: status.Error(codes.Unavailable, "offline"),
	}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/funding-rates", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAuthLoginSessionLogoutCookie(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{token: "tok-1"})

	loginReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin123"}`),
	)
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginRec.Code, loginRec.Body.String())
	}
	cookie := loginRec.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != "sq_session" || cookie[0].Value != "tok-1" {
		t.Fatalf("cookies=%+v", cookie)
	}
	if !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags=%+v", cookie[0])
	}
	var loginBody map[string]any
	if err := json.Unmarshal(loginRec.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	if loginBody["authenticated"] != true || loginBody["username"] != "admin" {
		t.Fatalf("login body=%+v", loginBody)
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	sessionReq.AddCookie(cookie[0])
	sessionRec := httptest.NewRecorder()
	router.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusOK {
		t.Fatalf("session status=%d", sessionRec.Code)
	}
	var sessionBody map[string]any
	if err := json.Unmarshal(sessionRec.Body.Bytes(), &sessionBody); err != nil {
		t.Fatal(err)
	}
	if sessionBody["authenticated"] != true || sessionBody["permission"] != "admin" {
		t.Fatalf("session body=%+v", sessionBody)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status=%d", logoutRec.Code)
	}
	cleared := logoutRec.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge != -1 || cleared[0].Value != "" {
		t.Fatalf("cleared cookie=%+v", cleared)
	}
}

func TestAuthLoginRejectsBadCredentials(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"wrong"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "invalid credentials" {
		t.Fatalf("body=%+v", body)
	}
}

func TestAuthSessionAnonymousWithoutCookie(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["authenticated"] != false {
		t.Fatalf("body=%+v", body)
	}
}

func TestTradingAccountsRequireAuth(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{token: "tok-1"})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/trading-accounts", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTradingAccountSnapshotContracts(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{token: "tok-1"})
	for _, path := range []string{
		"/api/v1/trading-accounts/7/snapshot",
		"/api/v1/trading-account-products/funding-arb/snapshot",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: "sq_session", Value: "tok-1"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s cache control missing", path)
		}
		if strings.Contains(recorder.Body.String(), "apiSecret") {
			t.Fatalf("secret leaked: %s", recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"accountEquityUsd"`) ||
			!strings.Contains(recorder.Body.String(), `"availableFundsUsd"`) {
			t.Fatalf("snapshot totals missing: %s", recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"spotSize":"1.5"`) {
			t.Fatalf("spot size missing: %s", recorder.Body.String())
		}
		if strings.Contains(path, "products") {
			if !strings.Contains(recorder.Body.String(), `"contractSize":"-2"`) {
				t.Fatalf("contract size missing: %s", recorder.Body.String())
			}
		} else if !strings.Contains(recorder.Body.String(), `"signedContractSize":"2"`) {
			t.Fatalf("signed contract size missing: %s", recorder.Body.String())
		}
	}
}

func TestTradingAccountsCRUDContract(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, &testAccountServer{token: "tok-1"})
	cookie := &http.Cookie{Name: "sq_session", Value: "tok-1"}

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/trading-accounts",
		strings.NewReader(`{
			"productName":"Funding Arb",
			"exchange":"binance",
			"accountName":"main",
			"apiKey":"abcdefghijklmnop",
			"apiSecret":"secret",
			"passphrase":"phrase"
		}`),
	)
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(cookie)
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data["accountName"] != "main" || created.Data["apiKeyMasked"] == nil {
		t.Fatalf("created=%+v", created.Data)
	}
	if _, exists := created.Data["apiSecret"]; exists {
		t.Fatalf("secret leaked: %+v", created.Data)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/trading-accounts", nil)
	listReq.AddCookie(cookie)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Meta["total"] != float64(1) || len(listed.Data) != 1 {
		t.Fatalf("listed=%+v", listed)
	}

	id := int64(listed.Data[0]["id"].(float64))
	deleteReq := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/trading-accounts/"+strconv.FormatInt(id, 10),
		nil,
	)
	deleteReq.AddCookie(cookie)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

func TestTradingAccountsCORSAllowsDelete(t *testing.T) {
	t.Setenv("GATEWAY_CORS_ORIGIN", "http://example.test")
	router := testRouter(t, &testFundingServer{}, &testAccountServer{})
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/trading-accounts", nil)
	request.Header.Set("Origin", "http://example.test")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Header().Get("Access-Control-Allow-Methods"), "DELETE") {
		t.Fatalf("allow methods=%q", recorder.Header().Get("Access-Control-Allow-Methods"))
	}
}

func TestReportDailyJSONIncludesVolumeAndQuality(t *testing.T) {
	item := &reportv1.DailySnapshot{
		Id: 7, ProductId: 3, ReportDate: "2026-08-13",
		OpeningEquityUsd: "100", ClosingEquityUsd: "110",
		NetCashFlowUsd: "0", PnlUsd: "10", ReturnRate: "0.1",
		AbsoluteReturn: "10", Volume_24HUsd: "25.50", Status: "partial",
	}
	got := reportDailyJSON(item)
	if got["volume24h"] != "25.50" {
		t.Fatalf("volume24h=%v", got["volume24h"])
	}
	if got["aum"] != "110" || got["absoluteReturn"] != "10" {
		t.Fatalf("daily json=%+v", got)
	}
	if got["status"] != "provisional" || got["partial"] != true {
		t.Fatalf("quality json=%+v", got)
	}
	if got["volume24h"] == nil {
		t.Fatal("volume24h must stay a decimal string when present")
	}
	empty := reportDailyJSON(&reportv1.DailySnapshot{ReportDate: "2026-08-13"})
	if empty["volume24h"] != nil {
		t.Fatalf("empty volume must be null: %+v", empty)
	}
}

func TestReportAUMSeriesJSONReversesNewestFirstDailyRows(t *testing.T) {
	daily := []*reportv1.DailySnapshot{
		{
			ReportDate:       "2026-08-16",
			ClosingEquityUsd: "9971.00",
		},
		{
			ReportDate:       "2026-08-15",
			ClosingEquityUsd: "9968.26",
		},
	}

	series := reportAUMSeriesJSON(daily)
	if len(series) != 2 {
		t.Fatalf("series length=%d want=2", len(series))
	}
	if series[0]["date"] != "2026-08-15" ||
		series[0]["aum"] != "9968.26" ||
		series[1]["date"] != "2026-08-16" ||
		series[1]["aum"] != "9971.00" {
		t.Fatalf("series must be oldest-first: %+v", series)
	}
	if daily[0].GetReportDate() != "2026-08-16" {
		t.Fatalf("daily rows were mutated: %+v", daily)
	}
}
