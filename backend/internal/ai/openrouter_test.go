package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenRouterValidateAndStream(t *testing.T) {
	var keyChecked bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization=%q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/api/v1/key":
			keyChecked = true
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":{"label":"test"}}`))
		case "/api/v1/chat/completions":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte(
				": keepalive\n\n" +
					"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
					"data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n" +
					"data: [DONE]\n\n",
			))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	provider := NewOpenRouterProvider(
		server.Client(), server.URL+"/api/v1", "openrouter/free", "https://example.test",
		"Self Quant Test", 200,
	)
	if err := provider.ValidateKey(context.Background(), "test-key"); err != nil {
		t.Fatal(err)
	}
	if !keyChecked {
		t.Fatal("key endpoint was not called")
	}
	var events []StreamEvent
	err := provider.StreamChat(
		context.Background(),
		"test-key",
		ChatRequest{
			ModelAlias: ModelFreeGeneral,
			Messages:   []Message{{Role: "user", Content: "hello"}},
		},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Delta != "hello" ||
		events[1].InputTokens != 2 || events[2].Type != "done" {
		t.Fatalf("events=%+v", events)
	}
}

func TestOpenRouterMapsCredentialAndRateLimitErrors(t *testing.T) {
	for _, test := range []struct {
		status int
		target error
	}{
		{status: http.StatusUnauthorized, target: ErrCredentialInvalid},
		{status: http.StatusTooManyRequests, target: ErrRateLimited},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(`{"error":{"message":"request rejected"}}`))
			}))
			defer server.Close()
			provider := NewOpenRouterProvider(
				server.Client(), server.URL, "openrouter/free", "", "", 100,
			)
			err := provider.ValidateKey(context.Background(), "bad-key")
			if !errors.Is(err, test.target) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestOpenRouterStreamHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	provider := NewOpenRouterProvider(
		&http.Client{Timeout: time.Second}, server.URL, "openrouter/free", "", "", 100,
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := provider.StreamChat(
		ctx, "key",
		ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}},
		func(StreamEvent) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenRouterCompleteChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":" condensed "}}]}`))
	}))
	defer server.Close()
	provider := NewOpenRouterProvider(
		server.Client(), server.URL, "openrouter/free", "", "", 200,
	)
	result, err := provider.CompleteChat(
		context.Background(), "key",
		ChatRequest{
			ModelAlias: ModelFreeGeneral, MaxOutputTokens: 50,
			Messages: []Message{{Role: "user", Content: "summarize"}},
		},
	)
	if err != nil || result != "condensed" {
		t.Fatalf("result=%q err=%v", result, err)
	}
}
