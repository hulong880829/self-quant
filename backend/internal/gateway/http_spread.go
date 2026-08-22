package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	spreadv1 "selfquant/backend/gen/spread/v1"
)

func (h *Handler) getBasisSpreadHistory(writer http.ResponseWriter, request *http.Request) {
	venue := strings.ToLower(strings.TrimSpace(chi.URLParam(request, "venue")))
	baseAsset := strings.ToUpper(strings.TrimSpace(chi.URLParam(request, "baseAsset")))
	quoteAsset := strings.ToUpper(strings.TrimSpace(chi.URLParam(request, "quoteAsset")))
	rangeValue := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("range")))
	if rangeValue == "" {
		rangeValue = "24h"
	}
	protoRange, ok := basisSpreadRange(rangeValue)
	if !ok || !validSpreadVenue(venue) || !validSpreadAsset(baseAsset) || !validSpreadAsset(quoteAsset) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "venue, assets and range are invalid",
		})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 6*time.Second)
	defer cancel()
	response, err := h.spread.GetBasisSpreadHistory(ctx, &spreadv1.GetBasisSpreadHistoryRequest{
		Venue: venue, BaseAsset: baseAsset, QuoteAsset: quoteAsset, Range: protoRange,
	})
	if err != nil {
		h.writeSpreadError(writer, request, err)
		return
	}
	payload := basisSpreadHistoryJSON(response, rangeValue)
	etag := basisSpreadETag(venue, baseAsset, quoteAsset, rangeValue, response.GetAsOf().AsTime())
	writer.Header().Set("ETag", etag)
	writer.Header().Set("Cache-Control", "public, max-age=15")
	if request.Header.Get("If-None-Match") == etag {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(writer, http.StatusOK, payload)
}

func (h *Handler) writeSpreadError(writer http.ResponseWriter, request *http.Request, err error) {
	httpStatus := http.StatusBadGateway
	message := "basis spread unavailable"
	switch status.Code(err) {
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
		message = "invalid basis spread request"
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
		message = "basis spread query timed out"
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
		message = "basis spread query failed"
	}
	writeJSON(writer, httpStatus, map[string]any{
		"error":     message,
		"requestId": middleware.GetReqID(request.Context()),
	})
}

func basisSpreadHistoryJSON(response *spreadv1.GetBasisSpreadHistoryResponse, rangeValue string) map[string]any {
	points := make([]map[string]any, 0, len(response.GetPoints()))
	for _, point := range response.GetPoints() {
		points = append(points, map[string]any{
			"ts":           protoTimeJSON(point.GetTs()),
			"spreadBps":    point.GetSpreadBps(),
			"spotAsk":      point.GetSpotAsk(),
			"perpetualAsk": point.GetPerpetualAsk(),
			"samples":      point.GetSamples(),
		})
	}
	summary := response.GetSummary()
	return map[string]any{
		"venue":             response.GetVenue(),
		"baseAsset":         response.GetBaseAsset(),
		"quoteAsset":        response.GetQuoteAsset(),
		"canonicalSymbol":   response.GetCanonicalSymbol(),
		"range":             rangeValue,
		"resolutionSeconds": response.GetResolutionSeconds(),
		"availability":      basisSpreadAvailabilityJSON(response.GetAvailability()),
		"asOf":              protoTimeJSON(response.GetAsOf()),
		"points":            points,
		"summary": map[string]any{
			"currentBps": summary.GetCurrentBps(),
			"minBps":     summary.GetMinBps(),
			"maxBps":     summary.GetMaxBps(),
			"avgBps":     summary.GetAvgBps(),
			"coverage":   summary.GetCoverage(),
		},
	}
}

func basisSpreadRange(value string) (spreadv1.BasisSpreadRange, bool) {
	switch value {
	case "1h":
		return spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_1H, true
	case "4h":
		return spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_4H, true
	case "8h":
		return spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_8H, true
	case "24h":
		return spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_24H, true
	case "7d":
		return spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_7D, true
	default:
		return spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_UNSPECIFIED, false
	}
}

func basisSpreadAvailabilityJSON(value spreadv1.BasisSpreadAvailability) string {
	if value == spreadv1.BasisSpreadAvailability_BASIS_SPREAD_AVAILABILITY_AVAILABLE {
		return "available"
	}
	return "unavailable"
}

func basisSpreadETag(venue, baseAsset, quoteAsset, rangeValue string, asOf time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		venue, baseAsset, quoteAsset, rangeValue, asOf.UTC().Format(time.RFC3339Nano),
	}, "|")))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func validSpreadVenue(value string) bool {
	if len(value) < 2 || len(value) > 32 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validSpreadAsset(value string) bool {
	if len(value) < 2 || len(value) > 16 {
		return false
	}
	for _, r := range value {
		if !unicode.IsUpper(r) && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
