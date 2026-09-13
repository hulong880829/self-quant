package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	reportv1 "selfquant/backend/gen/report/v1"
)

func (h *Handler) reportRequest(
	writer http.ResponseWriter,
	request *http.Request,
) (string, int64, bool) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return "", 0, false
	}
	productID, err := strconv.ParseInt(
		strings.TrimSpace(chi.URLParam(request, "id")), 10, 64,
	)
	if err != nil || productID <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid product id"})
		return "", 0, false
	}
	return token, productID, true
}

func (h *Handler) listReportProducts(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.report.ListProducts(ctx, &reportv1.ListProductsRequest{Token: token})
	if err != nil {
		writeReportError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, reportProductJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"total": len(data)},
	})
}

func (h *Handler) getReportProduct(writer http.ResponseWriter, request *http.Request) {
	token, productID, ok := h.reportRequest(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.report.GetProductDetail(ctx, &reportv1.GetProductDetailRequest{
		Token: token, ProductId: productID,
	})
	if err != nil {
		writeReportError(writer, err)
		return
	}
	flows := make([]map[string]any, 0, len(response.GetRecentCashFlows()))
	for _, item := range response.GetRecentCashFlows() {
		flows = append(flows, reportCashFlowJSON(item))
	}
	rows := make([]map[string]any, 0, len(response.GetDaily()))
	partial := false
	recomputing := false
	invalid := false
	for _, item := range response.GetDaily() {
		rows = append(rows, reportDailyJSON(item))
		switch item.GetStatus() {
		case "partial":
			partial = true
		case "recomputing":
			recomputing = true
		case "invalid":
			invalid = true
		}
	}
	aumSeries := reportAUMSeriesJSON(response.GetDaily())
	var latest any
	dataDate := ""
	if response.GetLatestSnapshot() != nil {
		latest = reportDailyJSON(response.GetLatestSnapshot())
		dataDate = response.GetLatestSnapshot().GetReportDate()
	}
	reportStatus := "final"
	if recomputing {
		reportStatus = "recomputing"
	} else if invalid {
		reportStatus = "failed"
	} else if partial {
		reportStatus = "provisional"
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": map[string]any{
			"product":         reportProductJSON(response.GetProduct()),
			"latest":          latest,
			"latestSnapshot":  latest,
			"rows":            rows,
			"daily":           rows,
			"aumSeries":       aumSeries,
			"status":          reportStatus,
			"partial":         partial,
			"errors":          []string{},
			"dataDate":        dataDate,
			"serverTime":      time.Now().UTC().Format(time.RFC3339Nano),
			"recentCashFlows": flows,
		},
	})
}

func reportAUMSeriesJSON(items []*reportv1.DailySnapshot) []map[string]any {
	series := make([]map[string]any, 0, len(items))
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		series = append(series, map[string]any{
			"date": item.GetReportDate(),
			"aum":  item.GetClosingEquityUsd(),
		})
	}
	return series
}

func (h *Handler) listReportDaily(writer http.ResponseWriter, request *http.Request) {
	token, productID, ok := h.reportRequest(writer, request)
	if !ok {
		return
	}
	limit, ok := reportLimit(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.report.ListDailySnapshots(ctx, &reportv1.ListDailySnapshotsRequest{
		Token: token, ProductId: productID,
		FromDate: request.URL.Query().Get("from"), ToDate: request.URL.Query().Get("to"),
		Limit: int32(limit),
	})
	if err != nil {
		writeReportError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, reportDailyJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"total": len(data)}})
}

func (h *Handler) listReportCashFlows(writer http.ResponseWriter, request *http.Request) {
	token, productID, ok := h.reportRequest(writer, request)
	if !ok {
		return
	}
	limit, ok := reportLimit(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.report.ListCashFlows(ctx, &reportv1.ListCashFlowsRequest{
		Token: token, ProductId: productID,
		FromDate: request.URL.Query().Get("from"), ToDate: request.URL.Query().Get("to"),
		Limit: int32(limit),
	})
	if err != nil {
		writeReportError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, reportCashFlowJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"total": len(data)}})
}

type createReportCashFlowBody struct {
	FlowDate   string `json:"flowDate"`
	AmountUSD  string `json:"amountUsd"`
	FlowType   string `json:"flowType"`
	Type       string `json:"type"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	OccurredAt string `json:"occurredAt"`
	Status     string `json:"status"`
	Note       string `json:"note"`
	Confirmed  bool   `json:"confirmed"`
}

func (h *Handler) createReportCashFlow(writer http.ResponseWriter, request *http.Request) {
	token, productID, ok := h.reportRequest(writer, request)
	if !ok {
		return
	}
	var body createReportCashFlowBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid cash flow payload"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	flowType := body.FlowType
	if flowType == "" {
		flowType = body.Type
	}
	amount := body.AmountUSD
	if amount == "" {
		amount = body.Amount
	}
	if strings.TrimSpace(body.OccurredAt) == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "occurredAt is required"})
		return
	}
	parsed, parseErr := time.Parse(time.RFC3339, body.OccurredAt)
	if parseErr != nil {
		parsed, parseErr = time.Parse(time.RFC3339Nano, body.OccurredAt)
	}
	if parseErr != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid occurredAt"})
		return
	}
	response, err := h.report.CreateCashFlow(ctx, &reportv1.CreateCashFlowRequest{
		Token: token, ProductId: productID,
		AmountUsd: amount, FlowType: flowType, Note: body.Note,
		Confirmed:  body.Confirmed || body.Status == "confirmed",
		OccurredAt: timestamppb.New(parsed),
	})
	if err != nil {
		writeReportError(writer, err)
		return
	}
	payload := reportCashFlowJSON(response.GetCashFlow())
	payload["recomputeStatus"] = response.GetRecomputeStatus()
	writeJSON(writer, http.StatusCreated, map[string]any{"data": payload})
}

type recomputeReportBody struct {
	FromDate string `json:"fromDate"`
	ToDate   string `json:"toDate"`
}

func (h *Handler) recomputeReportProduct(writer http.ResponseWriter, request *http.Request) {
	token, productID, ok := h.reportRequest(writer, request)
	if !ok {
		return
	}
	var body recomputeReportBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid recompute payload"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	response, err := h.report.RecomputeProduct(ctx, &reportv1.RecomputeProductRequest{
		Token: token, ProductId: productID, FromDate: body.FromDate, ToDate: body.ToDate,
	})
	if err != nil {
		writeReportError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": map[string]any{"snapshotsWritten": response.GetSnapshotsWritten()},
	})
}

func reportLimit(writer http.ResponseWriter, request *http.Request) (int, bool) {
	value := strings.TrimSpace(request.URL.Query().Get("limit"))
	if value == "" {
		return 100, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > 1000 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 1000"})
		return 0, false
	}
	return limit, true
}

func reportProductJSON(item *reportv1.Product) map[string]any {
	return map[string]any{
		"id": strconv.FormatInt(item.GetId(), 10), "name": item.GetName(),
		"displayName": item.GetDisplayName(), "category": item.GetCategory(),
		"strategy": item.GetStrategy(), "currency": item.GetBaseCurrency(),
		"inceptionDate": item.GetInceptionDate(),
		"status":        map[bool]string{true: "active", false: "inactive"}[item.GetActive()],
		"baseCurrency":  item.GetBaseCurrency(),
		"timezone":      item.GetTimezone(), "active": item.GetActive(),
		"accountCount": item.GetAccountCount(), "latestEquityUsd": item.GetLatestEquityUsd(),
		"latestPnlUsd": item.GetLatestPnlUsd(), "latestReturnRate": item.GetLatestReturnRate(),
		"updatedAt": protoTimeJSON(item.GetUpdatedAt()),
	}
}

func reportDailyJSON(item *reportv1.DailySnapshot) map[string]any {
	status, partial := reportRowStatus(item.GetStatus())
	dailyReturn := nullableReportDecimal(item.GetReturnRate())
	if status == "recomputing" {
		dailyReturn = nil
	}
	return map[string]any{
		"id":        strconv.FormatInt(item.GetId(), 10),
		"productId": strconv.FormatInt(item.GetProductId(), 10),
		"date":      item.GetReportDate(), "reportDate": item.GetReportDate(),
		"absoluteReturn": item.GetAbsoluteReturn(), "dailyReturn": dailyReturn,
		"annualizedReturn":   nullableReportDecimal(item.GetAnnualizedReturn()),
		"annualized7d":       nullableReportDecimal(item.GetAnnualized_7D()),
		"annualized30d":      nullableReportDecimal(item.GetAnnualized_30D()),
		"maxDrawdown":        nullableReportDecimal(item.GetMaxDrawdown()),
		"capitalUtilization": nil, "sharpe": nullableReportDecimal(item.GetSharpe()),
		"aum":              item.GetClosingEquityUsd(),
		"volume24h":        nullableReportDecimal(item.GetVolume_24HUsd()),
		"openingEquityUsd": item.GetOpeningEquityUsd(), "closingEquityUsd": item.GetClosingEquityUsd(),
		"netCashFlowUsd": item.GetNetCashFlowUsd(), "pnlUsd": item.GetPnlUsd(),
		"returnRate": dailyReturn, "sampleCount": item.GetSampleCount(),
		"status":      status,
		"partial":     partial,
		"finalizedAt": protoTimeJSON(item.GetFinalizedAt()),
	}
}

func reportRowStatus(status string) (string, bool) {
	switch status {
	case "partial":
		return "provisional", true
	case "recomputing":
		return "recomputing", false
	case "invalid":
		return "failed", false
	default:
		return "final", false
	}
}

func reportCashFlowJSON(item *reportv1.CashFlow) map[string]any {
	return map[string]any{
		"id":        strconv.FormatInt(item.GetId(), 10),
		"productId": strconv.FormatInt(item.GetProductId(), 10),
		"type":      item.GetFlowType(), "amount": item.GetAmountUsd(), "currency": "USD",
		"occurredAt": protoTimeJSON(item.GetOccurredAt()),
		"status":     map[bool]string{true: "confirmed", false: "pending"}[item.GetConfirmed()],
		"updatedAt":  protoTimeJSON(item.GetCreatedAt()), "flowDate": item.GetFlowDate(),
		"amountUsd": item.GetAmountUsd(), "flowType": item.GetFlowType(), "note": item.GetNote(),
		"confirmed": item.GetConfirmed(), "confirmedBy": item.GetConfirmedBy(),
		"confirmedAt": protoTimeJSON(item.GetConfirmedAt()), "createdBy": item.GetCreatedBy(),
		"createdAt": protoTimeJSON(item.GetCreatedAt()),
	}
}

func nullableReportDecimal(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func writeReportError(writer http.ResponseWriter, err error) {
	httpStatus := http.StatusBadGateway
	message := "report service unavailable"
	switch status.Code(err) {
	case codes.Unauthenticated:
		httpStatus, message = http.StatusUnauthorized, "authentication required"
	case codes.InvalidArgument:
		httpStatus, message = http.StatusBadRequest, status.Convert(err).Message()
	case codes.NotFound:
		httpStatus, message = http.StatusNotFound, "product not found"
	case codes.AlreadyExists:
		httpStatus, message = http.StatusConflict, "duplicate cash flow"
	case codes.DeadlineExceeded:
		httpStatus, message = http.StatusGatewayTimeout, "report request timed out"
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
	}
	writeJSON(writer, httpStatus, map[string]string{"error": message})
}
