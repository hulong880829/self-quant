package exchange

import (
	"context"
	"errors"
	"io"
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

func TestSignedClientOnlyMarksMutatingAmbiguityUncertain(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	client := newSignedClient(&http.Client{Transport: transport}, "https://venue.invalid", time.Millisecond)

	_, err := client.do(context.Background(), http.MethodPost, "/order", nil, []byte(`{}`), nil)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("POST err=%v", err)
	}
	_, err = client.do(context.Background(), http.MethodGet, "/order", nil, nil, nil)
	if err == nil || errors.Is(err, ErrUncertain) {
		t.Fatalf("GET err=%v", err)
	}
}

func TestSignedClientMalformedMutatingResponseIsUncertain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"truncated"`))
	}))
	defer server.Close()
	client := newSignedClient(server.Client(), server.URL, time.Millisecond)
	var target map[string]any
	_, err := client.do(
		context.Background(), http.MethodPost, "/order", nil, []byte(`{}`), &target,
	)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
