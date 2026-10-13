package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	accountv1 "selfquant/backend/gen/account/v1"
)

func (h *Handler) requireAdminSession(writer http.ResponseWriter, request *http.Request) (string, bool) {
	if request.Header.Get("Authorization") != "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "cookie and bearer cannot be used together",
		})
		return "", false
	}
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return "", false
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	session, err := h.account.ValidateSession(ctx, &accountv1.ValidateSessionRequest{Token: token})
	if err != nil {
		h.writeAccountError(writer, err, "authentication required")
		return "", false
	}
	if session.GetPermission() != "admin" {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "admin required"})
		return "", false
	}
	return session.GetUsername(), true
}

func (h *Handler) requireAdminWrite(writer http.ResponseWriter, request *http.Request) (string, bool) {
	actor, ok := h.requireAdminSession(writer, request)
	if !ok {
		return "", false
	}
	if !h.originAllowed(request) {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "invalid origin"})
		return "", false
	}
	return actor, true
}

func (h *Handler) originAllowed(request *http.Request) bool {
	expected := strings.TrimSpace(h.corsOrigin)
	if expected == "" {
		return false
	}
	return strings.TrimSpace(request.Header.Get("Origin")) == expected
}

func (h *Handler) listAdminPlatformAccounts(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.account.ListPlatformAccountsInternal(ctx, &accountv1.ListPlatformAccountsInternalRequest{
		ServiceToken:  h.gatewayInternalToken,
		ActorUsername: actor,
		BrokerId:      strings.TrimSpace(request.URL.Query().Get("brokerId")),
	})
	if err != nil {
		h.writeAccountError(writer, err, "platform accounts unavailable")
		return
	}
	items := make([]map[string]any, 0, len(response.GetAccounts()))
	for _, item := range response.GetAccounts() {
		items = append(items, platformAccountJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"accounts": items})
}

func (h *Handler) createAdminPlatformAccount(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminWrite(writer, request)
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
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.account.CreatePlatformAccountInternal(ctx, &accountv1.CreatePlatformAccountInternalRequest{
		ServiceToken:  h.gatewayInternalToken,
		ActorUsername: actor,
		Username:      strings.TrimSpace(body.Username),
		Password:      body.Password,
		BrokerId:      strings.TrimSpace(body.BrokerID),
		CreatedSource: "selfquant",
	})
	if err != nil {
		h.writePlatformAccountError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, platformAccountJSON(response.GetAccount()))
}

func (h *Handler) disableAdminPlatformAccount(writer http.ResponseWriter, request *http.Request) {
	h.mutateAdminPlatformAccount(writer, request, h.account.DisablePlatformAccountInternal)
}

func (h *Handler) restoreAdminPlatformAccount(writer http.ResponseWriter, request *http.Request) {
	h.mutateAdminPlatformAccount(writer, request, h.account.RestorePlatformAccountInternal)
}

func (h *Handler) deleteAdminPlatformAccount(writer http.ResponseWriter, request *http.Request) {
	h.mutateAdminPlatformAccount(writer, request, h.account.DeletePlatformAccountInternal)
}

func (h *Handler) mutateAdminPlatformAccount(
	writer http.ResponseWriter,
	request *http.Request,
	call func(context.Context, *accountv1.MutatePlatformAccountInternalRequest, ...grpc.CallOption) (*accountv1.PlatformAccountResponse, error),
) {
	actor, ok := h.requireAdminWrite(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := call(ctx, &accountv1.MutatePlatformAccountInternalRequest{
		ServiceToken:  h.gatewayInternalToken,
		ActorUsername: actor,
		Username:      chi.URLParam(request, "username"),
	})
	if err != nil {
		h.writePlatformAccountError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, platformAccountJSON(response.GetAccount()))
}

func (h *Handler) resetAdminPlatformAccountPassword(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminWrite(writer, request)
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSONBody(writer, request, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.account.ResetPlatformAccountPasswordInternal(ctx, &accountv1.ResetPlatformAccountPasswordInternalRequest{
		ServiceToken:  h.gatewayInternalToken,
		ActorUsername: actor,
		Username:      chi.URLParam(request, "username"),
		Password:      body.Password,
	})
	if err != nil {
		h.writePlatformAccountError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, platformAccountJSON(response.GetAccount()))
}

func (h *Handler) updateAdminPlatformAccountBroker(writer http.ResponseWriter, request *http.Request) {
	actor, ok := h.requireAdminWrite(writer, request)
	if !ok {
		return
	}
	var body struct {
		BrokerID string `json:"brokerId"`
	}
	if !decodeJSONBody(writer, request, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.account.UpdatePlatformAccountBrokerInternal(ctx, &accountv1.UpdatePlatformAccountBrokerInternalRequest{
		ServiceToken:  h.gatewayInternalToken,
		ActorUsername: actor,
		Username:      chi.URLParam(request, "username"),
		BrokerId:      strings.TrimSpace(body.BrokerID),
	})
	if err != nil {
		h.writePlatformAccountError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, platformAccountJSON(response.GetAccount()))
}

func (h *Handler) writePlatformAccountError(writer http.ResponseWriter, err error) {
	message := "platform account operation failed"
	httpStatus := http.StatusBadGateway
	switch status.Code(err) {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
		message = "authentication required"
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
		message = status.Convert(err).Message()
	case codes.AlreadyExists:
		httpStatus = http.StatusConflict
		message = "platform account already exists"
	case codes.NotFound:
		httpStatus = http.StatusNotFound
		message = "not found"
	case codes.FailedPrecondition:
		httpStatus = http.StatusConflict
		message = status.Convert(err).Message()
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
		message = "account service timed out"
	}
	writeJSON(writer, httpStatus, map[string]string{"error": message})
}

func decodeJSONBody(writer http.ResponseWriter, request *http.Request, dest any) bool {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid request payload"})
		return false
	}
	return true
}

func platformAccountJSON(item *accountv1.PlatformAccount) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"username":      item.GetUsername(),
		"permission":    item.GetPermission(),
		"brokerId":      item.GetBrokerId(),
		"status":        item.GetStatus(),
		"createdSource": item.GetCreatedSource(),
		"createdBy":     item.GetCreatedBy(),
		"authVersion":   item.GetAuthVersion(),
		"deletedAt":     protoTimeJSON(item.GetDeletedAt()),
	}
}
