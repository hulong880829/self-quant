package admin

import (
	"context"
	"errors"
	"testing"
)

func TestOverviewRequiresGatewayToken(t *testing.T) {
	service := NewService(nil, nil, "gateway-token", "admin-token")
	if _, err := service.Overview(context.Background(), "admin-token"); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("admin token must not call overview: %v", err)
	}
	if _, err := service.Overview(context.Background(), ""); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestIssueAccessTokenRequiresGatewayToken(t *testing.T) {
	service := NewService(nil, nil, "gateway-token", "admin-token")
	if _, _, err := service.IssueAccessToken(context.Background(), "admin-token", "brk", "secret"); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("admin token must not issue oauth: %v", err)
	}
}
