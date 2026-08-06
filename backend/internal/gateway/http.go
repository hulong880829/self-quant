package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	fundingv1 "selfquant/backend/gen/funding/v1"
)

type Handler struct {
	funding fundingv1.FundingServiceClient
	health  grpc_health_v1.HealthClient
}

func NewRouter(funding fundingv1.FundingServiceClient, health grpc_health_v1.HealthClient) http.Handler {
	handler := &Handler{funding: funding, health: health}
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Timeout(5 * time.Second))
	router.Use(corsMiddleware(os.Getenv("GATEWAY_CORS_ORIGIN")))
	router.Get("/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Get("/health/ready", handler.healthCheck)
	router.Get("/api/v1/funding-rates", handler.listFundingRates)
	return router
}

func corsMiddleware(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if origin != "" {
				writer.Header().Set("Access-Control-Allow-Origin", origin)
				writer.Header().Set("Vary", "Origin")
				writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
				writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
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
	response, err := h.health.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil || response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"status": "unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
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
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		history := make([]map[string]any, 0, len(item.GetHistory()))
		for _, point := range item.GetHistory() {
			history = append(history, map[string]any{
				"rate":      point.GetRate(),
				"settledAt": point.GetSettledAt().AsTime().UTC().Format(time.RFC3339Nano),
			})
		}
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
		data = append(data, map[string]any{
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
			"fundingHistory":          history,
			"index": map[string]any{
				"name":   strings.ToUpper(item.GetExchange()) + "_INDEX",
				"value":  item.GetIndexPrice(),
				"weight": "100",
			},
		})
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

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
