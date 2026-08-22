package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/account/portfolio"
)

type memoryTradeFillStore struct {
	mu     sync.Mutex
	cursor time.Time
	fills  []portfolio.TradeFill
}

func (s *memoryTradeFillStore) LoadCursor(context.Context, int64) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, nil
}

func (s *memoryTradeFillStore) SaveFills(
	_ context.Context,
	_ int64,
	_ string,
	fills []portfolio.TradeFill,
	through time.Time,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fills = append(s.fills, fills...)
	s.cursor = through
	return len(fills), nil
}

func (*memoryTradeFillStore) SaveSyncError(context.Context, int64, string) error {
	return nil
}

func TestInternalReportSourceUsesServiceTokenAndExistingAdapters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/papi/v1/account":
			_, _ = fmt.Fprint(writer, `{"actualEquity":"100","totalAvailableBalance":"80"}`)
		case "/papi/v1/balance", "/papi/v1/um/positionRisk", "/papi/v1/cm/positionRisk":
			_, _ = fmt.Fprint(writer, `[]`)
		default:
			_, _ = fmt.Fprint(writer, `[{"id":1,"orderId":2,"symbol":"BTCUSDT",`+
				`"side":"BUY","price":"10","qty":"2","quoteQty":"20",`+
				`"time":1700000100000}]`)
		}
	}))
	defer server.Close()

	service, _ := newTradingTestService(t)
	store := &memoryTradeFillStore{}
	service.WithSnapshots(
		portfolio.NewRegistry(server.Client(), map[string]string{"binance": server.URL}),
		nil, nil, time.Second,
	).WithInternalReports("report-secret", store)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateTradingAccount(
		context.Background(), session.Token, CreateTradingAccountInput{
			ProductName: "arb", Exchange: "binance", AccountName: "main",
			APIKey: "key", APISecret: "secret",
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := service.GetProductAccountSnapshotsInternal(
		context.Background(), "wrong", "admin", "arb",
	); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("wrong token err=%v", err)
	}
	snapshots, err := service.GetProductAccountSnapshotsInternal(
		context.Background(), "report-secret", "admin", "arb",
	)
	if err != nil || len(snapshots.Snapshots) != 1 ||
		snapshots.Snapshots[0].AccountEquityUSD != "100" {
		t.Fatalf("snapshots=%+v err=%v", snapshots, err)
	}
	through := time.UnixMilli(1_700_003_600_000).UTC()
	results, err := service.SyncProductTradeFillsInternal(
		context.Background(), "report-secret", "admin", "arb", through,
	)
	if err != nil || len(results) != 1 || results[0].InsertedCount != 3 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if len(store.fills) != 3 || !store.cursor.Equal(through) {
		t.Fatalf("fills=%d cursor=%s", len(store.fills), store.cursor)
	}
}
