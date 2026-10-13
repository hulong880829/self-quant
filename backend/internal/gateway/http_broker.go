package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	accountv1 "selfquant/backend/gen/account/v1"
	adminv1 "selfquant/backend/gen/admin/v1"
)

func (h *Handler) requireAdminService(writer http.ResponseWriter) bool {
	if h.admin == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "admin service unavailable"})
		return false
	}
	return true
}

func (h *Handler) adminOverview(writer http.ResponseWriter, request *http.Request) {
	if !h.requireAdminService(writer) {
		return
	}
	actor, ok := h.requireAdminSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
	defer cancel()
	response, err := h.admin.GetOverviewInternal(ctx, &adminv1.AdminActorRequest{
		ServiceToken: h.gatewayInternalToken, ActorUsername: actor,
	})
	if err != nil {
		h.writeAdminError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"platform": overviewJSON(response.GetPlatform()),
		"brokers":  overviewListJSON(response.GetBrokers()),
	})
}

func (h *Handler) listAdminBrokers(writer http.ResponseWriter, request *http.Request) {
	if !h.requireAdminService(writer) {
		return
	}
	actor, ok := h.requireAdminSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.admin.ListBrokersInternal(ctx, &adminv1.AdminActorRequest{
		ServiceToken: h.gatewayInternalToken, ActorUsername: actor,
	})
	if err != nil {
		h.writeAdminError(writer, err)
		return
	}
	items := make([]map[string]any, 0, len(response.GetBrokers()))
	for _, item := range response.GetBrokers() {
		items = append(items, brokerJSON(item, ""))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"brokers": items})
}

func (h *Handler) createAdminBroker(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminWrite(writer, request)
	if !ok || !h.requireAdminService(writer) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(writer, request, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.admin.CreateBrokerInternal(ctx, &adminv1.CreateBrokerInternalRequest{
		ServiceToken: h.gatewayInternalToken, ActorUsername: actor, Name: strings.TrimSpace(body.Name),
	})
	if err != nil {
		h.writeAdminError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, brokerJSON(response.GetBroker(), response.GetClientSecret()))
}

func (h *Handler) disableAdminBroker(writer http.ResponseWriter, request *http.Request) {
	h.mutateAdminBroker(writer, request)
}

func (h *Handler) restoreAdminBroker(writer http.ResponseWriter, request *http.Request) {
	h.mutateAdminBroker(writer, request)
}

func (h *Handler) deleteAdminBroker(writer http.ResponseWriter, request *http.Request) {
	h.mutateAdminBroker(writer, request)
}

func (h *Handler) mutateAdminBroker(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminWrite(writer, request)
	if !ok || !h.requireAdminService(writer) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.adminDisableRestoreDelete(ctx, request, actor)
	if err != nil {
		h.writeAdminError(writer, err)
		return
	}
	h.invalidateBrokerState(chi.URLParam(request, "brokerId"))
	writeJSON(writer, http.StatusOK, brokerJSON(response.GetBroker(), ""))
}

func (h *Handler) adminDisableRestoreDelete(
	ctx context.Context, request *http.Request, actor string,
) (*adminv1.BrokerResponse, error) {
	req := &adminv1.GetBrokerInternalRequest{
		ServiceToken: h.gatewayInternalToken, ActorUsername: actor, BrokerId: chi.URLParam(request, "brokerId"),
	}
	switch {
	case strings.HasSuffix(request.URL.Path, "/disable"):
		return h.admin.DisableBrokerInternal(ctx, req)
	case strings.HasSuffix(request.URL.Path, "/restore"):
		return h.admin.RestoreBrokerInternal(ctx, req)
	default:
		return h.admin.DeleteBrokerInternal(ctx, req)
	}
}

func (h *Handler) resetAdminBrokerSecret(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminWrite(writer, request)
	if !ok || !h.requireAdminService(writer) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.admin.ResetBrokerSecretInternal(ctx, &adminv1.GetBrokerInternalRequest{
		ServiceToken: h.gatewayInternalToken, ActorUsername: actor, BrokerId: chi.URLParam(request, "brokerId"),
	})
	if err != nil {
		h.writeAdminError(writer, err)
		return
	}
	h.invalidateBrokerState(chi.URLParam(request, "brokerId"))
	writeJSON(writer, http.StatusOK, brokerJSON(response.GetBroker(), response.GetClientSecret()))
}

func (h *Handler) requireBroker(writer http.ResponseWriter, request *http.Request) (string, bool) {
	if request.Header.Get("Cookie") != "" && bearerToken(request) != "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "cookie and bearer cannot be used together"})
		return "", false
	}
	bearer := bearerToken(request)
	if bearer == "" || h.brokerPublicKey == nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "bearer token required"})
		return "", false
	}
	claims, err := parseBrokerAccessToken(bearer, h.brokerPublicKey)
	if err != nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid access token"})
		return "", false
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	state, err := h.lookupBrokerState(ctx, claims.BrokerID)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "admin service unavailable"})
		return "", false
	}
	if state.status != "active" || state.tokenVersion != claims.TokenVersion {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid access token"})
		return "", false
	}
	return claims.BrokerID, true
}

func (h *Handler) brokerSummary(writer http.ResponseWriter, request *http.Request) {
	brokerID, ok := h.requireBroker(writer, request)
	if !ok || !h.requireAdminService(writer) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 4*time.Second)
	defer cancel()
	response, err := h.admin.GetOverviewInternal(ctx, &adminv1.AdminActorRequest{
		ServiceToken: h.gatewayInternalToken, ActorUsername: brokerID,
	})
	if err != nil {
		h.writeAdminError(writer, err)
		return
	}
	for _, row := range response.GetBrokers() {
		if row.GetBrokerId() == brokerID {
			writeJSON(writer, http.StatusOK, overviewJSON(row))
			return
		}
	}
	writeJSON(writer, http.StatusOK, overviewJSON(nil))
}

func (h *Handler) listBrokerPlatformAccounts(writer http.ResponseWriter, request *http.Request) {
	brokerID, ok := h.requireBroker(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.account.ListPlatformAccountsInternal(ctx, &accountv1.ListPlatformAccountsInternalRequest{
		ServiceToken: h.gatewayInternalToken, BrokerId: brokerID, ActorId: brokerID,
	})
	if err != nil {
		h.writePlatformAccountError(writer, err)
		return
	}
	items := make([]map[string]any, 0, len(response.GetAccounts()))
	for _, item := range response.GetAccounts() {
		items = append(items, platformAccountJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"accounts": items})
}

func (h *Handler) createBrokerPlatformAccount(writer http.ResponseWriter, request *http.Request) {
	brokerID, ok := h.requireBroker(writer, request)
	if !ok {
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		BrokerID string `json:"brokerId"`
	}
	if !decodeJSONBody(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.BrokerID) != "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "brokerId cannot be set by client"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.account.CreatePlatformAccountInternal(ctx, &accountv1.CreatePlatformAccountInternalRequest{
		ServiceToken: h.gatewayInternalToken, ActorId: brokerID, BrokerId: brokerID,
		Username: strings.TrimSpace(body.Username), Password: body.Password, CreatedSource: "broker",
	})
	if err != nil {
		h.writePlatformAccountError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, platformAccountJSON(response.GetAccount()))
}

func (h *Handler) writeAdminError(writer http.ResponseWriter, err error) {
	httpStatus := http.StatusBadGateway
	message := "admin service unavailable"
	switch status.Code(err) {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
		message = "authentication required"
	case codes.NotFound:
		httpStatus = http.StatusNotFound
		message = "not found"
	case codes.FailedPrecondition:
		httpStatus = http.StatusConflict
		message = status.Convert(err).Message()
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
	}
	writeJSON(writer, httpStatus, map[string]string{"error": message})
}

func brokerJSON(item *adminv1.Broker, secret string) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	payload := map[string]any{
		"brokerId":           item.GetBrokerId(),
		"name":               item.GetName(),
		"clientSecretMasked": item.GetClientSecretMasked(),
		"status":             item.GetStatus(),
		"tokenVersion":       item.GetTokenVersion(),
		"createdAt":          protoTimeJSON(item.GetCreatedAt()),
		"updatedAt":          protoTimeJSON(item.GetUpdatedAt()),
	}
	if secret != "" {
		payload["clientSecret"] = secret
	}
	return payload
}

func overviewJSON(item *adminv1.OverviewBrokerRow) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"brokerId":          item.GetBrokerId(),
		"name":              item.GetName(),
		"status":            item.GetStatus(),
		"platformAccounts":  item.GetPlatformAccounts(),
		"running":           item.GetRunning(),
		"closing":           item.GetClosing(),
		"closed":            item.GetClosed(),
		"failed":            item.GetFailed(),
		"equityUsd":         item.GetEquityUsd(),
		"coveredAccounts":   item.GetCoveredAccounts(),
		"failedSnapshots":   item.GetFailedSnapshots(),
		"staleSnapshots":    item.GetStaleSnapshots(),
	}
}

func overviewListJSON(items []*adminv1.OverviewBrokerRow) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, overviewJSON(item))
	}
	return out
}
