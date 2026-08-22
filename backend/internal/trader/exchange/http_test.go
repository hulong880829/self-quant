package exchange

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignedClientClassifiesRetryAfterRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "2")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := newSignedClient(server.Client(), server.URL, time.Millisecond)
	_, err := client.do(context.Background(), http.MethodGet, "/", nil, nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v", err)
	}
	client.limiter.mu.Lock()
	delay := time.Until(client.limiter.next)
	client.limiter.mu.Unlock()
	if delay < time.Second {
		t.Fatalf("rate limit delay=%s", delay)
	}
}
