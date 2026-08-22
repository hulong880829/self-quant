package account

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfquant/backend/internal/account/portfolio"
)

func TestSnapshotCacheSingleflightAndFailureFallback(t *testing.T) {
	var accountCalls atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/papi/v1/account" {
			accountCalls.Add(1)
			if fail.Load() {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			_, _ = fmt.Fprint(w, `{"actualEquity":"100","totalAvailableBalance":"80","accountMaintMargin":"5"}`)
			return
		}
		if r.URL.Path == "/papi/v1/um/positionRisk" {
			_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"1","entryPrice":"10","markPrice":"12","unRealizedProfit":"2","notional":"12"}]`)
			return
		}
		if r.URL.Path == "/papi/v1/balance" {
			_, _ = fmt.Fprint(w, `[{"asset":"BTC","crossMarginAsset":"2","crossMarginBorrowed":"0.5"}]`)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	service, _ := newTradingTestService(t)
	service.WithSnapshots(portfolio.NewRegistry(server.Client(), map[string]string{"binance": server.URL}), nil, nil, 20*time.Millisecond)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "arb", Exchange: "binance", AccountName: "main", APIKey: "key", APISecret: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.GetTradingAccountSnapshot(context.Background(), session.Token, created.ID)
	if err != nil || first.Stale || len(first.Positions) != 1 ||
		first.AccountEquityUSD != "100" || first.AvailableFundsUSD != "80" ||
		first.Positions[0].SpotSize != "1.5" || first.Positions[0].SignedContractSize != "1" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = service.GetTradingAccountSnapshot(context.Background(), session.Token, created.ID); err != nil {
		t.Fatal(err)
	}
	if accountCalls.Load() != 1 {
		t.Fatalf("cache miss: calls=%d", accountCalls.Load())
	}
	if _, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "arb", Exchange: "hyperliquid", AccountName: "unsupported", APIKey: "key", APISecret: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	group, err := service.GetProductGroupSnapshot(context.Background(), session.Token, "arb")
	if err != nil {
		t.Fatal(err)
	}
	if !group.Partial || group.AccountEquityUSD != "100" || group.AvailableFundsUSD != "80" {
		t.Fatalf("group=%+v", group)
	}
	if len(group.Positions) != 1 || group.Positions[0].SpotSize != "1.5" ||
		group.Positions[0].ContractSize != "1" {
		t.Fatalf("group positions=%+v", group.Positions)
	}

	time.Sleep(25 * time.Millisecond)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, snapshotErr := service.GetTradingAccountSnapshot(context.Background(), session.Token, created.ID); snapshotErr != nil {
				t.Error(snapshotErr)
			}
		}()
	}
	wait.Wait()
	if accountCalls.Load() != 2 {
		t.Fatalf("singleflight failed: calls=%d", accountCalls.Load())
	}

	time.Sleep(25 * time.Millisecond)
	fail.Store(true)
	fallback, err := service.GetTradingAccountSnapshot(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fallback.Stale || fallback.LastError == "" || len(fallback.Positions) != 1 ||
		fallback.AccountEquityUSD != "100" || fallback.AvailableFundsUSD != "80" {
		t.Fatalf("fallback=%+v", fallback)
	}
}

func TestProductGroupAggregatesSignedSpotAndContractsOncePerAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-MBX-APIKEY")
		switch r.URL.Path {
		case "/papi/v1/account":
			_, _ = fmt.Fprint(w, `{"actualEquity":"100","totalAvailableBalance":"80"}`)
		case "/papi/v1/balance":
			if key == "key-a" {
				_, _ = fmt.Fprint(w, `[{"asset":"BTC","crossMarginAsset":"2","crossMarginBorrowed":"0.5"}]`)
			} else {
				_, _ = fmt.Fprint(w, `[{"asset":"BTC","crossMarginAsset":"0.5","crossMarginBorrowed":"1"}]`)
			}
		case "/papi/v1/um/positionRisk":
			if key == "key-a" {
				_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"2","entryPrice":"10","markPrice":"12","unRealizedProfit":"4","notional":"24"}]`)
			} else {
				_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSDT","positionAmt":"-0.5","entryPrice":"10","markPrice":"12","unRealizedProfit":"-1","notional":"-6"}]`)
			}
		case "/papi/v1/cm/positionRisk":
			if key == "key-a" {
				_, _ = fmt.Fprint(w, `[{"symbol":"BTCUSD_PERP","positionAmt":"-1","entryPrice":"10","markPrice":"12","unRealizedProfit":"-2","notional":"-12"}]`)
			} else {
				_, _ = fmt.Fprint(w, `[]`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service, _ := newTradingTestService(t)
	service.WithSnapshots(portfolio.NewRegistry(server.Client(), map[string]string{"binance": server.URL}), nil, nil, time.Minute)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []CreateTradingAccountInput{
		{ProductName: "signed", Exchange: "binance", AccountName: "a", APIKey: "key-a", APISecret: "secret"},
		{ProductName: "signed", Exchange: "binance", AccountName: "b", APIKey: "key-b", APISecret: "secret"},
	} {
		if _, err := service.CreateTradingAccount(context.Background(), session.Token, input); err != nil {
			t.Fatal(err)
		}
	}
	group, err := service.GetProductGroupSnapshot(context.Background(), session.Token, "signed")
	if err != nil {
		t.Fatal(err)
	}
	if len(group.Positions) != 1 {
		t.Fatalf("positions=%+v", group.Positions)
	}
	position := group.Positions[0]
	if position.Symbol != "BTC" || position.SpotSize != "1" ||
		position.ContractSize != "0.5" || position.TotalNotionalUSD != "42" {
		t.Fatalf("position=%+v", position)
	}
}

func TestProductGroupPairsOKXXStockSpotWithPerpetual(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v5/account/balance":
				_, _ = fmt.Fprint(w, `{"code":"0","data":[{"totalEq":"100",`+
					`"availEq":"80","details":[{"ccy":"XGOOGL",`+
					`"cashBal":"0.58","liab":"0"}]}]}`)
			case "/api/v5/account/positions":
				_, _ = fmt.Fprint(w, `{"code":"0","data":[{`+
					`"instId":"GOOGL-USDT-SWAP","pos":"0.58",`+
					`"posSide":"short","avgPx":"346.4779","markPx":"346.77",`+
					`"upl":"-0.1694","notionalUsd":"201.1266","mmr":"1"}]}`)
			default:
				http.NotFound(w, r)
			}
		},
	))
	defer server.Close()

	service, _ := newTradingTestService(t)
	service.WithSnapshots(
		portfolio.NewRegistry(server.Client(), map[string]string{"okx": server.URL}),
		nil,
		nil,
		time.Minute,
	)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateTradingAccount(
		context.Background(),
		session.Token,
		CreateTradingAccountInput{
			ProductName: "xstock",
			Exchange:    "okx",
			AccountName: "main",
			APIKey:      "key",
			APISecret:   "secret",
			Passphrase:  "pass",
		},
	); err != nil {
		t.Fatal(err)
	}

	group, err := service.GetProductGroupSnapshot(
		context.Background(), session.Token, "xstock",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.Positions) != 1 {
		t.Fatalf("positions=%+v", group.Positions)
	}
	position := group.Positions[0]
	if position.Symbol != "GOOGL" || position.SpotSize != "0.58" ||
		position.ContractSize != "-0.58" {
		t.Fatalf("position=%+v", position)
	}
}
