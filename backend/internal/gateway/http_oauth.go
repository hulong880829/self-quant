package gateway

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	adminv1 "selfquant/backend/gen/admin/v1"
)

func (h *Handler) oauthToken(writer http.ResponseWriter, request *http.Request) {
	if h.admin == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "admin service unavailable"})
		return
	}
	if err := request.ParseForm(); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid token request"})
		return
	}
	if strings.TrimSpace(request.Form.Get("grant_type")) != "client_credentials" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported grant_type"})
		return
	}
	clientID, clientSecret, ok := basicClientCredentials(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid client credentials"})
		return
	}
	key := request.RemoteAddr + "\x00" + clientID
	if h.oauthLimiter != nil && !h.oauthLimiter.allow(key) {
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "too many token requests"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.admin.IssueBrokerAccessTokenInternal(ctx, &adminv1.IssueBrokerAccessTokenInternalRequest{
		ServiceToken: h.gatewayInternalToken,
		ClientId:     clientID,
		ClientSecret: clientSecret,
	})
	if err != nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid client credentials"})
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writeJSON(writer, http.StatusOK, map[string]any{
		"access_token": response.GetAccessToken(),
		"token_type":   response.GetTokenType(),
		"expires_in":   response.GetExpiresIn(),
	})
}

func basicClientCredentials(request *http.Request) (string, string, bool) {
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || parts[1] == "" {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), parts[1], true
}
