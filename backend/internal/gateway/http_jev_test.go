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
	jevv1 "selfquant/backend/gen/jev/v1"
)

type testJevServer struct {
	jevv1.UnimplementedJevServiceServer
	snapshotErr error
	etag        string
}

func (s *testJevServer) GetSnapshot(
	_ context.Context,
	request *jevv1.GetSnapshotRequest,
) (*jevv1.GetSnapshotResponse, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	etag := s.etag
	if etag == "" {
		etag = `"9"`
	}
	if request.GetIfNoneMatch() == etag {
		return &jevv1.GetSnapshotResponse{NotModified: true, Etag: etag, SnapshotVersion: "9"}, nil
	}
	now := time.Now().UTC()
	return &jevv1.GetSnapshotResponse{
		Etag: etag, SnapshotVersion: "9", Status: "degraded", DegradedReasons: []string{"typesafe_error"},
		ServerTime: timestamppb.New(now), Simulated: true, Equity: "10000", NetPnl: "0",
		PaperRoundTripCostBps: "12", PromptVersion: "DecisionPromptV4", PolicyVersion: "JEVPolicyV3",
		SingleFilter: &jevv1.JevSingleFilterStats{Input: 12, ContractPassed: 10, BasisForecastPassed: 2, Final: 1},
		CrossFilter:  &jevv1.JevCrossFilterStats{Input: 8, ContractPassed: 8, BasisForecastPassed: 1, Final: 0},
	}, nil
}

func (s *testJevServer) ListDecisions(
	_ context.Context,
	_ *jevv1.ListDecisionsRequest,
) (*jevv1.ListDecisionsResponse, error) {
	return &jevv1.ListDecisionsResponse{
		Etag: `"9"`,
		Items: []*jevv1.JevDecision{{
			Id: "1", CandidateKey: "single:binance:BTCUSDT:BTCUSDT", Decision: "hold",
			PolicyStatus: "accepted", ExecutionStatus: "not_applicable", PromptVersion: "DecisionPromptV4",
			PolicyVersion: "JEVPolicyV3",
		}, {
			Id: "2", CandidateKey: "single:okx:ETHUSDT:ETHUSDT", Decision: "enter",
			PolicyStatus: "suggested", ExecutionStatus: "portfolio_rejected",
		}, {
			Id: "3", CandidateKey: "single:binance:SOLUSDT:SOLUSDT", Decision: "enter",
			PolicyStatus: "accepted", ExecutionStatus: "fill_failed",
		}},
	}, nil
}

func testRouterWithJev(t *testing.T, jevServer jevv1.JevServiceServer) http.Handler {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, &testFundingServer{})
	accountv1.RegisterAccountServiceServer(server, &testAccountServer{})
	jevv1.RegisterJevServiceServer(server, jevServer)
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
			Jev: jevv1.NewJevServiceClient(connection),
		},
	)
}

func TestJevSnapshotUnavailableWithoutClient(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/jev-arb/snapshot", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestJevSnapshotETagAndDegraded200(t *testing.T) {
	router := testRouterWithJev(t, &testJevServer{etag: `"9"`})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/jev-arb/snapshot", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "degraded" || payload["paperRoundTripCostBps"] != "12" {
		t.Fatalf("%+v", payload)
	}
	single, _ := payload["singleFilter"].(map[string]any)
	if single["input"] != float64(12) || single["contractPassed"] != float64(10) ||
		single["basisForecastPassed"] != float64(2) || single["final"] != float64(1) {
		t.Fatalf("singleFilter %+v", single)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/jev-arb/snapshot", nil)
	request.Header.Set("If-None-Match", `"9"`)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotModified {
		t.Fatalf("etag status=%d", recorder.Code)
	}
}

func TestJevSnapshotNoSnapshotIs503(t *testing.T) {
	router := testRouterWithJev(t, &testJevServer{snapshotErr: status.Error(codes.FailedPrecondition, "jev snapshot unavailable")})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/jev-arb/snapshot", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestJevDecisionsExposeNotApplicableAndPortfolioRejected(t *testing.T) {
	router := testRouterWithJev(t, &testJevServer{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/jev-arb/decisions?limit=50", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 3 ||
		payload.Data[0]["executionStatus"] != "not_applicable" ||
		payload.Data[1]["executionStatus"] != "portfolio_rejected" ||
		payload.Data[2]["executionStatus"] != "fill_failed" {
		t.Fatalf("%+v", payload.Data)
	}
	if payload.Data[0]["expectedNetReturn24h"] != nil || payload.Data[0]["basisSampleCount"] != nil ||
		payload.Data[0]["basisModelVersion"] != nil {
		t.Fatalf("historical metrics must be null: %+v", payload.Data[0])
	}
}

func TestJevLimitHardCap(t *testing.T) {
	router := testRouterWithJev(t, &testJevServer{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/jev-arb/decisions?limit=500", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestOpenAPIOmitsJevPaths(t *testing.T) {
	for _, path := range documentedOpenAPIPaths {
		if path == "/api/v1/jev-arb/snapshot" {
			t.Fatal("jev routes must stay out of public openapi")
		}
	}
}
