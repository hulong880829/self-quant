package polymarket

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestUserStreamRecognizesCredentialRejection(t *testing.T) {
	session := &userStreamSession{}
	err := session.applyMessage([]byte(`{"error":"Unauthorized/Invalid api key"}`))
	if !errors.Is(err, errUserStreamUnauthorized) {
		t.Fatalf("err=%v", err)
	}
}

func TestUserStreamReconcileTimeoutDoesNotTearDownHealthySocket(t *testing.T) {
	clobServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer clobServer.Close()
	upgrader := websocket.Upgrader{}
	wsServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if err := connection.WriteMessage(websocket.TextMessage, []byte("PONG")); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer wsServer.Close()

	provider := &recoveryCredentials{current: Credentials{
		SignerAddress: "0xabc", APIKey: "key",
		APISecret: "c2VjcmV0LWtleQ", Passphrase: "pass",
	}}
	service := NewService(
		nil, nil, NewCLOBClient(clobServer.URL, 20*time.Millisecond),
		NewDataClient(clobServer.URL, 20*time.Millisecond),
		nil, provider, NewSnapshotStore(), time.Second, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	service.openOrders[7] = cachedOpenOrders{
		value: []OpenOrder{{ID: "cached"}}, expiresAt: time.Now().Add(-time.Second),
	}
	service.positions[7] = cachedPositions{
		value:     []Position{{TokenID: "cached", CurrentPrice: "1"}},
		expiresAt: time.Now().Add(-time.Second),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	manager := &UserStreamManager{
		url:     strings.Replace(wsServer.URL, "http://", "ws://", 1),
		service: service,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	session := &userStreamSession{
		accountID: 7, token: "token", credentials: provider.current,
		manager: manager, ctx: ctx, subscribers: make(map[uint64]chan AccountEvent),
	}
	if err := session.runOnce(); err != nil {
		t.Fatalf("healthy stream was torn down by reconcile: %v", err)
	}
}

func TestOpenOrderFromUserEvent(t *testing.T) {
	order, live := openOrderFromUserEvent(userStreamMessage{
		EventType: "order", Type: "UPDATE", ID: "order-1", Market: "condition-1",
		AssetID: "token-1", Side: "BUY", OriginalSize: "10000000", SizeMatched: "2500000",
		Price: "0.54", Outcome: "Yes", OrderType: "GTC", Status: "LIVE",
		CreatedAt: "1786186200",
	})
	if !live {
		t.Fatal("expected live order")
	}
	if order.RemainingSize != "7.5" || order.OriginalSize != "10" ||
		order.MatchedSize != "2.5" || order.CreatedAt.Unix() != 1786186200 {
		t.Fatalf("order=%+v", order)
	}
}

func TestApplyOpenOrderEventRemovesCancellation(t *testing.T) {
	service := &Service{
		cacheTTL: time.Minute,
		openOrders: map[int64]cachedOpenOrders{
			7: {value: []OpenOrder{{ID: "order-1"}, {ID: "order-2"}}},
		},
	}
	orders := service.applyOpenOrderEvent(7, OpenOrder{ID: "order-1"}, false)
	if len(orders) != 1 || orders[0].ID != "order-2" {
		t.Fatalf("orders=%+v", orders)
	}
}

func TestUserStreamCancellationIsNotLive(t *testing.T) {
	order, live := openOrderFromUserEvent(userStreamMessage{
		EventType: "order", Type: "CANCELLATION", ID: "order-1",
		OriginalSize: "10000000", SizeMatched: "1000000", Status: "CANCELED",
	})
	if live || order.ID != "order-1" {
		t.Fatalf("order=%+v live=%v", order, live)
	}
}
