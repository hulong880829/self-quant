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
	fundingv1 "selfquant/backend/gen/funding/v1"
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

func testRouter(t *testing.T, fundingServer fundingv1.FundingServiceServer) http.Handler {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, fundingServer)
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
		grpc_health_v1.NewHealthClient(connection),
	)
}

func TestFundingSnapshotContract(t *testing.T) {
	router := testRouter(t, &testFundingServer{})
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
}

func TestFundingUnavailableMapping(t *testing.T) {
	router := testRouter(t, &testFundingServer{
		err: status.Error(codes.Unavailable, "offline"),
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/funding-rates", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
