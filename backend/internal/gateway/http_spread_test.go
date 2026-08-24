package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
	spreadv1 "selfquant/backend/gen/spread/v1"
)

type testSpreadServer struct {
	spreadv1.UnimplementedSpreadServiceServer
	err error
}

func (s *testSpreadServer) GetBasisSpreadHistory(
	_ context.Context,
	request *spreadv1.GetBasisSpreadHistoryRequest,
) (*spreadv1.GetBasisSpreadHistoryResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	now := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	return &spreadv1.GetBasisSpreadHistoryResponse{
		Venue: request.GetVenue(), CompareVenue: request.GetCompareVenue(),
		BaseAsset: request.GetBaseAsset(), QuoteAsset: request.GetQuoteAsset(),
		CanonicalSymbol: "BTCUSDT",
		Range:           request.GetRange(), ResolutionSeconds: 60,
		Availability: spreadv1.BasisSpreadAvailability_BASIS_SPREAD_AVAILABILITY_AVAILABLE,
		AsOf:         timestamppb.New(now),
		Points: []*spreadv1.BasisSpreadPoint{{
			Ts: timestamppb.New(now), SpreadBps: "12.5", SpotAsk: "100",
			PerpetualAsk: "100.125", Samples: 2,
		}},
		Summary: &spreadv1.BasisSpreadSummary{
			CurrentBps: "12.5", MinBps: "12.5", MaxBps: "12.5", AvgBps: "12.5",
			Coverage: "0.01",
		},
	}, nil
}

func spreadRouter(t *testing.T, spreadServer spreadv1.SpreadServiceServer) http.Handler {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, &testFundingServer{})
	accountv1.RegisterAccountServiceServer(server, &testAccountServer{})
	spreadv1.RegisterSpreadServiceServer(server, spreadServer)
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
		Options{
			SessionCookieName: "sq_session", SessionCookieMaxAge: time.Hour,
			Spread:       spreadv1.NewSpreadServiceClient(connection),
			SpreadHealth: grpc_health_v1.NewHealthClient(connection),
		},
	)
}

func TestBasisSpreadHistoryContractAndETag(t *testing.T) {
	router := spreadRouter(t, &testSpreadServer{})
	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/basis-spreads/Binance/btc/usdt/history?range=24h", nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["venue"] != "binance" || payload["canonicalSymbol"] != "BTCUSDT" ||
		payload["availability"] != "available" {
		t.Fatalf("payload=%v", payload)
	}
	etag := recorder.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing etag")
	}
	repeat := httptest.NewRequest(
		http.MethodGet, "/api/v1/basis-spreads/binance/BTC/USDT/history?range=24h", nil,
	)
	repeat.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	router.ServeHTTP(second, repeat)
	if second.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestBasisSpreadHistoryForwardsCompareVenue(t *testing.T) {
	router := spreadRouter(t, &testSpreadServer{})
	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/basis-spreads/binance/BTC/USDT/history?range=24h&compareVenue=okx", nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["venue"] != "binance" || payload["compareVenue"] != "okx" {
		t.Fatalf("payload=%v", payload)
	}
}

func TestBasisSpreadHistoryRejectsSameCompareVenue(t *testing.T) {
	router := spreadRouter(t, &testSpreadServer{})
	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/basis-spreads/binance/BTC/USDT/history?range=24h&compareVenue=binance", nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBasisSpreadHistoryRejectsInvalidRange(t *testing.T) {
	router := spreadRouter(t, &testSpreadServer{})
	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/basis-spreads/binance/BTC/USDT/history?range=2h", nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBasisSpreadHistoryTimeoutMapping(t *testing.T) {
	router := spreadRouter(t, &testSpreadServer{err: status.Error(codes.DeadlineExceeded, "slow")})
	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/basis-spreads/binance/BTC/USDT/history?range=1h", nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
