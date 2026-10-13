package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	accountv1 "selfquant/backend/gen/account/v1"
)

func (h *Handler) userSession(writer http.ResponseWriter, request *http.Request) (string, bool) {
	cookie, hasCookie := h.sessionToken(request)
	bearer := bearerToken(request)
	if hasCookie && bearer != "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "cookie and bearer cannot be used together",
		})
		return "", false
	}
	if bearer != "" {
		return h.brokerUserSession(writer, request, bearer)
	}
	if !hasCookie {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "authentication required",
		})
		return "", false
	}
	return cookie, true
}

func (h *Handler) brokerUserSession(writer http.ResponseWriter, request *http.Request, bearer string) (string, bool) {
	if h.brokerPublicKey == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "broker auth unavailable"})
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
	accountName := strings.TrimSpace(request.Header.Get("X-SelfQuant-Account"))
	if accountName == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "X-SelfQuant-Account is required"})
		return "", false
	}
	key := claims.BrokerID + "\x00" + accountName
	h.delegatedMu.RLock()
	if entry, ok := h.delegated[key]; ok && time.Now().Before(entry.expires) {
		token := entry.token
		h.delegatedMu.RUnlock()
		return token, true
	}
	h.delegatedMu.RUnlock()
	delegated, err := h.account.IssueBrokerDelegatedSessionInternal(ctx, &accountv1.IssueBrokerDelegatedSessionInternalRequest{
		ServiceToken:    h.gatewayInternalToken,
		BrokerId:        claims.BrokerID,
		TargetUsername:  accountName,
		ActorId:         claims.BrokerID,
	})
	if err != nil {
		h.writeAccountError(writer, err, "delegated session unavailable")
		return "", false
	}
	token := delegated.GetSessionToken()
	h.delegatedMu.Lock()
	if h.delegated == nil {
		h.delegated = map[string]delegatedSession{}
	}
	h.delegated[key] = delegatedSession{token: token, expires: time.Now().Add(45 * time.Second)}
	h.delegatedMu.Unlock()
	return token, true
}

func bearerToken(request *http.Request) string {
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
