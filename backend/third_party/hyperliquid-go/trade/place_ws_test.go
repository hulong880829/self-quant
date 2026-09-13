package trade

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/stream"
	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const testSigningKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

type recordingPoster struct {
	mu     sync.Mutex
	action any
	vault  string
	resp   json.RawMessage
	err    error
	calls  int
}

func (r *recordingPoster) PostActionContext(
	_ context.Context,
	action any,
	_ signing.SignatureResult,
	_ int64,
	vaultAddress string,
) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.action = action
	r.vault = vaultAddress
	r.calls++
	return r.resp, r.err
}

func testTradeClient(t *testing.T, poster *recordingPoster, vault string) *Client {
	t.Helper()
	key, err := crypto.HexToECDSA(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	infoC := stubInfo(t, "http://example.invalid", map[string]int{"BTC": 5})
	client := New(Config{
		PrivateKey: key,
		Vault:      vault,
		Info:       infoC,
	})
	client.SetStream(poster)
	return client
}

func TestPlaceWSRecordsSameActionShape(t *testing.T) {
	cloid := "0x0123456789abcdef0123456789abcdef"
	poster := &recordingPoster{resp: json.RawMessage(
		`{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":42,"cid":"` + cloid + `","status":"open"}}]}}}`,
	)}
	client := testTradeClient(t, poster, "0xvault")
	_, err := client.PlaceALOWS(
		context.Background(), "BTC", types.Buy, 1, 100,
		SkipValidation(), WithCloid(cloid), WithReduceOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	action, ok := poster.action.(signing.OrderAction)
	if !ok {
		t.Fatalf("action type %T", poster.action)
	}
	if action.Type != "order" || len(action.Orders) != 1 {
		t.Fatalf("action=%+v", action)
	}
	order := action.Orders[0]
	if order.Asset != 0 || !order.IsBuy || !order.ReduceOnly ||
		order.OrderType.Limit == nil || order.OrderType.Limit.Tif != string(types.TifAlo) {
		t.Fatalf("order=%+v", order)
	}
	if order.Cloid == nil || *order.Cloid != cloid {
		t.Fatalf("cloid=%v", order.Cloid)
	}
	if poster.vault != "0xvault" {
		t.Fatalf("vault=%q", poster.vault)
	}
}

func TestPlaceWSFilledRestingRejectedAndIOCNoMatch(t *testing.T) {
	tests := []struct {
		name   string
		resp   string
		status string
		err    string
	}{
		{
			name:   "filled",
			resp:   `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"oid":7,"avgPx":"100.5","totalSz":"0.5"}}]}}}`,
			status: "filled",
		},
		{
			name:   "resting",
			resp:   `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":42,"cid":"0xabc","status":"open"}}]}}}`,
			status: "open",
		},
		{
			name: "rejected",
			resp: `{"status":"ok","response":{"type":"order","data":{"statuses":[{"error":"Insufficient margin"}]}}}`,
			err:  "Insufficient margin",
		},
		{
			name: "ioc no match",
			resp: `{"status":"ok","response":{"type":"order","data":{"statuses":[{"error":"Order could not immediately match against any resting orders."}]}}}`,
			err:  "Order could not immediately match against any resting orders.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poster := &recordingPoster{resp: json.RawMessage(test.resp)}
			client := testTradeClient(t, poster, "")
			result, err := client.PlaceIOCWS(
				context.Background(), "BTC", types.Sell, 1, 100, SkipValidation(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.status != "" && result.Status != test.status {
				t.Fatalf("status=%q want %q result=%+v", result.Status, test.status, result)
			}
			if test.err != "" && result.Error != test.err {
				t.Fatalf("error=%q want %q", result.Error, test.err)
			}
		})
	}
}

func TestPlaceWSTransportErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "not connected", err: stream.ErrNotConnected},
		{name: "connection lost", err: stream.ErrConnectionLost},
		{name: "timeout", err: stream.ErrRequestTimeout},
		{name: "canceled", err: stream.ErrRequestCanceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poster := &recordingPoster{err: test.err}
			client := testTradeClient(t, poster, "")
			_, err := client.PlaceGTCWS(
				context.Background(), "BTC", types.Buy, 1, 100, SkipValidation(),
			)
			if !errors.Is(err, test.err) {
				t.Fatalf("err=%v want %v", err, test.err)
			}
			if poster.calls != 1 {
				t.Fatalf("calls=%d, must not REST-retry", poster.calls)
			}
		})
	}
}

func TestPlaceWSRequiresStream(t *testing.T) {
	key, err := crypto.HexToECDSA(testSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	client := New(Config{
		PrivateKey: key,
		Info:       stubInfo(t, "http://example.invalid", map[string]int{"BTC": 5}),
	})
	_, err = client.PlaceALOWS(context.Background(), "BTC", types.Buy, 1, 100, SkipValidation())
	if !errors.Is(err, stream.ErrNotConnected) {
		t.Fatalf("err=%v, want ErrNotConnected", err)
	}
}

func TestPlaceWSDoesNotFallbackToREST(t *testing.T) {
	poster := &recordingPoster{err: stream.ErrRequestTimeout}
	client := testTradeClient(t, poster, "")
	_, err := client.PlaceIOCWS(
		context.Background(), "BTC", types.Buy, 1, 100, SkipValidation(),
	)
	if !errors.Is(err, stream.ErrRequestTimeout) {
		t.Fatalf("err=%v", err)
	}
	if poster.calls != 1 {
		t.Fatalf("calls=%d", poster.calls)
	}
}
