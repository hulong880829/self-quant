package polymarket

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDoWithRetryOnlyRetriesConfiguredIdempotentStatuses(t *testing.T) {
	for _, retryStatus := range []int{
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
	} {
		t.Run(http.StatusText(retryStatus), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests++
				if requests == 1 {
					writer.Header().Set("Retry-After", "0")
					writer.WriteHeader(retryStatus)
					return
				}
				_, _ = io.WriteString(writer, "ok")
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			response, err := doWithRetry(ctx, server.Client(), func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			}, 2)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if requests != 2 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}

func TestDoWithRetryDoesNotRetryOther4xx(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doWithRetry(
		context.Background(), server.Client(),
		func() (*http.Request, error) { return request.Clone(context.Background()), nil },
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}
