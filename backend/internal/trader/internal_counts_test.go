package trader

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubCountStore struct {
	rows []OwnerArbitrageCount
}

func (s stubCountStore) CountArbitrageCombinationsByOwner(context.Context) ([]OwnerArbitrageCount, error) {
	return s.rows, nil
}

func TestGetArbitrageCountsByOwnerInternalTokenMatrix(t *testing.T) {
	service := NewService(nil, nil, nil, nil, time.Second, nil).WithAdminInternalToken("admin-token")
	service.counts = stubCountStore{rows: []OwnerArbitrageCount{{
		OwnerUsername: "user", Running: 2, Closing: 1, Closed: 4, Failed: 3,
	}}}

	if _, err := service.GetArbitrageCountsByOwnerInternal(context.Background(), "gateway-token"); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("gateway token: %v", err)
	}
	if _, err := service.GetArbitrageCountsByOwnerInternal(context.Background(), "trader-token"); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("trader token: %v", err)
	}
	if _, err := service.GetArbitrageCountsByOwnerInternal(context.Background(), ""); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("empty token: %v", err)
	}
	rows, err := service.GetArbitrageCountsByOwnerInternal(context.Background(), "admin-token")
	if err != nil || len(rows) != 1 || rows[0].Running != 2 {
		t.Fatalf("admin token: %+v err=%v", rows, err)
	}
}
