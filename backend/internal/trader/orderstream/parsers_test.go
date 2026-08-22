package orderstream

import (
	"testing"
	"time"
)

func TestVenueParsers(t *testing.T) {
	t.Parallel()
	received := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name    string
		venue   string
		product string
		payload string
		client  string
		order   string
		trade   string
		status  Status
		filled  string
		average string
	}{
		{
			name: "Binance spot", venue: VenueBinance, product: ProductSpot,
			payload: `{"e":"executionReport","E":1700000000123,"c":"client-1","i":42,"X":"FILLED","z":"2","Z":"20","t":7}`,
			client:  "client-1", order: "42", trade: "7", status: StatusFilled, filled: "2", average: "10",
		},
		{
			name: "Binance perpetual", venue: VenueBinance, product: ProductPerpetual,
			payload: `{"e":"ORDER_TRADE_UPDATE","E":1700000000123,"o":{"c":"client-2","i":43,"X":"PARTIALLY_FILLED","z":"1","ap":"11","t":8}}`,
			client:  "client-2", order: "43", trade: "8", status: StatusPartiallyFilled, filled: "1", average: "11",
		},
		{
			name: "OKX", venue: VenueOKX, product: ProductPerpetual,
			payload: `{"arg":{"channel":"orders"},"data":[{"clOrdId":"client-3","ordId":"44","state":"filled","accFillSz":"3","avgPx":"12","tradeId":"9","uTime":"1700000000123","seqId":"99"}]}`,
			client:  "client-3", order: "44", trade: "9", status: StatusFilled, filled: "3", average: "12",
		},
		{
			name: "OKX spot", venue: VenueOKX, product: ProductSpot,
			payload: `{"arg":{"channel":"orders"},"data":[{"clOrdId":"client-3s","ordId":"44s","state":"live","accFillSz":"0","avgPx":""}]}`,
			client:  "client-3s", order: "44s", status: StatusNew, filled: "0",
		},
		{
			name: "Bybit", venue: VenueBybit, product: ProductSpot,
			payload: `{"topic":"execution","creationTime":1700000000123,"data":[{"orderLinkId":"client-4","orderId":"45","orderStatus":"Filled","cumExecQty":"4","avgPrice":"13","execId":"10","execTime":"1700000000123"}]}`,
			client:  "client-4", order: "45", trade: "10", status: StatusFilled, filled: "4", average: "13",
		},
		{
			name: "Bybit perpetual", venue: VenueBybit, product: ProductPerpetual,
			payload: `{"topic":"order","creationTime":1700000000123,"data":[{"orderLinkId":"client-4p","orderId":"45p","orderStatus":"PartiallyFilled","cumExecQty":"1","avgPrice":"13"}]}`,
			client:  "client-4p", order: "45p", status: StatusPartiallyFilled, filled: "1", average: "13",
		},
		{
			name: "Bitget", venue: VenueBitget, product: ProductPerpetual,
			payload: `{"arg":{"instType":"UTA","topic":"order"},"data":[{"clientOid":"client-5","orderId":"46","orderStatus":"filled","cumExecQty":"5","avgPrice":"14","updatedTime":"1700000000123"}]}`,
			client:  "client-5", order: "46", status: StatusFilled, filled: "5", average: "14",
		},
		{
			name: "Bitget spot", venue: VenueBitget, product: ProductSpot,
			payload: `{"arg":{"instType":"UTA","topic":"order"},"data":[{"clientOid":"client-5s","orderId":"46s","orderStatus":"new","cumExecQty":"0"}]}`,
			client:  "client-5s", order: "46s", status: StatusNew, filled: "0",
		},
		{
			name: "Gate", venue: VenueGate, product: ProductSpot,
			payload: `{"channel":"spot.usertrades","time_ms":1700000000123,"result":{"text":"client-6","order_id":"47","status":"closed","filled_total":"6","avg_deal_price":"15","trade_id":"12"}}`,
			client:  "client-6", order: "47", trade: "12", status: StatusFilled, filled: "6", average: "15",
		},
		{
			name: "Gate perpetual", venue: VenueGate, product: ProductPerpetual,
			payload: `{"channel":"futures.orders","time_ms":1700000000123,"result":[{"text":"client-6p","id":"47p","status":"open","filled_total":"0"}]}`,
			client:  "client-6p", order: "47p", status: StatusNew, filled: "0",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			key := Key{Account: "account", Venue: test.venue, Product: test.product}
			updates, matched, err := DefaultParsers()[test.venue](key, []byte(test.payload), received)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !matched || len(updates) != 1 {
				t.Fatalf("matched=%v updates=%d", matched, len(updates))
			}
			update := updates[0]
			if update.Account != "account" || update.ClientOrderID != test.client ||
				update.VenueOrderID != test.order || update.TradeID != test.trade ||
				update.Status != test.status || update.CumulativeFilled != test.filled ||
				update.AveragePrice != test.average {
				t.Fatalf("unexpected update: %+v", update)
			}
			if update.EventTime.IsZero() {
				t.Fatal("event time was not populated")
			}
		})
	}
}

func TestParsersIgnoreControlMessages(t *testing.T) {
	t.Parallel()
	key := Key{Account: "a", Venue: VenueOKX, Product: ProductSpot}
	updates, matched, err := parseOKX(key, []byte(`{"event":"subscribe","arg":{"channel":"orders"}}`), time.Now())
	if err != nil || matched || len(updates) != 0 {
		t.Fatalf("updates=%v matched=%v err=%v", updates, matched, err)
	}
}

func TestBitgetFastFillParsesObjectPayload(t *testing.T) {
	key := Key{Account: "a", Venue: VenueBitget, Product: ProductPerpetual}
	updates, matched, err := parseBitget(key, []byte(
		`{"arg":{"instType":"UTA","topic":"fast-fill"},"data":{"clientOid":"client","orderId":"order","execId":"fill","execQty":"0.2","execPrice":"100","execTime":"1700000000123"}}`,
	), time.Now())
	if err != nil || !matched || len(updates) != 1 {
		t.Fatalf("updates=%v matched=%v err=%v", updates, matched, err)
	}
	if updates[0].TradeID != "fill" || updates[0].LastFilled != "0.2" ||
		updates[0].LastPrice != "100" || updates[0].Type != UpdateTrade {
		t.Fatalf("unexpected fast fill: %+v", updates[0])
	}
}

func TestPrivateStreamControlErrorRejectsSubscriptionFailure(t *testing.T) {
	if err := privateStreamControlError([]byte(
		`{"event":"error","code":"30004","msg":"requires login"}`,
	)); err == nil {
		t.Fatal("expected subscription failure")
	}
	if err := privateStreamControlError([]byte(
		`{"event":"subscribe","code":"0"}`,
	)); err != nil {
		t.Fatalf("successful subscription rejected: %v", err)
	}
}
