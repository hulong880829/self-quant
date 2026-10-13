package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"/api/v1/tradefi-perps-opportunities",
	"/api/v1/funding-opportunities",
	"/api/v1/funding-rates/{exchange}/{exchangeSymbol}/history",
	"/api/v1/basis-spreads/{venue}/{baseAsset}/{quoteAsset}/history",
	"/api/v1/oauth/token",
	"/api/v1/admin/overview",
	"/api/v1/admin/brokers",
	"/api/v1/admin/platform-accounts",
	"/api/v1/broker/summary",
	"/api/v1/broker/platform-accounts",
	"/api/v1/trading-accounts",
	"/api/v1/trading-accounts/{id}",
	"/api/v1/trading-accounts/{id}/fee-rates",
	"/api/v1/trading-accounts/{id}/fee-rates/sync",
	"/api/v1/trading-accounts/{id}/snapshot",
	"/api/v1/trading-accounts/{id}/trading-readiness",
	"/api/v1/trading-account-products/{productName}/snapshot",
	"/api/v1/trader/accounts/{accountId}/instruments",
	"/api/v1/trader/accounts/{accountId}/account-profile/apply",
	"/api/v1/trader/orders",
	"/api/v1/trader/orders/{id}",
	"/api/v1/trader/twaps",
	"/api/v1/trader/twaps/{id}",
	"/api/v1/trader/twaps/{id}/orders",
	"/api/v1/trader/arbitrage-combinations",
	"/api/v1/trader/arbitrage-combinations/preflight",
	"/api/v1/trader/arbitrage-combinations/{id}",
	"/api/v1/trader/arbitrage-combinations/{id}/orders",
	"/api/v1/trader/arbitrage-combinations/{id}/executions",
	"/api/v1/trader/arbitrage-combinations/{id}/events",
}

const documentedOpenAPIOperations = 44

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
		"persistAuthorization: false",
		"不要在公共电脑输入生产 Broker Secret",
		"<title>SelfQuant Broker 交易 API</title>",
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
	info, _ := spec["info"].(map[string]any)
	if info["version"] != "1.1.3" {
		t.Fatalf("info.version=%v", info["version"])
	}
	title, _ := info["title"].(string)
	if !strings.Contains(title, "Broker") || !containsHan(title) {
		t.Fatalf("info.title=%q", title)
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
		"/api/v1/reports", "/api/v1/ai", "/api/v1/polymarket", "/api/v1/jev-arb", "/health",
	}
	for path := range paths {
		for _, prefix := range forbidden {
			if strings.HasPrefix(path, prefix) || path == prefix {
				t.Fatalf("undocumented path leaked: %s", path)
			}
		}
	}
	operations := 0
	for _, raw := range paths {
		item, _ := raw.(map[string]any)
		for method := range item {
			switch method {
			case "get", "post", "put", "patch", "delete":
				operations++
			}
		}
	}
	if operations != documentedOpenAPIOperations {
		t.Fatalf("operation count=%d want=%d", operations, documentedOpenAPIOperations)
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
	for _, name := range []string{
		"Funding", "Spread", "OAuth", "Admin", "Broker",
		"Trading Accounts", "Instruments", "Orders", "TWAP", "Arbitrage",
	} {
		if !tagNames[name] {
			t.Fatalf("missing tag %s in %v", name, tagNames)
		}
	}
	if _, ok := spec["security"]; ok {
		t.Fatal("spec must not declare global security")
	}
	schemes, _ := spec["components"].(map[string]any)["securitySchemes"].(map[string]any)
	if schemes["BrokerClientBasic"] == nil || schemes["BrokerBearer"] == nil || schemes["SessionCookie"] == nil {
		t.Fatalf("securitySchemes=%v", keysOf(schemes))
	}
	sessionCookie, _ := schemes["SessionCookie"].(map[string]any)
	if sessionCookie["type"] != "apiKey" || sessionCookie["in"] != "cookie" || sessionCookie["name"] != "sq_session" {
		t.Fatalf("SessionCookie=%v", sessionCookie)
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
			summary, _ := op["summary"].(string)
			if !containsHan(summary) {
				t.Fatalf("%s %s summary must be Chinese", method, path)
			}
			assertOperationSecurity(t, path, method, op)
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
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	assertOpenAPIContractDetails(t, paths, schemas)
	assertAllOperationExamples(t, paths, schemas)
	rawSpec := recorder.Body.String()
	if strings.Contains(rawSpec, "当前没有非下单的组合级 preflight") {
		t.Fatal("arbitrage tag still claims that preflight is unavailable")
	}
	for _, name := range []string{
		"ValidateSession", "IssueBrokerDelegatedSessionInternal",
		"GetTradingCredentialsInternal", "INTERNAL_TOKEN",
	} {
		if strings.Contains(rawSpec, name) {
			t.Fatalf("internal RPC %s leaked into OpenAPI", name)
		}
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

func TestOpenAPITradefiSchemaMatchesHTTPOutput(t *testing.T) {
	router := testRouter(t, &tradefiOnlyFundingServer{}, nil)
	specRecorder := httptest.NewRecorder()
	router.ServeHTTP(
		specRecorder,
		httptest.NewRequest(http.MethodGet, "/swagger/openapi.json", nil),
	)
	if specRecorder.Code != http.StatusOK {
		t.Fatalf("spec status=%d body=%s", specRecorder.Code, specRecorder.Body.String())
	}
	var spec map[string]any
	if err := json.Unmarshal(specRecorder.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	paths := mapValue(t, spec, "paths")
	schemas := mapValue(t, mapValue(t, spec, "components"), "schemas")
	op := operation(t, paths, "/api/v1/tradefi-perps-opportunities", "get")

	responseRecorder := httptest.NewRecorder()
	router.ServeHTTP(
		responseRecorder,
		httptest.NewRequest(http.MethodGet, "/api/v1/tradefi-perps-opportunities", nil),
	)
	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("response status=%d body=%s", responseRecorder.Code, responseRecorder.Body.String())
	}
	var payload any
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	assertExampleAgainstSchema(
		t, schemas, payload, responseMediaSchema(t, op, "200"), "tradefi HTTP response",
	)
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

func assertOperationSecurity(t *testing.T, path, method string, op map[string]any) {
	t.Helper()
	security, _ := op["security"].([]any)
	switch {
	case strings.HasPrefix(path, "/api/v1/funding") || strings.HasPrefix(path, "/api/v1/basis-spreads"):
		if len(security) != 0 {
			t.Fatalf("%s %s must be public, security=%v", method, path, security)
		}
	case path == "/api/v1/oauth/token":
		assertNamedSecurity(t, method, path, security, "BrokerClientBasic")
	case strings.HasPrefix(path, "/api/v1/admin/"):
		assertNamedSecurity(t, method, path, security, "SessionCookie")
	case strings.HasPrefix(path, "/api/v1/broker/"):
		assertNamedSecurity(t, method, path, security, "BrokerBearer")
		if hasParameter(op, "X-SelfQuant-Account") {
			t.Fatalf("%s %s must not require X-SelfQuant-Account", method, path)
		}
	case strings.HasPrefix(path, "/api/v1/trading-accounts") ||
		strings.HasPrefix(path, "/api/v1/trading-account-products") ||
		strings.HasPrefix(path, "/api/v1/trader/"):
		assertNamedSecurity(t, method, path, security, "BrokerBearer")
		if !hasParameter(op, "X-SelfQuant-Account") {
			t.Fatalf("%s %s missing X-SelfQuant-Account", method, path)
		}
	}
	if (path == "/api/v1/trader/orders" && method == "post") ||
		(path == "/api/v1/trader/twaps" && method == "post") ||
		(path == "/api/v1/trader/arbitrage-combinations" && method == "post") {
		if !hasParameter(op, "Idempotency-Key") {
			t.Fatalf("%s %s missing Idempotency-Key", method, path)
		}
	}
}

func assertNamedSecurity(t *testing.T, method, path string, security []any, name string) {
	t.Helper()
	if len(security) != 1 {
		t.Fatalf("%s %s security=%v want %s", method, path, security, name)
	}
	entry, _ := security[0].(map[string]any)
	if _, ok := entry[name]; !ok {
		t.Fatalf("%s %s security=%v want %s", method, path, security, name)
	}
}

func hasParameter(op map[string]any, name string) bool {
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		param, _ := raw.(map[string]any)
		if param["name"] == name {
			return true
		}
		ref, _ := param["$ref"].(string)
		if strings.HasSuffix(ref, "/"+name) {
			return true
		}
		if name == "X-SelfQuant-Account" && strings.HasSuffix(ref, "/TargetPlatformAccount") {
			return true
		}
		if name == "Idempotency-Key" && strings.HasSuffix(ref, "/IdempotencyKey") {
			return true
		}
	}
	return false
}

func assertOpenAPIContractDetails(t *testing.T, paths, schemas map[string]any) {
	t.Helper()
	decimal, _ := schemas["DecimalString"].(map[string]any)
	if decimal["type"] != "string" {
		t.Fatalf("DecimalString=%v", decimal)
	}
	createAccount, _ := schemas["CreateTradingAccountRequest"].(map[string]any)
	if _, ok := createAccount["oneOf"]; ok {
		t.Fatal("CreateTradingAccountRequest must be flat, not oneOf")
	}
	props, _ := createAccount["properties"].(map[string]any)
	for _, name := range []string{
		"productName", "exchange", "accountName", "apiKey", "apiSecret", "passphrase",
		"privateKey", "walletType", "funderAddress", "tradingApiKey", "tradingApiSecret",
		"signingAddress", "vaultAddress", "accountIndex", "apiKeyIndex",
	} {
		if props[name] == nil {
			t.Fatalf("missing create field %s", name)
		}
	}
	for _, name := range []string{"apiSecret", "privateKey", "passphrase", "tradingApiSecret"} {
		field, _ := props[name].(map[string]any)
		if field["writeOnly"] != true {
			t.Fatalf("%s writeOnly=%v", name, field["writeOnly"])
		}
	}
	arbCreate, _ := schemas["CreateArbitrageCombinationRequest"].(map[string]any)
	arbProps, _ := arbCreate["properties"].(map[string]any)
	if _, ok := arbProps["productName"]; ok {
		t.Fatal("arbitrage create request must not advertise productName")
	}
	runMode, _ := arbProps["runMode"].(map[string]any)
	if runMode["enum"] == nil {
		t.Fatal("runMode must stay a closed input enum")
	}
	execMode, _ := arbProps["executionMode"].(map[string]any)
	if execMode["enum"] == nil {
		t.Fatal("executionMode must stay a closed input enum")
	}
	arbUpdate, _ := schemas["UpdateArbitrageCombinationRequest"].(map[string]any)
	arbUpdateProps, _ := arbUpdate["properties"].(map[string]any)
	for _, name := range []string{
		"exitAfterSeconds", "exitAnnualizedRate", "earlyExitFunding8hAnnualizedFloor",
		"earlyExitLiquidationDistancePct",
	} {
		if arbUpdateProps[name] == nil {
			t.Fatalf("missing update field %s", name)
		}
	}
	for schemaName, field := range map[string]string{
		"TradingAccount":       "tradingStatus",
		"TradingReadiness":     "tradingStatus",
		"Order":                "status",
		"TwapJob":              "status",
		"ArbitrageCombination": "status",
	} {
		assertOpenStatus(t, schemas, schemaName, field)
	}
	assertOpenStatus(t, schemas, "ArbitrageCombination", "runtimeState")
	assertOpenStatus(t, schemas, "ArbitrageCombination", "oneShotPhase")
	assertOpenStatus(t, schemas, "TradingAccount", "bindingStatus")
	assertOpenStatus(t, schemas, "TradingAccount", "feeSyncStatus")
	assertOpenStatus(t, schemas, "TradingAccount", "tradingUnavailableCode")
	assertOpenStatus(t, schemas, "FeeRates", "feeSyncStatus")
	for _, known := range []string{
		"monitoring", "ready", "partially_filled", "building_target", "manual_intervention",
	} {
		if !statusDescriptionContains(schemas, known) {
			t.Fatalf("known status %s missing from descriptions", known)
		}
	}
	forbiddenSecrets := map[string]struct{}{
		"apiSecret": {}, "privateKey": {}, "passphrase": {},
		"tradingApiSecret": {}, "client_secret": {},
	}
	for path, raw := range paths {
		item, _ := raw.(map[string]any)
		for method, opRaw := range item {
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}
			responses, _ := op["responses"].(map[string]any)
			for status, respRaw := range responses {
				resp, _ := respRaw.(map[string]any)
				content, _ := resp["content"].(map[string]any)
				app, _ := content["application/json"].(map[string]any)
				schema, _ := app["schema"].(map[string]any)
				for _, name := range responsePropertyNames(schemas, schema, nil) {
					if _, banned := forbiddenSecrets[name]; banned {
						t.Fatalf("%s %s %s response has secret field %s", method, path, status, name)
					}
				}
			}
		}
	}
	if _, ok := schemas["TradingAccount"].(map[string]any)["properties"].(map[string]any)["hasPassphrase"]; !ok {
		t.Fatal("hasPassphrase must remain on trading account responses")
	}
	assertP0Contract(t, paths, schemas)
	assertP1ReadContract(t, paths, schemas)
}

func assertP1ReadContract(t *testing.T, paths, schemas map[string]any) {
	t.Helper()
	preflight := operation(t, paths, "/api/v1/trader/arbitrage-combinations/preflight", "post")
	description, _ := preflight["description"].(string)
	for _, want := range []string{"只读", "不设置杠杆", "不锁资金", "不发订单", "不保证"} {
		if !strings.Contains(description, want) {
			t.Fatalf("preflight description missing %q", want)
		}
	}
	assertResponseExamples(t, schemas, preflight, "200")
	for _, suffix := range []string{"orders", "executions", "events"} {
		path := "/api/v1/trader/arbitrage-combinations/{id}/" + suffix
		op := operation(t, paths, path, "get")
		assertQueryIntegerBounds(t, op, "limit", 1, 100, 50)
		for _, name := range []string{"cursor", "from", "to", "executionId"} {
			if !hasParameter(op, name) {
				t.Fatalf("%s missing query parameter %s", path, name)
			}
		}
		text, _ := op["description"].(string)
		for _, want := range []string{"keyset", "不返回 total", "不访问交易所"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s description missing %q", path, want)
			}
		}
	}
	detail := operation(t, paths, "/api/v1/trader/arbitrage-combinations/{id}", "get")
	detailDescription, _ := detail["description"].(string)
	for _, want := range []string{
		"/orders", "/executions", "/events", "cursor", "from", "to", "executionId",
		"不支持按事件类型筛选", "逐笔成交", "实际手续费", "Maker/Taker",
		"实际资金费结算明细",
	} {
		if !strings.Contains(detailDescription, want) {
			t.Fatalf("arbitrage detail description missing %q", want)
		}
	}
	if strings.Contains(detailDescription, "当前没有按 combinationId") {
		t.Fatal("arbitrage detail still claims that complete history APIs are unavailable")
	}
	for _, schemaName := range []string{"ArbitrageExecution", "ArbitrageEvent"} {
		schema := mapValue(t, schemas, schemaName)
		schemaDescription, _ := schema["description"].(string)
		for _, stale := range []string{
			"近期记录", "近期事件", "不是完整历史", "不是完整审计历史",
		} {
			if strings.Contains(schemaDescription, stale) {
				t.Fatalf("%s description contains stale scope %q", schemaName, stale)
			}
		}
	}
	detailSchemaJSON, err := json.Marshal(mapValue(t, schemas, "ArbitrageCombinationDetail"))
	if err != nil {
		t.Fatal(err)
	}
	detailSchemaText := string(detailSchemaJSON)
	for _, want := range []string{"最近 10 条", "最近 20 条", "最近 50 条"} {
		if !strings.Contains(detailSchemaText, want) {
			t.Fatalf("arbitrage detail schema missing recent-window description %q", want)
		}
	}
	assertSchemaFields(t, schemas, "ArbitrageFinalMetrics",
		[]string{
			"status", "completedAt", "retryCount", "lastError", "realizedSpreadPnl",
			"estimatedFundingPnl", "estimatedTradingFee", "combinedPositionAnnualized",
			"metricsQuality", "holdingSeconds", "notionalExposureSeconds", "calculatedAt",
		},
		[]string{
			"status", "completedAt", "retryCount", "lastError", "realizedSpreadPnl",
			"estimatedFundingPnl", "estimatedTradingFee", "combinedPositionAnnualized",
			"metricsQuality", "holdingSeconds", "notionalExposureSeconds", "calculatedAt",
		})
	event := mapValue(t, schemas, "ArbitrageEvent")
	eventProps := mapValue(t, event, "properties")
	if eventProps["payload"] == nil || eventProps["executionId"] == nil {
		t.Fatalf("structured event fields=%v", keysOf(eventProps))
	}
}

func assertP0Contract(t *testing.T, paths, schemas map[string]any) {
	t.Helper()
	tradefi := operation(t, paths, "/api/v1/tradefi-perps-opportunities", "get")
	tradefiSchema := responseMediaSchema(t, tradefi, "200")
	if tradefiSchema["$ref"] != "#/components/schemas/TradefiPerpsOpportunityListResponse" {
		t.Fatalf("tradefi response schema=%v", tradefiSchema)
	}
	assertSchemaFields(t, schemas, "TradefiPerpsOpportunityListResponse",
		[]string{"data", "meta"}, []string{"data", "meta"})
	assertSchemaFields(t, schemas, "TradefiPerpsOpportunityListMeta",
		[]string{"total", "snapshotVersion", "serverTime"},
		[]string{"total", "snapshotVersion", "serverTime"})
	assertSchemaFields(t, schemas, "TradefiSpotLeg",
		[]string{"exchange", "symbol", "conId", "primaryExchange", "localSymbol", "tradingClass"},
		[]string{"exchange", "symbol", "conId", "primaryExchange", "localSymbol", "tradingClass"})
	assertSchemaFields(t, schemas, "TradefiPerpetualLeg",
		[]string{"exchange", "exchangeSymbol", "baseAsset", "quoteAsset"},
		[]string{"exchange", "exchangeSymbol", "baseAsset", "quoteAsset"})
	assertSchemaFields(t, schemas, "TradefiPerpsOpportunity",
		[]string{
			"id", "stockSymbol", "spot", "perpetual", "fundingRate",
			"perpetualFundingAnnualized", "nextFundingRate", "fundingIntervalSeconds",
			"nextFundingAt", "volume24hBase", "turnover24hUsd", "positionQuantity",
			"positionNotionalUsd", "cumulative24h", "cumulative7d", "sourceUpdatedAt",
			"stale", "verifiedAt", "priceMultiplier",
		},
		[]string{
			"id", "stockSymbol", "spot", "perpetual", "fundingRate",
			"perpetualFundingAnnualized", "nextFundingRate", "fundingIntervalSeconds",
			"nextFundingAt", "volume24hBase", "turnover24hUsd", "positionQuantity",
			"positionNotionalUsd", "cumulative24h", "cumulative7d", "sourceUpdatedAt",
			"stale", "verifiedAt", "priceMultiplier", "history24hComplete",
			"history7dComplete",
		})
	nextFunding := schemaProperty(t, schemas, "TradefiPerpsOpportunity", "nextFundingRate")
	if nextFunding["nullable"] != true {
		t.Fatalf("TradefiPerpsOpportunity.nextFundingRate=%v", nextFunding)
	}
	assertResponseExamples(t, schemas, tradefi, "200")
	tradefiDescription, _ := tradefi["description"].(string)
	for _, want := range []string{"市场数据参考", "不是收益预测", "可成交承诺"} {
		if !strings.Contains(tradefiDescription, want) {
			t.Fatalf("tradefi description missing %q", want)
		}
	}

	create := operation(t, paths, "/api/v1/trader/arbitrage-combinations", "post")
	createDescription, _ := create["description"].(string)
	for _, want := range []string{
		"Binance、OKX、Bybit、Bitget、Gate、Hyperliquid、Aster、Lighter",
		"Entropy 明确不在执行白名单", "仅代表上述八家当前发布范围",
		"客户端不得自行推导或承诺最终数量", "相同 key 且相同请求返回原结果",
		"相同 key 但不同请求返回冲突", "响应丢失后可用原 key 重试",
		"预检成功不承诺创建成功", "details.appliedLegs",
		"账户设置一定未改变",
	} {
		if !strings.Contains(createDescription, want) {
			t.Fatalf("create description missing %q", want)
		}
	}
	assertExampleAgainstSchema(
		t, schemas, requestMedia(t, create)["example"],
		mapValue(t, requestMedia(t, create), "schema"), "create request",
	)
	assertResponseExamples(t, schemas, create, "201")
	for _, schemaName := range []string{
		"CreateArbitrageCombinationRequest",
		"UpdateArbitrageCombinationRequest",
		"ArbitrageCombination",
	} {
		targetDescription, _ := schemaProperty(
			t, schemas, schemaName, "targetNotional",
		)["description"].(string)
		if !strings.Contains(targetDescription, "每条腿最终") ||
			!strings.Contains(targetDescription, "不是") {
			t.Fatalf("%s.targetNotional=%q", schemaName, targetDescription)
		}
	}
	exitDescription, _ := schemaProperty(
		t, schemas, "ArbitrageCombination", "exitAnnualizedRate",
	)["description"].(string)
	for _, want := range []string{
		"退出阈值", "不是承诺收益或最终结算值", "当前可执行 BBO",
		"已记录或推导的资金费", "账户费率", "没有独立的实际 slippage",
	} {
		if !strings.Contains(exitDescription, want) {
			t.Fatalf("exitAnnualizedRate description missing %q", want)
		}
	}
	combinedDescription, _ := schemaProperty(
		t, schemas, "ArbitrageCombination", "combinedPositionAnnualized",
	)["description"].(string)
	for _, want := range []string{"配对库存持有秒数", "平均配对名义", "不是简单 createdAt", "收益承诺"} {
		if !strings.Contains(combinedDescription, want) {
			t.Fatalf("combinedPositionAnnualized description missing %q", want)
		}
	}

	list := operation(t, paths, "/api/v1/trader/arbitrage-combinations", "get")
	if got := strings.Join(queryParamEnum(t, paths,
		"/api/v1/trader/arbitrage-combinations", "get", "view"), ","); got != "running,closed" {
		t.Fatalf("arbitrage view enum=%s", got)
	}
	if viewSchema := queryParamSchema(t, list, "view"); viewSchema["default"] != "running" {
		t.Fatalf("arbitrage view schema=%v", viewSchema)
	}
	assertQueryIntegerBounds(t, list, "limit", 1, 100, 50)
	cursorSchema := queryParamSchema(t, list, "cursor")
	if cursorSchema["format"] != "uuid" {
		t.Fatalf("arbitrage cursor schema=%v", cursorSchema)
	}
	listDescription, _ := list["description"].(string)
	for _, want := range []string{
		"上一页 nextCursor 返回的组合 UUID", "对应时间倒序",
		"空字符串表示没有下一页",
	} {
		if !strings.Contains(listDescription, want) {
			t.Fatalf("list description missing %q", want)
		}
	}

	opportunity := operation(t, paths, "/api/v1/funding-opportunities", "get")
	assertQueryIntegerBounds(t, opportunity, "limit", 1, 100, 100)
	opportunityDescription, _ := opportunity["description"].(string)
	for _, want := range []string{
		"综合 Score 降序", "没有 cursor", "前 limit 条",
		"不能把返回集合当成全市场全集", "市场参考", "不是收益预测", "可成交承诺",
	} {
		if !strings.Contains(opportunityDescription, want) {
			t.Fatalf("opportunity description missing %q", want)
		}
	}

	patch := operation(t, paths, "/api/v1/trader/arbitrage-combinations/{id}", "patch")
	if hasParameter(patch, "Idempotency-Key") {
		t.Fatal("arbitrage PATCH must not advertise Idempotency-Key")
	}
	patchDescription, _ := patch["description"].(string)
	for _, want := range []string{
		"earlyExitLiquidationDistancePct", "createdAt + exitAfterSeconds",
		"原始 createdAt", "响应丢失后必须先 GET",
	} {
		if !strings.Contains(patchDescription, want) {
			t.Fatalf("patch description missing %q", want)
		}
	}

	closeOp := operation(t, paths, "/api/v1/trader/arbitrage-combinations/{id}", "delete")
	closeResponses, _ := closeOp["responses"].(map[string]any)
	if closeResponses["202"] == nil || closeResponses["200"] != nil {
		t.Fatalf("close responses=%v", keysOf(closeResponses))
	}
	closeDescription, _ := closeOp["description"].(string)
	for _, want := range []string{
		"成功返回 202", "只表示关闭请求已接受", "随后必须轮询组合详情",
		"严格小于 20 USD", "等于或超过 20 USD 不是 dust",
		"closed_with_dust", "one_shot_exited_with_dust",
	} {
		if !strings.Contains(closeDescription, want) {
			t.Fatalf("close description missing %q", want)
		}
	}
	assertResponseExamples(t, schemas, closeOp, "202")

	detail := operation(t, paths, "/api/v1/trader/arbitrage-combinations/{id}", "get")
	detailDescription, _ := detail["description"].(string)
	for _, want := range []string{
		"最近 10 条", "最近 20 条", "最近 50 条", "recentOrders",
		"腿 A 买入、腿 B 卖出", "腿 A 卖出、腿 B 买入",
	} {
		if !strings.Contains(detailDescription, want) {
			t.Fatalf("detail description missing %q", want)
		}
	}
	assertResponseExamples(t, schemas, detail, "200")
	assertArbitrageExampleSemantics(t, detail)

	blob, _ := json.Marshal(map[string]any{"paths": paths, "schemas": schemas})
	for _, forbidden := range []string{"尚未统一", "进入 waiting_exit 附近的时间", "targetReachedAt + exitAfterSeconds"} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("stale waiting_exit timing text remains: %q", forbidden)
		}
	}
}

func responseMediaSchema(t *testing.T, op map[string]any, status string) map[string]any {
	t.Helper()
	responses := mapValue(t, op, "responses")
	response := mapValue(t, responses, status)
	content := mapValue(t, response, "content")
	return mapValue(t, mapValue(t, content, "application/json"), "schema")
}

func requestMedia(t *testing.T, op map[string]any) map[string]any {
	t.Helper()
	body := mapValue(t, op, "requestBody")
	return mapValue(t, mapValue(t, body, "content"), "application/json")
}

func mapValue(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	result, _ := value[key].(map[string]any)
	if result == nil {
		t.Fatalf("%s is not an object in %v", key, value)
	}
	return result
}

func schemaProperty(
	t *testing.T, schemas map[string]any, schemaName, propertyName string,
) map[string]any {
	t.Helper()
	schema := mapValue(t, schemas, schemaName)
	return mapValue(t, mapValue(t, schema, "properties"), propertyName)
}

func assertSchemaFields(
	t *testing.T,
	schemas map[string]any,
	name string,
	required, properties []string,
) {
	t.Helper()
	schema := mapValue(t, schemas, name)
	if schema["additionalProperties"] != false {
		t.Fatalf("%s must set additionalProperties=false", name)
	}
	assertStringSet(t, name+".required", stringValues(schema["required"]), required)
	props := mapValue(t, schema, "properties")
	assertStringSet(t, name+".properties", keysOf(props), properties)
}

func assertStringSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	gotSet := make(map[string]bool, len(got))
	for _, value := range got {
		gotSet[value] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, value := range want {
		wantSet[value] = true
	}
	if len(gotSet) != len(wantSet) {
		t.Fatalf("%s=%v want=%v", name, got, want)
	}
	for value := range wantSet {
		if !gotSet[value] {
			t.Fatalf("%s missing %s: got=%v", name, value, got)
		}
	}
}

func stringValues(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		result = append(result, text)
	}
	return result
}

func assertQueryIntegerBounds(
	t *testing.T, op map[string]any, name string, minimum, maximum, defaultValue float64,
) {
	t.Helper()
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		param, _ := raw.(map[string]any)
		if param["name"] != name {
			continue
		}
		schema := mapValue(t, param, "schema")
		if schema["minimum"] != minimum || schema["maximum"] != maximum ||
			schema["default"] != defaultValue {
			t.Fatalf("%s schema=%v", name, schema)
		}
		return
	}
	t.Fatalf("missing query parameter %s", name)
}

func queryParamSchema(t *testing.T, op map[string]any, name string) map[string]any {
	t.Helper()
	params, _ := op["parameters"].([]any)
	for _, raw := range params {
		param, _ := raw.(map[string]any)
		if param["name"] == name {
			return mapValue(t, param, "schema")
		}
	}
	t.Fatalf("missing query parameter %s", name)
	return nil
}

func assertResponseExamples(
	t *testing.T, schemas map[string]any, op map[string]any, status string,
) {
	t.Helper()
	responses := mapValue(t, op, "responses")
	response := mapValue(t, responses, status)
	media := mapValue(t, mapValue(t, response, "content"), "application/json")
	schema := mapValue(t, media, "schema")
	if example, ok := media["example"]; ok {
		assertExampleAgainstSchema(t, schemas, example, schema, status+" example")
	}
	examples, _ := media["examples"].(map[string]any)
	for name := range examples {
		entry := mapValue(t, examples, name)
		assertExampleAgainstSchema(t, schemas, entry["value"], schema, status+" "+name)
	}
}

func assertAllOperationExamples(
	t *testing.T, paths, schemas map[string]any,
) {
	t.Helper()
	for path, rawItem := range paths {
		item, _ := rawItem.(map[string]any)
		for method, rawOp := range item {
			op, ok := rawOp.(map[string]any)
			if !ok || method == "parameters" || method == "summary" || method == "description" {
				continue
			}
			if body, _ := op["requestBody"].(map[string]any); body != nil {
				assertContentExamples(t, schemas, body["content"], method+" "+path+" request")
			}
			responses, _ := op["responses"].(map[string]any)
			for status, rawResponse := range responses {
				response, _ := rawResponse.(map[string]any)
				if response != nil {
					assertContentExamples(
						t, schemas, response["content"],
						method+" "+path+" response "+status,
					)
				}
			}
		}
	}
}

func assertContentExamples(
	t *testing.T, schemas map[string]any, rawContent any, location string,
) {
	t.Helper()
	content, _ := rawContent.(map[string]any)
	for mediaType, rawMedia := range content {
		media, _ := rawMedia.(map[string]any)
		if media == nil {
			continue
		}
		schema, _ := media["schema"].(map[string]any)
		if example, ok := media["example"]; ok {
			if schema == nil {
				t.Fatalf("%s %s example has no schema", location, mediaType)
			}
			assertExampleAgainstSchema(t, schemas, example, schema, location+" example")
		}
		examples, _ := media["examples"].(map[string]any)
		for name, rawEntry := range examples {
			entry, _ := rawEntry.(map[string]any)
			if schema == nil {
				t.Fatalf("%s %s example %s has no schema", location, mediaType, name)
			}
			assertExampleAgainstSchema(
				t, schemas, entry["value"], schema, location+" example "+name,
			)
		}
	}
}

func assertExampleAgainstSchema(
	t *testing.T,
	schemas map[string]any,
	example any,
	schema map[string]any,
	path string,
) {
	t.Helper()
	if example == nil && schema["nullable"] == true {
		return
	}
	if ref, _ := schema["$ref"].(string); ref != "" {
		name := ref[strings.LastIndex(ref, "/")+1:]
		assertExampleAgainstSchema(t, schemas, example, mapValue(t, schemas, name), path)
		return
	}
	for _, raw := range anyValues(schema["allOf"]) {
		assertExampleAgainstSchema(t, schemas, example, raw, path)
	}
	object, _ := example.(map[string]any)
	for _, name := range stringValues(schema["required"]) {
		if object == nil {
			t.Fatalf("%s must be an object", path)
		}
		if _, ok := object[name]; !ok {
			t.Fatalf("%s missing required field %s", path, name)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if object != nil && schema["additionalProperties"] == false {
		for name := range object {
			if properties[name] == nil {
				t.Fatalf("%s has undocumented field %s", path, name)
			}
		}
	}
	for name, raw := range properties {
		if object == nil {
			break
		}
		child, ok := object[name]
		if !ok {
			continue
		}
		childSchema, _ := raw.(map[string]any)
		assertExampleAgainstSchema(t, schemas, child, childSchema, path+"."+name)
	}
	if items, _ := schema["items"].(map[string]any); items != nil {
		list, _ := example.([]any)
		for index, item := range list {
			assertExampleAgainstSchema(t, schemas, item, items, path+"["+strconv.Itoa(index)+"]")
		}
	}
}

func anyValues(value any) []map[string]any {
	raw, _ := value.([]any)
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		object, _ := item.(map[string]any)
		result = append(result, object)
	}
	return result
}

func assertArbitrageExampleSemantics(t *testing.T, detail map[string]any) {
	t.Helper()
	responses := mapValue(t, detail, "responses")
	response := mapValue(t, responses, "200")
	media := mapValue(t, mapValue(t, response, "content"), "application/json")
	examples := mapValue(t, media, "examples")
	for name, raw := range examples {
		entry, _ := raw.(map[string]any)
		value, _ := entry["value"].(map[string]any)
		data, _ := value["data"].(map[string]any)
		created, err := time.Parse(time.RFC3339Nano, data["createdAt"].(string))
		if err != nil {
			t.Fatalf("%s createdAt: %v", name, err)
		}
		scheduled, err := time.Parse(time.RFC3339Nano, data["scheduledExitAt"].(string))
		if err != nil {
			t.Fatalf("%s scheduledExitAt: %v", name, err)
		}
		seconds := time.Duration(data["exitAfterSeconds"].(float64)) * time.Second
		if !scheduled.Equal(created.Add(seconds)) {
			t.Fatalf("%s scheduledExitAt=%s want=%s", name, scheduled, created.Add(seconds))
		}
	}
	deleteDust := mapValue(t, mapValue(t, examples, "dust"), "value")
	deleteDustData := mapValue(t, deleteDust, "data")
	if deleteDustData["status"] != "closed" || deleteDustData["oneShotPhase"] != "exited" ||
		deleteDustData["closedAt"] == "" {
		t.Fatalf("DELETE dust example=%v", deleteDustData)
	}
	oneShotDust := mapValue(t, mapValue(t, examples, "oneShotDust"), "value")
	oneShotDustData := mapValue(t, oneShotDust, "data")
	if oneShotDustData["status"] != "running" || oneShotDustData["oneShotPhase"] != "exited" ||
		oneShotDustData["closedAt"] != "" {
		t.Fatalf("one-shot dust example=%v", oneShotDustData)
	}
	assertExecutionExample(t, examples, "running", "ask", "open", false)
	assertExecutionExample(t, examples, "closing", "bid", "close", true)
	assertLegOrderExamples(t, examples, "running", "buy", "sell")
	assertLegOrderExamples(t, examples, "closing", "sell", "buy")
}

func assertExecutionExample(
	t *testing.T,
	examples map[string]any,
	name, direction, effect string,
	reduceOnly bool,
) {
	t.Helper()
	value := mapValue(t, mapValue(t, examples, name), "value")
	data := mapValue(t, value, "data")
	executions, _ := data["recentExecutions"].([]any)
	execution, _ := executions[0].(map[string]any)
	if execution["direction"] != direction || execution["positionEffect"] != effect ||
		execution["reduceOnly"] != reduceOnly {
		t.Fatalf("%s execution=%v", name, execution)
	}
}

func assertLegOrderExamples(
	t *testing.T,
	examples map[string]any,
	name, legASide, legBSide string,
) {
	t.Helper()
	value := mapValue(t, mapValue(t, examples, name), "value")
	data := mapValue(t, value, "data")
	orders, _ := data["orders"].([]any)
	sides := map[string]string{}
	for _, raw := range orders {
		order, _ := raw.(map[string]any)
		leg, _ := order["arbitrageLeg"].(string)
		side, _ := order["side"].(string)
		sides[leg] = side
	}
	if sides["a"] != legASide || sides["b"] != legBSide {
		t.Fatalf("%s leg order sides=%v", name, sides)
	}
}

func assertOpenStatus(t *testing.T, schemas map[string]any, schemaName, field string) {
	t.Helper()
	schema, _ := schemas[schemaName].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	item, _ := props[field].(map[string]any)
	if item["type"] != "string" {
		t.Fatalf("%s.%s type=%v", schemaName, field, item["type"])
	}
	if _, ok := item["enum"]; ok {
		t.Fatalf("%s.%s must not use a closed enum", schemaName, field)
	}
	desc, _ := item["description"].(string)
	if !strings.Contains(desc, "必须容忍未知") {
		t.Fatalf("%s.%s description must tell clients to tolerate unknown values: %s", schemaName, field, desc)
	}
}

func statusDescriptionContains(schemas map[string]any, known string) bool {
	blob, _ := json.Marshal(schemas)
	return strings.Contains(string(blob), known)
}

func responsePropertyNames(schemas, schema map[string]any, seen map[string]bool) []string {
	if schema == nil {
		return nil
	}
	if seen == nil {
		seen = map[string]bool{}
	}
	if ref, _ := schema["$ref"].(string); ref != "" {
		name := ref[strings.LastIndex(ref, "/")+1:]
		if seen[name] {
			return nil
		}
		seen[name] = true
		resolved, _ := schemas[name].(map[string]any)
		return responsePropertyNames(schemas, resolved, seen)
	}
	var names []string
	if props, _ := schema["properties"].(map[string]any); props != nil {
		for name, raw := range props {
			names = append(names, name)
			nested, _ := raw.(map[string]any)
			names = append(names, responsePropertyNames(schemas, nested, seen)...)
		}
	}
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		list, _ := schema[key].([]any)
		for _, raw := range list {
			nested, _ := raw.(map[string]any)
			names = append(names, responsePropertyNames(schemas, nested, seen)...)
		}
	}
	if items, _ := schema["items"].(map[string]any); items != nil {
		names = append(names, responsePropertyNames(schemas, items, seen)...)
	}
	return names
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
