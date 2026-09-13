package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
	accountv1 "selfquant/backend/gen/account/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
	spreadv1 "selfquant/backend/gen/spread/v1"
)

var documentedOpenAPIPaths = []string{
	"/api/v1/funding-rates",
	"/api/v1/funding-rates/lookup",
	"/api/v1/funding-spreads",
	"/api/v1/funding-opportunities",
	"/api/v1/funding-rates/{exchange}/{exchangeSymbol}/history",
	"/api/v1/basis-spreads/{venue}/{baseAsset}/{quoteAsset}/history",
}

func TestSwaggerRedirectsIndex(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/swagger", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	location := recorder.Header().Get("Location")
	if location != "/swagger/" {
		t.Fatalf("location=%q", location)
	}
}

func TestSwaggerUIHTML(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/swagger/", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	contentType := recorder.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("content-type=%q", contentType)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`url: "/swagger/openapi.json"`,
		"deepLinking: true",
		"displayRequestDuration: true",
		"swagger-ui-dist@5.17.14",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("html missing %q", want)
		}
	}
	if strings.Contains(body, "withCredentials") {
		t.Fatal("swagger UI must not set withCredentials")
	}
}

func TestOpenAPISpecContract(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/swagger/openapi.json", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	contentType := recorder.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content-type=%q", contentType)
	}
	var spec map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	openapi, _ := spec["openapi"].(string)
	if !strings.HasPrefix(openapi, "3.0.") {
		t.Fatalf("openapi=%q", openapi)
	}
	paths, _ := spec["paths"].(map[string]any)
	if len(paths) != len(documentedOpenAPIPaths) {
		t.Fatalf("path count=%d keys=%v", len(paths), keysOf(paths))
	}
	for _, path := range documentedOpenAPIPaths {
		if _, ok := paths[path]; !ok {
			t.Fatalf("missing path %s", path)
		}
	}
	forbidden := []string{
		"/api/v1/trader", "/api/v1/trading-accounts", "/api/v1/reports",
		"/api/v1/ai", "/api/v1/polymarket", "/health",
	}
	for path := range paths {
		for _, prefix := range forbidden {
			if strings.HasPrefix(path, prefix) || path == prefix {
				t.Fatalf("undocumented path leaked: %s", path)
			}
		}
	}
	if !containsHan(recorder.Body.String()) {
		t.Fatal("spec must include Chinese descriptions")
	}
	tags, _ := spec["tags"].([]any)
	tagNames := map[string]bool{}
	for _, raw := range tags {
		tag, _ := raw.(map[string]any)
		name, _ := tag["name"].(string)
		tagNames[name] = true
		description, _ := tag["description"].(string)
		if !containsHan(description) {
			t.Fatalf("tag %s description must be Chinese", name)
		}
	}
	if !tagNames["Funding"] || !tagNames["Spread"] || len(tagNames) != 2 {
		t.Fatalf("tags=%v", tagNames)
	}
	if _, ok := spec["security"]; ok {
		t.Fatal("spec must not declare global security")
	}
	for path, raw := range paths {
		item, _ := raw.(map[string]any)
		if security, ok := item["security"]; ok && !isEmptySecurity(security) {
			t.Fatalf("path %s has security", path)
		}
		for method, opRaw := range item {
			if method == "parameters" || method == "summary" || method == "description" {
				continue
			}
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}
			if security, exists := op["security"]; exists && !isEmptySecurity(security) {
				t.Fatalf("%s %s has security", method, path)
			}
			summary, _ := op["summary"].(string)
			if !containsHan(summary) {
				t.Fatalf("%s %s summary must be Chinese", method, path)
			}
		}
	}
	period := queryParamEnum(t, paths, "/api/v1/funding-opportunities", "get", "period")
	if got := strings.Join(period, ","); got != "8h,24h" {
		t.Fatalf("period enum=%v", period)
	}
	rangeEnum := queryParamEnum(t, paths, "/api/v1/basis-spreads/{venue}/{baseAsset}/{quoteAsset}/history", "get", "range")
	if got := strings.Join(rangeEnum, ","); got != "1h,4h,8h,24h,7d" {
		t.Fatalf("range enum=%v", rangeEnum)
	}
	lookup := operation(t, paths, "/api/v1/funding-rates/lookup", "post")
	if _, ok := lookup["responses"].(map[string]any)["401"]; ok {
		t.Fatal("lookup must not document 401")
	}
	description, _ := lookup["description"].(string)
	if !strings.Contains(description, "无需登录") {
		t.Fatalf("lookup must say it is public: %s", description)
	}
	if _, ok := spec["components"].(map[string]any)["securitySchemes"]; ok {
		t.Fatal("must not register security schemes")
	}
}

func TestSwaggerRoutesDoNotCallBackends(t *testing.T) {
	funding := &testFundingServer{}
	spread := &testSpreadServer{}
	router := documentedAPIRouter(t, funding, spread)
	for _, path := range []string{"/swagger", "/swagger/", "/swagger/openapi.json"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusFound {
			t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	if n := fundingRPCCount(funding); n != 0 {
		t.Fatalf("funding rpc calls=%d", n)
	}
	if spread.historyCalls != 0 {
		t.Fatalf("spread rpc calls=%d", spread.historyCalls)
	}
}

func documentedAPIRouter(
	t *testing.T,
	fundingServer fundingv1.FundingServiceServer,
	spreadServer spreadv1.SpreadServiceServer,
) http.Handler {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fundingv1.RegisterFundingServiceServer(server, fundingServer)
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

func fundingRPCCount(server *testFundingServer) int {
	return server.batchGetCalls + server.listRatesCalls + server.listSpreadsCalls +
		server.listOpportunitiesCalls + server.historyCalls
}

func operation(t *testing.T, paths map[string]any, path, method string) map[string]any {
	t.Helper()
	item, _ := paths[path].(map[string]any)
	op, _ := item[method].(map[string]any)
	if op == nil {
		t.Fatalf("missing %s %s", method, path)
	}
	return op
}

func queryParamEnum(t *testing.T, paths map[string]any, path, method, name string) []string {
	t.Helper()
	op := operation(t, paths, path, method)
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		param, _ := raw.(map[string]any)
		if param["name"] != name {
			continue
		}
		schema, _ := param["schema"].(map[string]any)
		values, _ := schema["enum"].([]any)
		out := make([]string, 0, len(values))
		for _, value := range values {
			text, _ := value.(string)
			out = append(out, text)
		}
		return out
	}
	t.Fatalf("missing query param %s on %s", name, path)
	return nil
}

func isEmptySecurity(value any) bool {
	list, ok := value.([]any)
	return ok && len(list) == 0
}

func keysOf(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func containsHan(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
