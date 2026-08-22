package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPolymarketErrorMappingPreservesActionablePrivateErrors(t *testing.T) {
	tests := []struct {
		name, message string
		grpcCode      codes.Code
		httpCode      int
	}{
		{"credentials", "polymarket credentials invalid; please rebind", codes.Unauthenticated, http.StatusUnauthorized},
		{"rate-limit", "polymarket upstream rate limited", codes.ResourceExhausted, http.StatusTooManyRequests},
		{"timeout", "polymarket upstream timed out", codes.DeadlineExceeded, http.StatusGatewayTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&Handler{}).writePolymarketError(
				recorder, status.Error(test.grpcCode, test.message),
			)
			if recorder.Code != test.httpCode {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if test.name != "timeout" && !strings.Contains(recorder.Body.String(), test.message) {
				t.Fatalf("body=%s", recorder.Body.String())
			}
		})
	}
}
