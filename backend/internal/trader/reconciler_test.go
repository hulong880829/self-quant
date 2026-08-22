package trader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

type reconcileMemoryStore struct {
	orders  []Order
	updates int
}

func (s *reconcileMemoryStore) LeaseDueOrders(context.Context, int, time.Duration) ([]Order, error) {
	return s.orders, nil
}
func (s *reconcileMemoryStore) MarkReconcileFailure(context.Context, string, time.Time) error {
	return nil
}
func (s *reconcileMemoryStore) DeferReconcile(context.Context, string, time.Time) error {
	return nil
}
func (s *reconcileMemoryStore) UpdateResult(context.Context, string, VenueResult) (Order, error) {
	s.updates++
	return Order{}, nil
}
func (s *reconcileMemoryStore) AppendEvent(context.Context, string, string, map[string]any) error {
	return nil
}

type reconcileCredentials struct {
	mu    sync.Mutex
	calls int
}

func (p *reconcileCredentials) GetInternal(context.Context, string, string, int64) (Credentials, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return Credentials{Exchange: "binance", APIKey: "key", APISecret: "secret"}, nil
}

type reconcileCatalog struct{ instrument Instrument }

func (c reconcileCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return []Instrument{c.instrument}, nil
}
func (c reconcileCatalog) Get(context.Context, int64) (Instrument, error) {
	return c.instrument, nil
}

func TestReconcilerReusesCredentialsPerAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"orderId":11,"status":"FILLED","executedQty":"1","avgPrice":"100"}`))
	}))
	defer server.Close()
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	store := &reconcileMemoryStore{orders: []Order{
		{ID: "one", OwnerUsername: "alice", TradingAccountID: 3, Exchange: "binance", InstrumentID: 7, Quantity: "1"},
		{ID: "two", OwnerUsername: "alice", TradingAccountID: 3, Exchange: "binance", InstrumentID: 7, Quantity: "1"},
	}}
	provider := &reconcileCredentials{}
	registry := exchange.NewRegistry(server.Client(), map[string]string{"binance": server.URL})
	reconciler := NewReconciler(
		store, reconcileCatalog{instrument}, provider, registry, "token",
		time.Second, time.Second, 20, 2, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconciler.runOnce(context.Background())
	if provider.calls != 1 {
		t.Fatalf("credential calls=%d", provider.calls)
	}
	if store.updates != 2 {
		t.Fatalf("updates=%d", store.updates)
	}
}
