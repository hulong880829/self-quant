package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	accountv1 "selfquant/backend/gen/account/v1"
	aiv1 "selfquant/backend/gen/ai/v1"
)

const aiProviderOpenRouter = "openrouter"

type aiCredentialBody struct {
	APIKey string `json:"apiKey"`
}

func (h *Handler) getAICredential(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	response, err := h.ai.GetCredentialStatus(ctx, &aiv1.GetCredentialStatusRequest{
		Token: token, Provider: aiProviderOpenRouter,
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": aiCredentialJSON(response.GetCredential()),
	})
}

func (h *Handler) upsertAICredential(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 16<<10)
	var body aiCredentialBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.APIKey) == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "api key is required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	response, err := h.account.UpsertAICredential(ctx, &accountv1.UpsertAICredentialRequest{
		Token: token, Provider: aiProviderOpenRouter, ApiKey: body.APIKey,
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": accountAICredentialJSON(response.GetCredential()),
	})
}

func (h *Handler) deleteAICredential(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	if _, err := h.account.DeleteAICredential(ctx, &accountv1.DeleteAICredentialRequest{
		Token: token, Provider: aiProviderOpenRouter,
	}); err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (h *Handler) testAICredential(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	response, err := h.ai.TestCredential(ctx, &aiv1.TestCredentialRequest{
		Token: token, Provider: aiProviderOpenRouter,
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": aiCredentialJSON(response.GetCredential()),
	})
}

type aiChatBody struct {
	ConversationID  string `json:"conversationId"`
	Prompt          string `json:"prompt"`
	ClientMessageID string `json:"clientMessageId"`
	ModelAlias      string `json:"modelAlias"`
}

type aiStreamResult struct {
	event *aiv1.StreamChatEvent
	err   error
}

func (h *Handler) streamAIChat(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"error": "stream unsupported"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	var body aiChatBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil ||
		strings.TrimSpace(body.ConversationID) == "" ||
		strings.TrimSpace(body.Prompt) == "" ||
		strings.TrimSpace(body.ClientMessageID) == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid chat payload"})
		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	controller := http.NewResponseController(writer)
	stream, err := h.ai.StreamChat(request.Context(), &aiv1.StreamChatRequest{
		Token: token, ModelAlias: body.ModelAlias,
		ConversationId: body.ConversationID, Prompt: body.Prompt,
		ClientMessageId: body.ClientMessageID,
	})
	if err != nil {
		_ = writeSSEEvent(
			writer, flusher, controller, "error",
			map[string]any{"error": aiPublicError(err)},
		)
		return
	}
	results := make(chan aiStreamResult, 1)
	go func() {
		for {
			event, receiveErr := stream.Recv()
			select {
			case results <- aiStreamResult{event: event, err: receiveErr}:
			case <-request.Context().Done():
				return
			}
			if receiveErr != nil {
				return
			}
		}
	}()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	sawDone := false
	for {
		select {
		case <-request.Context().Done():
			return
		case <-heartbeat.C:
			if !writeSSEComment(writer, flusher, controller) {
				return
			}
		case result := <-results:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) || sawDone ||
					status.Code(result.err) == codes.Canceled {
					return
				}
				_ = writeSSEEvent(
					writer, flusher, controller, "error",
					map[string]any{"error": aiPublicError(result.err)},
				)
				return
			}
			eventType := strings.TrimSpace(result.event.GetType())
			if eventType == "" {
				eventType = "delta"
			}
			if !writeSSEEvent(writer, flusher, controller, eventType, map[string]any{
				"delta": result.event.GetDelta(), "tool": result.event.GetTool(),
				"error": result.event.GetError(), "inputTokens": result.event.GetInputTokens(),
				"outputTokens":       result.event.GetOutputTokens(),
				"conversationId":     result.event.GetConversationId(),
				"userMessageId":      result.event.GetUserMessageId(),
				"assistantMessageId": result.event.GetAssistantMessageId(),
				"title":              result.event.GetTitle(), "status": result.event.GetStatus(),
			}) {
				return
			}
			sawDone = eventType == "done"
		}
	}
}

type aiConversationBody struct {
	ModelAlias string `json:"modelAlias"`
}

type aiConversationRenameBody struct {
	Title string `json:"title"`
}

func (h *Handler) listAIConversations(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	limit := queryLimit(request, 20, 50)
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	response, err := h.ai.ListConversations(ctx, &aiv1.ListConversationsRequest{
		Token: token, Limit: int32(limit),
		Cursor: strings.TrimSpace(request.URL.Query().Get("cursor")),
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	items := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		items = append(items, aiConversationJSON(item))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": items, "nextCursor": response.GetNextCursor(),
	})
}

func (h *Handler) createAIConversation(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	var body aiConversationBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid conversation payload"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	response, err := h.ai.CreateConversation(ctx, &aiv1.CreateConversationRequest{
		Token: token, ModelAlias: body.ModelAlias,
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusCreated, map[string]any{
		"data": aiConversationJSON(response.GetConversation()),
	})
}

func (h *Handler) renameAIConversation(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	var body aiConversationRenameBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	response, err := h.ai.RenameConversation(ctx, &aiv1.RenameConversationRequest{
		Token: token, ConversationId: chi.URLParam(request, "id"), Title: body.Title,
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": aiConversationJSON(response.GetConversation()),
	})
}

func (h *Handler) deleteAIConversation(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	if _, err := h.ai.DeleteConversation(ctx, &aiv1.DeleteConversationRequest{
		Token: token, ConversationId: chi.URLParam(request, "id"),
	}); err != nil {
		h.writeAIError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listAIConversationMessages(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.aiSession(writer, request)
	if !ok {
		return
	}
	limit := queryLimit(request, 30, 50)
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	response, err := h.ai.ListMessages(ctx, &aiv1.ListMessagesRequest{
		Token: token, ConversationId: chi.URLParam(request, "id"),
		Limit: int32(limit), Cursor: strings.TrimSpace(request.URL.Query().Get("cursor")),
	})
	if err != nil {
		h.writeAIError(writer, err)
		return
	}
	items := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		items = append(items, aiConversationMessageJSON(item))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": items, "nextCursor": response.GetNextCursor(),
	})
}

func queryLimit(request *http.Request, fallback, maximum int) int {
	value, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil || value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func aiConversationJSON(item *aiv1.Conversation) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id": item.GetId(), "title": item.GetTitle(),
		"lastMessagePreview": item.GetLastMessagePreview(),
		"messageCount":       item.GetMessageCount(), "modelAlias": item.GetModelAlias(),
		"lastMessageAt": protoTimeJSON(item.GetLastMessageAt()),
		"expiresAt":     protoTimeJSON(item.GetExpiresAt()),
		"createdAt":     protoTimeJSON(item.GetCreatedAt()),
		"updatedAt":     protoTimeJSON(item.GetUpdatedAt()),
	}
}

func aiConversationMessageJSON(item *aiv1.ConversationMessage) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id": item.GetId(), "conversationId": item.GetConversationId(),
		"turnId": item.GetTurnId(), "clientMessageId": item.GetClientMessageId(),
		"role": item.GetRole(), "status": item.GetStatus(), "content": item.GetContent(),
		"tool": item.GetTool(), "inputTokens": item.GetInputTokens(),
		"outputTokens": item.GetOutputTokens(),
		"createdAt":    protoTimeJSON(item.GetCreatedAt()),
		"updatedAt":    protoTimeJSON(item.GetUpdatedAt()),
	}
}

func (h *Handler) aiSession(
	writer http.ResponseWriter,
	request *http.Request,
) (string, bool) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return "", false
	}
	return token, true
}

func aiCredentialJSON(item *aiv1.CredentialStatus) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"provider": item.GetProvider(), "apiKeyMasked": item.GetApiKeyMasked(),
		"status": item.GetStatus(), "lastError": item.GetLastError(),
		"lastTestedAt": protoTimeJSON(item.GetLastTestedAt()),
	}
}

func accountAICredentialJSON(item *accountv1.AICredential) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	return map[string]any{
		"provider": item.GetProvider(), "apiKeyMasked": item.GetApiKeyMasked(),
		"status": item.GetStatus(), "lastError": item.GetLastError(),
		"lastTestedAt": protoTimeJSON(item.GetLastTestedAt()),
	}
}

func writeSSEComment(
	writer http.ResponseWriter,
	flusher http.Flusher,
	controller *http.ResponseController,
) bool {
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(writer, ": keepalive\n\n"); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func writeSSEEvent(
	writer http.ResponseWriter,
	flusher http.Flusher,
	controller *http.ResponseController,
	eventType string,
	data any,
) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false
	}
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func (h *Handler) writeAIError(writer http.ResponseWriter, err error) {
	httpStatus := http.StatusBadGateway
	message := "ai service unavailable"
	switch status.Code(err) {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
		message = "authentication required"
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
		message = status.Convert(err).Message()
	case codes.NotFound:
		httpStatus = http.StatusNotFound
		if strings.Contains(status.Convert(err).Message(), "conversation") {
			message = "ai conversation not found"
		} else {
			message = "ai credential not configured"
		}
	case codes.AlreadyExists:
		httpStatus = http.StatusConflict
		message = "ai conversation is already streaming"
	case codes.FailedPrecondition:
		httpStatus = http.StatusUnprocessableEntity
		message = "ai credential is not valid"
	case codes.ResourceExhausted:
		detail := status.Convert(err).Message()
		switch {
		case strings.Contains(detail, "raw message limit"):
			httpStatus = http.StatusConflict
			message = "当前会话已达到 1000 条原文上限，请新建会话"
		case strings.Contains(detail, "conversation limit"):
			httpStatus = http.StatusConflict
			message = "最多保留 5 个会话，请先删除一个"
		default:
			httpStatus = http.StatusTooManyRequests
			message = "ai provider rate limited"
		}
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
		message = "ai request timed out"
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
		message = "ai provider unavailable"
	}
	writeJSON(writer, httpStatus, map[string]string{"error": message})
}

func aiPublicError(err error) string {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return "authentication required"
	case codes.FailedPrecondition:
		return "ai credential is not valid"
	case codes.ResourceExhausted:
		detail := status.Convert(err).Message()
		switch {
		case strings.Contains(detail, "raw message limit"):
			return "当前会话已达到 1000 条原文上限，请新建会话"
		case strings.Contains(detail, "conversation limit"):
			return "最多保留 5 个会话，请先删除一个"
		default:
			return "ai provider rate limited"
		}
	case codes.InvalidArgument:
		return "invalid ai request"
	case codes.NotFound:
		return "ai conversation not found"
	case codes.AlreadyExists:
		return "ai conversation is already streaming"
	default:
		return "ai stream unavailable"
	}
}
