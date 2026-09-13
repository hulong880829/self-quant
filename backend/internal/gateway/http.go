package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	accountv1 "selfquant/backend/gen/account/v1"
	aiv1 "selfquant/backend/gen/ai/v1"
	fundingv1 "selfquant/backend/gen/funding/v1"
	polymarketv1 "selfquant/backend/gen/polymarket/v1"
	reportv1 "selfquant/backend/gen/report/v1"
	spreadv1 "selfquant/backend/gen/spread/v1"
	traderv1 "selfquant/backend/gen/trader/v1"
)

type Options struct {
	SessionCookieName   string
	SessionCookieSecure bool
	SessionCookieMaxAge time.Duration
	Polymarket          polymarketv1.PolymarketServiceClient
	Report              reportv1.ReportServiceClient
	Trader              traderv1.TraderServiceClient
	AI                  aiv1.AIServiceClient
	Spread              spreadv1.SpreadServiceClient
	AccountHealth       grpc_health_v1.HealthClient
	PolymarketHealth    grpc_health_v1.HealthClient
	ReportHealth        grpc_health_v1.HealthClient
	TraderHealth        grpc_health_v1.HealthClient
	AIHealth            grpc_health_v1.HealthClient
	SpreadHealth        grpc_health_v1.HealthClient
}

type Handler struct {
	funding             fundingv1.FundingServiceClient
	account             accountv1.AccountServiceClient
	polymarket          polymarketv1.PolymarketServiceClient
	report              reportv1.ReportServiceClient
	trader              traderv1.TraderServiceClient
	ai                  aiv1.AIServiceClient
	spread              spreadv1.SpreadServiceClient
	health              grpc_health_v1.HealthClient
	accountHealth       grpc_health_v1.HealthClient
	polymarketHealth    grpc_health_v1.HealthClient
	reportHealth        grpc_health_v1.HealthClient
	traderHealth        grpc_health_v1.HealthClient
	aiHealth            grpc_health_v1.HealthClient
	spreadHealth        grpc_health_v1.HealthClient
	sessionCookieName   string
	sessionCookieSecure bool
	sessionCookieMaxAge time.Duration
}

func NewRouter(
	funding fundingv1.FundingServiceClient,
	account accountv1.AccountServiceClient,
	health grpc_health_v1.HealthClient,
	options Options,
) http.Handler {
	cookieName := strings.TrimSpace(options.SessionCookieName)
	if cookieName == "" {
		cookieName = "sq_session"
	}
	maxAge := options.SessionCookieMaxAge
	if maxAge <= 0 {
		maxAge = 12 * time.Hour
	}
	handler := &Handler{
		funding:             funding,
		account:             account,
		health:              health,
		polymarket:          options.Polymarket,
		report:              options.Report,
		trader:              options.Trader,
		ai:                  options.AI,
		spread:              options.Spread,
		accountHealth:       options.AccountHealth,
		polymarketHealth:    options.PolymarketHealth,
		reportHealth:        options.ReportHealth,
		traderHealth:        options.TraderHealth,
		aiHealth:            options.AIHealth,
		spreadHealth:        options.SpreadHealth,
		sessionCookieName:   cookieName,
		sessionCookieSecure: options.SessionCookieSecure,
		sessionCookieMaxAge: maxAge,
	}
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Compress(5))
	router.Use(corsMiddleware(os.Getenv("GATEWAY_CORS_ORIGIN")))
	router.Get("/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Get("/health/ready", handler.healthCheck)
	router.Post("/api/v1/auth/login", handler.login)
	router.Get("/api/v1/auth/session", handler.session)
	router.Post("/api/v1/auth/logout", handler.logout)
	router.Get("/api/v1/trading-accounts", handler.listTradingAccounts)
	router.Post("/api/v1/trading-accounts", handler.createTradingAccount)
	router.Get("/api/v1/trading-accounts/{id}/fee-rates", handler.getTradingAccountFeeRates)
	router.Post("/api/v1/trading-accounts/{id}/fee-rates/sync", handler.syncTradingAccountFeeRates)
	router.Get("/api/v1/trading-accounts/{id}/snapshot", handler.getTradingAccountSnapshot)
	router.Get("/api/v1/trading-account-products/{productName}/snapshot", handler.getProductGroupSnapshot)
	router.Delete("/api/v1/trading-accounts/{id}", handler.deleteTradingAccount)
	router.Post("/api/v1/trading-accounts/{id}/trading-readiness", handler.inspectTradingReadiness)
	router.Get("/api/v1/funding-rates", handler.listFundingRates)
	router.Post("/api/v1/funding-rates/lookup", handler.lookupFundingRates)
	router.Get("/api/v1/funding-spreads", handler.listFundingSpreads)
	router.Get("/api/v1/funding-opportunities", handler.listFundingOpportunities)
	router.Get(
		"/api/v1/funding-rates/{exchange}/{exchangeSymbol}/history",
		handler.getFundingHistory,
	)
	router.Get("/swagger", redirectSwaggerIndex)
	router.Get("/swagger/", serveSwaggerUI)
	router.Get("/swagger/openapi.json", serveOpenAPISpec)
	if handler.spread != nil {
		router.Get(
			"/api/v1/basis-spreads/{venue}/{baseAsset}/{quoteAsset}/history",
			handler.getBasisSpreadHistory,
		)
	}
	if handler.polymarket != nil {
		router.Get("/api/v1/polymarket/markets", handler.listPolymarketMarkets)
		router.Get("/api/v1/polymarket/markets/{id}", handler.getPolymarketSnapshot)
		router.Get("/api/v1/polymarket/stream", handler.streamPolymarketSnapshots)
		router.Get("/api/v1/polymarket/accounts/{accountId}/summary", handler.getPolymarketAccountSummary)
		router.Get("/api/v1/polymarket/accounts/{accountId}/positions", handler.listPolymarketPositions)
		router.Get("/api/v1/polymarket/accounts/{accountId}/open-orders", handler.listPolymarketOpenOrders)
		router.Get("/api/v1/polymarket/accounts/{accountId}/events", handler.streamPolymarketAccountEvents)
		router.Delete("/api/v1/polymarket/accounts/{accountId}/orders/{id}", handler.cancelPolymarketOrder)
		router.Post("/api/v1/polymarket/orders", handler.placePolymarketOrder)
		router.Get("/api/v1/polymarket/orders/{id}", handler.getPolymarketOrder)
	}
	if handler.trader != nil {
		router.Get("/api/v1/trader/accounts/{accountId}/instruments", handler.listTraderInstruments)
		router.Post("/api/v1/trader/accounts/{accountId}/account-profile/apply", handler.applyTraderAccountProfile)
		router.Post("/api/v1/trader/orders", handler.placeTraderOrder)
		router.Get("/api/v1/trader/orders", handler.listTraderOrders)
		router.Get("/api/v1/trader/orders/{id}", handler.getTraderOrder)
		router.Delete("/api/v1/trader/orders/{id}", handler.cancelTraderOrder)
		router.Post("/api/v1/trader/twaps", handler.createTraderTwap)
		router.Get("/api/v1/trader/twaps", handler.listTraderTwaps)
		router.Get("/api/v1/trader/twaps/{id}", handler.getTraderTwap)
		router.Get("/api/v1/trader/twaps/{id}/orders", handler.listTraderTwapOrders)
		router.Delete("/api/v1/trader/twaps/{id}", handler.cancelTraderTwap)
		router.Post("/api/v1/trader/arbitrage-combinations", handler.createArbitrageCombination)
		router.Get("/api/v1/trader/arbitrage-combinations", handler.listArbitrageCombinations)
		router.Get("/api/v1/trader/arbitrage-combinations/{id}", handler.getArbitrageCombination)
		router.Patch("/api/v1/trader/arbitrage-combinations/{id}", handler.updateArbitrageCombination)
		router.Delete("/api/v1/trader/arbitrage-combinations/{id}", handler.closeArbitrageCombination)
	}
	if handler.report != nil {
		router.Get("/api/v1/reports/products", handler.listReportProducts)
		router.Get("/api/v1/reports/products/{id}", handler.getReportProduct)
		router.Get("/api/v1/reports/products/{id}/daily", handler.listReportDaily)
		router.Get("/api/v1/reports/products/{id}/cash-flows", handler.listReportCashFlows)
		router.Post("/api/v1/reports/products/{id}/cash-flows", handler.createReportCashFlow)
		router.Post("/api/v1/reports/products/{id}/recompute", handler.recomputeReportProduct)
	}
	if handler.ai != nil {
		router.Get("/api/v1/ai/credentials/openrouter", handler.getAICredential)
		router.Post("/api/v1/ai/credentials/openrouter", handler.upsertAICredential)
		router.Delete("/api/v1/ai/credentials/openrouter", handler.deleteAICredential)
		router.Post("/api/v1/ai/credentials/openrouter/test", handler.testAICredential)
		router.Get("/api/v1/ai/conversations", handler.listAIConversations)
		router.Post("/api/v1/ai/conversations", handler.createAIConversation)
		router.Post("/api/v1/ai/conversations/{id}/rename", handler.renameAIConversation)
		router.Delete("/api/v1/ai/conversations/{id}", handler.deleteAIConversation)
		router.Get("/api/v1/ai/conversations/{id}/messages", handler.listAIConversationMessages)
		router.Post("/api/v1/ai/chat", handler.streamAIChat)
	}
	return router
}

func (h *Handler) getTradingAccountSnapshot(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(request, "id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid trading account id"})
		return
	}
	cacheOnly := strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("cacheOnly")), "true")
	timeout := 12 * time.Second
	if cacheOnly {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	response, err := h.account.GetTradingAccountSnapshot(ctx, &accountv1.GetTradingAccountSnapshotRequest{
		Token: token, TradingAccountId: id, CacheOnly: cacheOnly,
	})
	if err != nil {
		if cacheOnly && status.Code(err) == codes.FailedPrecondition &&
			status.Convert(err).Message() == "snapshot_cache_miss" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		h.writeAccountError(writer, err, "account snapshot unavailable")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, tradingAccountSnapshotJSON(response))
}

func (h *Handler) getProductGroupSnapshot(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	product := strings.TrimSpace(chi.URLParam(request, "productName"))
	if product == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "product name is required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	response, err := h.account.GetProductGroupSnapshot(ctx, &accountv1.GetProductGroupSnapshotRequest{Token: token, ProductName: product})
	if err != nil {
		h.writeAccountError(writer, err, "product snapshot unavailable")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	snapshot := response.GetSnapshot()
	positions := make([]map[string]any, 0, len(snapshot.GetPositions()))
	for _, item := range snapshot.GetPositions() {
		positions = append(positions, map[string]any{
			"symbol": item.GetSymbol(), "side": item.GetSide(),
			"totalNotionalUsd": item.GetTotalNotionalUsd(),
			"spotSize":         item.GetSpotSize(), "contractSize": item.GetContractSize(),
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"productName": snapshot.GetProductName(), "accountCount": snapshot.GetAccountCount(),
		"accountEquityUsd": snapshot.GetAccountEquityUsd(), "availableFundsUsd": snapshot.GetAvailableFundsUsd(),
		"positions": positions, "sourceUpdatedAt": protoTimeJSON(snapshot.GetSourceUpdatedAt()),
		"serverTime": protoTimeJSON(response.GetServerTime()), "stale": snapshot.GetStale(),
		"partial": snapshot.GetPartial(), "errors": snapshot.GetErrors(),
	})
}

func tradingAccountSnapshotJSON(response *accountv1.GetTradingAccountSnapshotResponse) map[string]any {
	snapshot := response.GetSnapshot()
	positions := make([]map[string]any, 0, len(snapshot.GetPositions()))
	for _, item := range snapshot.GetPositions() {
		positions = append(positions, map[string]any{
			"key": item.GetKey(), "kind": item.GetKind(), "exchange": item.GetExchange(),
			"symbol": item.GetSymbol(), "side": item.GetSide(), "notionalUsd": item.GetNotionalUsd(),
			"size": item.GetSize(), "spotSize": item.GetSpotSize(),
			"signedContractSize": item.GetSignedContractSize(),
			"entryPrice":         item.GetEntryPrice(), "markPrice": item.GetMarkPrice(),
			"unrealizedPnl": item.GetUnrealizedPnl(), "marketTitle": item.GetMarketTitle(),
			"outcome": item.GetOutcome(), "initialValue": item.GetInitialValue(),
			"currentValue": item.GetCurrentValue(), "cashPnl": item.GetCashPnl(),
			"conditionId": item.GetConditionId(), "tokenId": item.GetTokenId(), "endTime": protoTimeJSON(item.GetEndTime()),
		})
	}
	return map[string]any{
		"tradingAccountId": snapshot.GetTradingAccountId(), "productName": snapshot.GetProductName(),
		"exchange": snapshot.GetExchange(), "accountName": snapshot.GetAccountName(),
		"accountEquityUsd":  snapshot.GetAccountEquityUsd(),
		"availableFundsUsd": snapshot.GetAvailableFundsUsd(), "riskPercent": snapshot.GetRiskPercent(),
		"positions": positions, "sourceUpdatedAt": protoTimeJSON(snapshot.GetSourceUpdatedAt()),
		"serverTime": protoTimeJSON(response.GetServerTime()), "stale": snapshot.GetStale(),
		"lastError": snapshot.GetLastError(),
	}
}

func protoTimeJSON(value *timestamppb.Timestamp) string {
	if value == nil || !value.IsValid() {
		return ""
	}
	return value.AsTime().UTC().Format(time.RFC3339Nano)
}

func corsMiddleware(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if origin != "" {
				writer.Header().Set("Access-Control-Allow-Origin", origin)
				writer.Header().Add("Vary", "Origin")
				writer.Header().Set("Access-Control-Allow-Credentials", "true")
				writer.Header().Set(
					"Access-Control-Allow-Headers",
					"Content-Type, Accept, If-None-Match, Idempotency-Key",
				)
				writer.Header().Set("Access-Control-Expose-Headers", "ETag")
				writer.Header().Set(
					"Access-Control-Allow-Methods",
					"GET, POST, PATCH, DELETE, OPTIONS",
				)
			}
			if request.Method == http.MethodOptions {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
}

func (h *Handler) healthCheck(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]grpc_health_v1.HealthClient{"funding": h.health}
	if h.accountHealth != nil {
		checks["account"] = h.accountHealth
	}
	if h.polymarketHealth != nil {
		checks["polymarket"] = h.polymarketHealth
	}
	if h.reportHealth != nil {
		checks["report"] = h.reportHealth
	}
	if h.traderHealth != nil {
		checks["trader"] = h.traderHealth
	}
	if h.aiHealth != nil {
		checks["ai"] = h.aiHealth
	}
	if h.spreadHealth != nil {
		checks["spread"] = h.spreadHealth
	}
	dependencies := make(map[string]string, len(checks))
	ready := true
	for name, client := range checks {
		response, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		if err != nil || response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
			dependencies[name] = "unavailable"
			ready = false
		} else {
			dependencies[name] = "ok"
		}
	}
	if !ready {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable", "dependencies": dependencies,
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "dependencies": dependencies})
}

type loginRequestBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) login(writer http.ResponseWriter, request *http.Request) {
	var body loginRequestBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "invalid login payload",
		})
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" || body.Password == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "username and password are required",
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.account.Login(ctx, &accountv1.LoginRequest{
		Username: username, Password: body.Password,
	})
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated, codes.InvalidArgument:
			writeJSON(writer, http.StatusUnauthorized, map[string]string{
				"error": "invalid credentials",
			})
		case codes.DeadlineExceeded:
			writeJSON(writer, http.StatusGatewayTimeout, map[string]string{
				"error": "login timed out",
			})
		default:
			writeJSON(writer, http.StatusBadGateway, map[string]string{
				"error": "account service unavailable",
			})
		}
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     h.sessionCookieName,
		Value:    response.GetToken(),
		Path:     "/",
		HttpOnly: true,
		Secure:   h.sessionCookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.sessionCookieMaxAge.Seconds()),
	})
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      response.GetUsername(),
		"permission":    response.GetPermission(),
	})
}

func (h *Handler) session(writer http.ResponseWriter, request *http.Request) {
	cookie, err := request.Cookie(h.sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"authenticated": false,
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.account.ValidateSession(ctx, &accountv1.ValidateSessionRequest{
		Token: cookie.Value,
	})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			h.clearSessionCookie(writer)
			writeJSON(writer, http.StatusOK, map[string]any{
				"authenticated": false,
			})
			return
		}
		writeJSON(writer, http.StatusBadGateway, map[string]string{
			"error": "account service unavailable",
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      response.GetUsername(),
		"permission":    response.GetPermission(),
	})
}

func (h *Handler) logout(writer http.ResponseWriter, _ *http.Request) {
	h.clearSessionCookie(writer)
	writeJSON(writer, http.StatusOK, map[string]any{
		"authenticated": false,
	})
}

func (h *Handler) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name:     h.sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.sessionCookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (h *Handler) sessionToken(request *http.Request) (string, bool) {
	cookie, err := request.Cookie(h.sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return "", false
	}
	return cookie.Value, true
}

type createTradingAccountBody struct {
	ProductName      string `json:"productName"`
	Exchange         string `json:"exchange"`
	AccountName      string `json:"accountName"`
	APIKey           string `json:"apiKey"`
	APISecret        string `json:"apiSecret"`
	Passphrase       string `json:"passphrase"`
	PrivateKey       string `json:"privateKey"`
	WalletType       string `json:"walletType"`
	FunderAddress    string `json:"funderAddress"`
	TradingAPIKey    string `json:"tradingApiKey"`
	TradingAPISecret string `json:"tradingApiSecret"`
	SigningAddress   string `json:"signingAddress"`
	VaultAddress     string `json:"vaultAddress"`
	AccountIndex     *int64 `json:"accountIndex"`
	APIKeyIndex      *int32 `json:"apiKeyIndex"`
}

func tradingAccountJSON(item *accountv1.TradingAccount) map[string]any {
	payload := map[string]any{
		"id":                       item.GetId(),
		"productName":              item.GetProductName(),
		"exchange":                 displayExchange(item.GetExchange()),
		"exchangeSlug":             strings.ToLower(item.GetExchange()),
		"accountName":              item.GetAccountName(),
		"hasPassphrase":            item.GetHasPassphrase(),
		"walletAddress":            item.GetWalletAddress(),
		"walletType":               item.GetWalletType(),
		"bindingStatus":            item.GetBindingStatus(),
		"credentialsPresent":       item.GetCredentialsPresent(),
		"credentialsVerified":      item.GetCredentialsVerified(),
		"tradingMode":              item.GetTradingMode(),
		"tradingReady":             item.GetTradingReady(),
		"tradingStatus":            item.GetTradingStatus(),
		"tradingUnavailableCode":   item.GetTradingUnavailableCode(),
		"tradingUnavailableReason": item.GetTradingUnavailableReason(),
		"spotFee":                  marketFeeJSON(item.GetSpotFee()),
		"contractFee":              marketFeeJSON(item.GetContractFee()),
		"feeSource":                item.GetFeeSource(),
		"feeUpdatedAt":             optionalRFC3339(item.GetFeeUpdatedAt()),
		"feeSyncStatus":            item.GetFeeSyncStatus(),
		"feeSyncError":             item.GetFeeSyncError(),
		"feeStale":                 item.GetFeeStale(),
		"unsupportedMarkets":       item.GetUnsupportedMarkets(),
		"createdAt":                item.GetCreatedAt().AsTime().UTC().Format(time.RFC3339Nano),
		"updatedAt":                item.GetUpdatedAt().AsTime().UTC().Format(time.RFC3339Nano),
	}
	if item.ResolvedAccountIndex != nil {
		payload["resolvedAccountIndex"] = item.GetResolvedAccountIndex()
	}
	if item.ResolvedApiKeyIndex != nil {
		payload["resolvedApiKeyIndex"] = item.GetResolvedApiKeyIndex()
	}
	return payload
}

func marketFeeJSON(item *accountv1.MarketFeeRate) map[string]any {
	if item == nil {
		return map[string]any{"status": "", "maker": "", "taker": ""}
	}
	return map[string]any{
		"status": item.GetStatus(), "maker": item.GetMaker(), "taker": item.GetTaker(),
	}
}

func optionalRFC3339(value *timestamppb.Timestamp) string {
	if value == nil {
		return ""
	}
	return value.AsTime().UTC().Format(time.RFC3339Nano)
}

func feeRatesJSON(item *accountv1.GetTradingAccountFeeRatesResponse) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"spotFee":            marketFeeJSON(item.GetSpotFee()),
		"contractFee":        marketFeeJSON(item.GetContractFee()),
		"feeSource":          item.GetFeeSource(),
		"feeUpdatedAt":       optionalRFC3339(item.GetFeeUpdatedAt()),
		"feeSyncStatus":      item.GetFeeSyncStatus(),
		"feeSyncError":       item.GetFeeSyncError(),
		"feeStale":           item.GetFeeStale(),
		"unsupportedMarkets": item.GetUnsupportedMarkets(),
	}
}

func (h *Handler) writeAccountError(writer http.ResponseWriter, err error, fallback string) {
	httpStatus := http.StatusBadGateway
	message := fallback
	switch status.Code(err) {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
		message = "authentication required"
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
		message = status.Convert(err).Message()
	case codes.AlreadyExists:
		httpStatus = http.StatusConflict
		message = "trading account already exists"
	case codes.NotFound:
		httpStatus = http.StatusNotFound
		message = "trading account not found"
	case codes.FailedPrecondition:
		httpStatus = http.StatusConflict
		message = status.Convert(err).Message()
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
		message = "account service timed out"
	}
	writeJSON(writer, httpStatus, map[string]string{"error": message})
}

func (h *Handler) listTradingAccounts(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.account.ListTradingAccounts(ctx, &accountv1.ListTradingAccountsRequest{
		Token: token,
	})
	if err != nil {
		h.writeAccountError(writer, err, "trading accounts unavailable")
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, tradingAccountJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"total": len(data)},
	})
}

func (h *Handler) createTradingAccount(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return
	}
	var body createTradingAccountBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "invalid trading account payload",
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	var created *accountv1.TradingAccount
	var err error
	if strings.EqualFold(strings.TrimSpace(body.Exchange), "polymarket") {
		response, createErr := h.account.CreatePolymarketTradingAccount(
			ctx,
			&accountv1.CreatePolymarketTradingAccountRequest{
				Token: token, ProductName: body.ProductName,
				AccountName: body.AccountName, PrivateKey: body.PrivateKey,
				WalletType: body.WalletType, FunderAddress: body.FunderAddress,
			},
		)
		err = createErr
		if response != nil {
			created = response.GetAccount()
		}
	} else {
		response, createErr := h.account.CreateTradingAccount(ctx, &accountv1.CreateTradingAccountRequest{
			Token:            token,
			ProductName:      body.ProductName,
			Exchange:         body.Exchange,
			AccountName:      body.AccountName,
			ApiKey:           body.APIKey,
			ApiSecret:        body.APISecret,
			Passphrase:       body.Passphrase,
			TradingApiKey:    body.TradingAPIKey,
			TradingApiSecret: body.TradingAPISecret,
			SigningAddress:   body.SigningAddress,
			VaultAddress:     body.VaultAddress,
			AccountIndex:     body.AccountIndex,
			ApiKeyIndex:      body.APIKeyIndex,
		})
		err = createErr
		if response != nil {
			created = response.GetAccount()
		}
	}
	if err != nil {
		h.writeAccountError(writer, err, "create trading account failed")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"data": tradingAccountJSON(created),
	})
}

func (h *Handler) deleteTradingAccount(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(request, "id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "trading account id is required",
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	_, err = h.account.DeleteTradingAccount(ctx, &accountv1.DeleteTradingAccountRequest{
		Token: token, Id: id,
	})
	if err != nil {
		h.writeAccountError(writer, err, "delete trading account failed")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"deleted": true})
}

func (h *Handler) getTradingAccountFeeRates(writer http.ResponseWriter, request *http.Request) {
	h.handleFeeRates(writer, request, false)
}

func (h *Handler) syncTradingAccountFeeRates(writer http.ResponseWriter, request *http.Request) {
	h.handleFeeRates(writer, request, true)
}

func (h *Handler) handleFeeRates(writer http.ResponseWriter, request *http.Request, sync bool) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(request, "id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "trading account id is required",
		})
		return
	}
	timeout := 2 * time.Second
	if sync {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	var response *accountv1.GetTradingAccountFeeRatesResponse
	if sync {
		response, err = h.account.SyncTradingAccountFeeRates(ctx, &accountv1.SyncTradingAccountFeeRatesRequest{
			Token: token, Id: id,
		})
	} else {
		response, err = h.account.GetTradingAccountFeeRates(ctx, &accountv1.GetTradingAccountFeeRatesRequest{
			Token: token, Id: id,
		})
	}
	if err != nil {
		h.writeAccountError(writer, err, "trading account fee rates unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": feeRatesJSON(response)})
}

func (h *Handler) inspectTradingReadiness(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(request, "id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "trading account id is required",
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	response, err := h.account.InspectTradingReadiness(ctx, &accountv1.InspectTradingReadinessRequest{
		Token: token, TradingAccountId: id,
	})
	if err != nil {
		h.writeAccountError(writer, err, "trading readiness unavailable")
		return
	}
	payload := map[string]any{
		"tradingAccountId":         response.GetTradingAccountId(),
		"exchange":                 displayExchange(response.GetExchange()),
		"exchangeSlug":             strings.ToLower(response.GetExchange()),
		"credentialsPresent":       response.GetCredentialsPresent(),
		"credentialsVerified":      response.GetCredentialsVerified(),
		"tradingMode":              response.GetTradingMode(),
		"tradingReady":             response.GetTradingReady(),
		"tradingStatus":            response.GetTradingStatus(),
		"tradingUnavailableCode":   response.GetTradingUnavailableCode(),
		"tradingUnavailableReason": response.GetTradingUnavailableReason(),
	}
	if response.ResolvedAccountIndex != nil {
		payload["resolvedAccountIndex"] = response.GetResolvedAccountIndex()
	}
	if response.ResolvedApiKeyIndex != nil {
		payload["resolvedApiKeyIndex"] = response.GetResolvedApiKeyIndex()
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": payload})
}

func (h *Handler) listFundingRates(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.funding.ListFundingRates(ctx, &fundingv1.ListFundingRatesRequest{})
	if err != nil {
		httpStatus := http.StatusBadGateway
		switch status.Code(err) {
		case codes.InvalidArgument:
			httpStatus = http.StatusBadRequest
		case codes.DeadlineExceeded:
			httpStatus = http.StatusGatewayTimeout
		case codes.Unavailable:
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(writer, httpStatus, map[string]any{
			"error":     "funding service unavailable",
			"requestId": middleware.GetReqID(request.Context()),
		})
		return
	}
	serverTime := response.GetServerTime().AsTime().UTC()
	etag := `"` + response.GetSnapshotVersion() + `"`
	writer.Header().Set("ETag", etag)
	writer.Header().Set("Cache-Control", "no-cache")
	if request.Header.Get("If-None-Match") == etag {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, fundingRateJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{
			"total":           len(data),
			"availableTotal":  response.GetTotal(),
			"snapshotVersion": response.GetSnapshotVersion(),
			"serverTime":      serverTime.Format(time.RFC3339Nano),
		},
	})
}

func fundingRateJSON(item *fundingv1.FundingRate) map[string]any {
	nextFundingAt := item.GetNextFundingAt().AsTime().UTC()
	sourceUpdatedAt := item.GetSourceUpdatedAt().AsTime().UTC()
	var nextFundingRate any
	if item.NextFundingRate != nil {
		nextFundingRate = item.GetNextFundingRate()
	}
	latestPrice := item.GetLastPrice()
	if latestPrice == "0" || latestPrice == "" {
		latestPrice = item.GetMarkPrice()
	}
	row := map[string]any{
		"id":                      item.GetExchange() + "-" + strings.ToLower(item.GetExchangeSymbol()),
		"exchange":                displayExchange(item.GetExchange()),
		"exchangeSymbol":          item.GetExchangeSymbol(),
		"symbol":                  item.GetGlobalSymbol(),
		"baseAsset":               item.GetBaseAsset(),
		"quoteAsset":              item.GetQuoteAsset(),
		"positionQuantity":        item.GetPositionQuantity(),
		"positionNotional":        item.GetPositionNotionalUsd(),
		"dailyVolume":             item.GetTurnover_24HUsd(),
		"annualizedRate":          item.GetAnnualizedRate(),
		"currentFundingRate":      item.GetFundingRate(),
		"nextFundingRate":         nextFundingRate,
		"settlementIntervalHours": item.GetFundingIntervalSeconds() / 3600,
		"nextFundingAt":           nextFundingAt.Format(time.RFC3339Nano),
		"cumulative24h":           item.GetCumulative_24H(),
		"cumulative7d":            item.GetCumulative_7D(),
		"latestPrice":             latestPrice,
		"priceChange24h":          item.GetPriceChange_24H(),
		"sourceUpdatedAt":         sourceUpdatedAt.Format(time.RFC3339Nano),
		"stale":                   item.GetStale(),
		"venueContractType":       venueContractTypeJSON(item.GetVenueContractType()),
		"index": map[string]any{
			"name":   strings.ToUpper(item.GetExchange()) + "_INDEX",
			"value":  item.GetIndexPrice(),
			"weight": "100",
		},
	}
	if item.History_24HComplete != nil {
		row["history24hComplete"] = item.GetHistory_24HComplete()
	}
	if item.History_7DComplete != nil {
		row["history7dComplete"] = item.GetHistory_7DComplete()
	}
	return row
}

type fundingLookupBody struct {
	Keys []fundingLookupKeyBody `json:"keys"`
}

type fundingLookupKeyBody struct {
	Exchange       string `json:"exchange"`
	ExchangeSymbol string `json:"exchangeSymbol"`
	BaseAsset      string `json:"baseAsset"`
	QuoteAsset     string `json:"quoteAsset"`
}

func (h *Handler) lookupFundingRates(writer http.ResponseWriter, request *http.Request) {
	var body fundingLookupBody
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid lookup payload"})
		return
	}
	keys := make([]*fundingv1.FundingRateLookupKey, 0, len(body.Keys))
	for _, item := range body.Keys {
		keys = append(keys, &fundingv1.FundingRateLookupKey{
			Exchange:       item.Exchange,
			ExchangeSymbol: item.ExchangeSymbol,
			BaseAsset:      item.BaseAsset,
			QuoteAsset:     item.QuoteAsset,
		})
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.funding.BatchGetFundingRates(ctx, &fundingv1.BatchGetFundingRatesRequest{
		Keys: keys,
	})
	if err != nil {
		if status.Code(err) == codes.InvalidArgument {
			writeJSON(writer, http.StatusBadRequest, map[string]string{
				"error": status.Convert(err).Message(),
			})
			return
		}
		httpStatus := http.StatusBadGateway
		switch status.Code(err) {
		case codes.DeadlineExceeded:
			httpStatus = http.StatusGatewayTimeout
		case codes.Unavailable:
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(writer, httpStatus, map[string]any{
			"error":     "funding service unavailable",
			"requestId": middleware.GetReqID(request.Context()),
		})
		return
	}
	serverTime := response.GetServerTime().AsTime().UTC()
	results := make([]map[string]any, 0, len(response.GetResults()))
	for _, item := range response.GetResults() {
		key := item.GetKey()
		row := map[string]any{
			"key": map[string]any{
				"exchange":       key.GetExchange(),
				"exchangeSymbol": key.GetExchangeSymbol(),
				"baseAsset":      key.GetBaseAsset(),
				"quoteAsset":     key.GetQuoteAsset(),
			},
			"status": item.GetStatus(),
		}
		if item.GetStatus() == "hit" && item.GetItem() != nil {
			row["item"] = fundingRateJSON(item.GetItem())
		}
		results = append(results, row)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"results": results,
		"meta": map[string]any{
			"snapshotVersion": response.GetSnapshotVersion(),
			"serverTime":      serverTime.Format(time.RFC3339Nano),
		},
	})
}

func (h *Handler) listFundingSpreads(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.funding.ListFundingSpreads(
		ctx, &fundingv1.ListFundingSpreadsRequest{},
	)
	if err != nil {
		httpStatus := http.StatusBadGateway
		switch status.Code(err) {
		case codes.DeadlineExceeded:
			httpStatus = http.StatusGatewayTimeout
		case codes.Unavailable:
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(writer, httpStatus, map[string]any{
			"error":     "funding service unavailable",
			"requestId": middleware.GetReqID(request.Context()),
		})
		return
	}
	serverTime := response.GetServerTime().AsTime().UTC()
	etag := `"` + response.GetSnapshotVersion() + `"`
	writer.Header().Set("ETag", etag)
	writer.Header().Set("Cache-Control", "no-cache")
	if request.Header.Get("If-None-Match") == etag {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		longLeg := fundingSpreadLegJSON(item.GetLongLeg())
		shortLeg := fundingSpreadLegJSON(item.GetShortLeg())
		row := map[string]any{
			"id": item.GetGlobalSymbol() + "-" +
				item.GetLongLeg().GetExchange() + "-" + item.GetShortLeg().GetExchange(),
			"symbol":              item.GetGlobalSymbol(),
			"baseAsset":           item.GetBaseAsset(),
			"quoteAsset":          item.GetQuoteAsset(),
			"longLeg":             longLeg,
			"shortLeg":            shortLeg,
			"spreadAnnualized":    item.GetSingleSpreadAnnualized(),
			"spread24hAnnualized": item.GetSpread_24HAnnualized(),
			"spread7dAnnualized":  item.GetSpread_7DAnnualized(),
			"minPositionNotional": item.GetMinPositionNotionalUsd(),
			"minDailyVolume":      item.GetMinTurnover_24HUsd(),
			"sourceUpdatedAt":     item.GetUpdatedAt().AsTime().UTC().Format(time.RFC3339Nano),
			"stale":               item.GetStale(),
		}
		if item.History_24HComplete != nil {
			row["history24hComplete"] = item.GetHistory_24HComplete()
		}
		if item.History_7DComplete != nil {
			row["history7dComplete"] = item.GetHistory_7DComplete()
		}
		data = append(data, row)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{
			"total":           len(data),
			"snapshotVersion": response.GetSnapshotVersion(),
			"serverTime":      serverTime.Format(time.RFC3339Nano),
		},
	})
}

func fundingSpreadLegJSON(item *fundingv1.FundingSpreadLeg) map[string]any {
	row := map[string]any{
		"exchange":                displayExchange(item.GetExchange()),
		"exchangeSymbol":          item.GetExchangeSymbol(),
		"globalSymbol":            item.GetGlobalSymbol(),
		"baseAsset":               item.GetBaseAsset(),
		"quoteAsset":              item.GetQuoteAsset(),
		"fundingRate":             item.GetEffectiveFundingRate(),
		"settlementIntervalHours": item.GetFundingIntervalSeconds() / 3600,
		"nextFundingAt":           item.GetNextFundingAt().AsTime().UTC().Format(time.RFC3339Nano),
		"positionNotional":        item.GetPositionNotionalUsd(),
		"dailyVolume":             item.GetTurnover_24HUsd(),
		"latestPrice":             item.GetLastPrice(),
		"sourceUpdatedAt":         item.GetSourceUpdatedAt().AsTime().UTC().Format(time.RFC3339Nano),
		"stale":                   item.GetStale(),
		"venueContractType":       venueContractTypeJSON(item.GetVenueContractType()),
	}
	if item != nil && item.History_24HComplete != nil {
		row["history24hComplete"] = item.GetHistory_24HComplete()
	}
	if item != nil && item.History_7DComplete != nil {
		row["history7dComplete"] = item.GetHistory_7DComplete()
	}
	return row
}

func (h *Handler) listFundingOpportunities(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	period := strings.ToLower(strings.TrimSpace(query.Get("period")))
	if period == "" {
		period = "8h"
	}
	limit := 100
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "limit must be positive"})
			return
		}
		limit = min(parsed, 100)
	}
	requestMessage := &fundingv1.ListFundingOpportunitiesRequest{
		Period: period, MinLegNotionalUsd: strings.TrimSpace(query.Get("minLegNotionalUsd")),
		MinLegVolume_24HUsd: strings.TrimSpace(query.Get("minLegVolume24hUsd")),
		Limit:               int32(limit),
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.funding.ListFundingOpportunities(ctx, requestMessage)
	if err != nil {
		if status.Code(err) == codes.InvalidArgument {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": status.Convert(err).Message()})
			return
		}
		httpStatus := http.StatusBadGateway
		if status.Code(err) == codes.DeadlineExceeded {
			httpStatus = http.StatusGatewayTimeout
		} else if status.Code(err) == codes.Unavailable {
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(writer, httpStatus, map[string]any{
			"error":     "funding opportunity service unavailable",
			"requestId": middleware.GetReqID(request.Context()),
		})
		return
	}
	etag := `"` + response.GetSnapshotVersion() + "-" + response.GetStatus() + "-" +
		strconv.FormatUint(response.GetGeneration(), 10) + "-" + period + "-" +
		requestMessage.GetMinLegNotionalUsd() + "-" + requestMessage.GetMinLegVolume_24HUsd() +
		"-" + strconv.Itoa(limit) + `"`
	writer.Header().Set("ETag", etag)
	writer.Header().Set("Cache-Control", "no-cache")
	statusValue := response.GetStatus()
	if statusValue == "" && !response.GetStale() {
		statusValue = "ready"
	}
	if request.Header.Get("If-None-Match") == etag &&
		statusValue == "ready" && !response.GetStale() {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, map[string]any{
			"id": item.GetGlobalSymbol() + "-" + item.GetLongLeg().GetExchange() + "-" +
				item.GetShortLeg().GetExchange() + "-" + item.GetPeriod(),
			"rank": item.GetRank(), "symbol": item.GetGlobalSymbol(),
			"baseAsset": item.GetBaseAsset(), "quoteAsset": item.GetQuoteAsset(),
			"period": item.GetPeriod(), "longLeg": fundingSpreadLegJSON(item.GetLongLeg()),
			"shortLeg":                   fundingSpreadLegJSON(item.GetShortLeg()),
			"currentMidSpreadBps":        item.GetCurrentMidSpreadBps(),
			"currentExecutableSpreadBps": item.GetCurrentExecutableSpreadBps(),
			"targetSpreadBps":            item.GetTargetSpreadBps(),
			"periodExpectedReturn":       item.GetPeriodExpectedReturn(),
			"fundingExpectedAnnualized":  item.GetFundingExpectedAnnualized(),
			"spreadExpectedAnnualized":   item.GetSpreadExpectedAnnualized(),
			"combinedExpectedAnnualized": item.GetCombinedExpectedAnnualized(),
			"firstPassageProbability":    item.GetFirstPassageProbability(),
			"profitProbability":          item.GetProfitProbability(),
			"expectedExitMinutes":        item.GetExpectedExitMinutes(),
			"p5Return":                   item.GetP5Return(),
			"minPositionNotional":        item.GetMinPositionNotionalUsd(),
			"minDailyVolume":             item.GetMinTurnover_24HUsd(),
			"coverage":                   item.GetCoverage(),
			"confidence":                 item.GetConfidence(),
			"modelState":                 item.GetModelState(),
			"sampleCount":                item.GetSampleCount(),
			"expectedPaybackMinutes":     item.GetExpectedPaybackMinutes(),
			"paybackStatus":              item.GetPaybackStatus(),
			"sourceUpdatedAt":            item.GetUpdatedAt().AsTime().UTC().Format(time.RFC3339Nano),
			"stale":                      item.GetStale(),
		})
	}
	calculatedAt := ""
	if response.GetCalculatedAt() != nil {
		calculatedAt = response.GetCalculatedAt().AsTime().UTC().Format(time.RFC3339Nano)
	}
	lastSuccessfulAt := ""
	if response.GetLastSuccessfulAt() != nil {
		lastSuccessfulAt = response.GetLastSuccessfulAt().AsTime().UTC().Format(time.RFC3339Nano)
	}
	dataThrough := ""
	if response.GetDataThrough() != nil {
		dataThrough = response.GetDataThrough().AsTime().UTC().Format(time.RFC3339Nano)
	}
	serverTime := time.Now().UTC().Format(time.RFC3339Nano)
	if response.GetServerTime() != nil {
		serverTime = response.GetServerTime().AsTime().UTC().Format(time.RFC3339Nano)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{
			"total": response.GetTotal(), "snapshotVersion": response.GetSnapshotVersion(),
			"serverTime": serverTime, "calculatedAt": calculatedAt, "stale": response.GetStale(),
			"status": statusValue, "lastSuccessfulAt": lastSuccessfulAt,
			"dataThrough": dataThrough, "generation": response.GetGeneration(),
		},
	})
}

func (h *Handler) getFundingHistory(writer http.ResponseWriter, request *http.Request) {
	limit := 10
	if value := request.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{
				"error": "limit must be a positive integer",
			})
			return
		}
		limit = min(parsed, 100)
	}
	exchangeName := strings.ToLower(strings.TrimSpace(chi.URLParam(request, "exchange")))
	exchangeSymbol := strings.TrimSpace(chi.URLParam(request, "exchangeSymbol"))
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	response, err := h.funding.GetFundingHistory(
		ctx,
		&fundingv1.GetFundingHistoryRequest{
			Exchange: exchangeName, ExchangeSymbol: exchangeSymbol, Limit: int32(limit),
		},
	)
	if err != nil {
		httpStatus := http.StatusBadGateway
		switch status.Code(err) {
		case codes.InvalidArgument:
			httpStatus = http.StatusBadRequest
		case codes.DeadlineExceeded:
			httpStatus = http.StatusGatewayTimeout
		case codes.Unavailable:
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(writer, httpStatus, map[string]any{
			"error": "funding history unavailable",
		})
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, point := range response.GetItems() {
		data = append(data, map[string]any{
			"rate":      point.GetRate(),
			"settledAt": point.GetSettledAt().AsTime().UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{
			"exchange": exchangeName, "exchangeSymbol": exchangeSymbol, "total": len(data),
		},
	})
}

func displayExchange(value string) string {
	switch strings.ToLower(value) {
	case "okx":
		return "OKX"
	case "hyperliquid":
		return "Hyperliquid"
	default:
		if value == "" {
			return value
		}
		return strings.ToUpper(value[:1]) + strings.ToLower(value[1:])
	}
}

func venueContractTypeJSON(value string) string {
	if strings.TrimSpace(value) == "" {
		return "PERPETUAL"
	}
	return value
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
