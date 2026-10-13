package trader

import (
	"context"
	"errors"
	"testing"
	"time"
)

type historyCaptureStore struct {
	arbitrageStore
	owner string
}

func (s *historyCaptureStore) GetArbitrageCombinationByOwner(
	_ context.Context, owner, id string,
) (ArbitrageCombination, error) {
	if owner != "admin" || id == "" {
		return ArbitrageCombination{}, ErrNotFound
	}
	return ArbitrageCombination{ID: id, OwnerUsername: owner}, nil
}

func (s *historyCaptureStore) ListArbitrageOrderHistory(
	_ context.Context, owner, _ string, _ ArbitrageHistoryQuery,
) ([]Order, string, error) {
	s.owner = owner
	return []Order{{ID: "order-1"}}, "next", nil
}

func (s *historyCaptureStore) ListArbitrageExecutionHistory(
	context.Context, string, ArbitrageHistoryQuery,
) ([]ArbitrageExecution, string, error) {
	return nil, "", nil
}

func (s *historyCaptureStore) ListArbitrageEventHistory(
	context.Context, string, ArbitrageHistoryQuery,
) ([]ArbitrageEvent, string, error) {
	return nil, "", nil
}

func TestHistoryCursorRoundTripAndBounds(t *testing.T) {
	createdAt := time.Date(2026, 10, 13, 1, 2, 3, 4, time.UTC)
	cursor := encodeHistoryCursor(createdAt, "7f928919-1f36-45e9-9e78-7834465b220b")
	query, decoded, err := normalizeHistoryQuery(ArbitrageHistoryQuery{
		Limit: 100, Cursor: cursor,
	})
	if err != nil || query.Limit != 100 || !decoded.CreatedAt.Equal(createdAt) ||
		decoded.ID != "7f928919-1f36-45e9-9e78-7834465b220b" {
		t.Fatalf("query=%+v cursor=%+v err=%v", query, decoded, err)
	}
	if _, _, err := normalizeHistoryQuery(ArbitrageHistoryQuery{Limit: 101}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("limit err=%v", err)
	}
	if _, _, err := normalizeHistoryQuery(ArbitrageHistoryQuery{Cursor: "not-base64"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cursor err=%v", err)
	}
}

func TestEventPayloadFallbackDoesNotFailRow(t *testing.T) {
	scanner := func(dest ...any) error {
		*dest[0].(*string) = "1"
		*dest[1].(*string) = "7f928919-1f36-45e9-9e78-7834465b220b"
		*dest[2].(*string) = ""
		*dest[3].(*string) = "legacy"
		*dest[4].(*string) = `["legacy-array"]`
		*dest[5].(*time.Time) = time.Now().UTC()
		return nil
	}
	event, err := scanArbitrageEvent(scanner)
	if err != nil {
		t.Fatal(err)
	}
	if event.PayloadObject || event.Payload != nil || event.Message != `["legacy-array"]` {
		t.Fatalf("event=%+v", event)
	}
}

func TestHistoryServiceEnforcesOwnerBeforeQuery(t *testing.T) {
	store := &historyCaptureStore{}
	service := &Service{
		arbitrage:   store,
		credentials: arbitrageServiceCredentials{owner: "admin"},
	}
	items, next, err := service.ListArbitrageCombinationOrders(
		context.Background(), "token", "7f928919-1f36-45e9-9e78-7834465b220b",
		ArbitrageHistoryQuery{Limit: 1},
	)
	if err != nil || len(items) != 1 || next != "next" || store.owner != "admin" {
		t.Fatalf("items=%+v next=%q owner=%q err=%v", items, next, store.owner, err)
	}
}
