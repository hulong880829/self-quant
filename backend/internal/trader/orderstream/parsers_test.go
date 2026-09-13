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
			payload: `{"channel":"spot.usertrades","time_ms":1700000000123,"result":{"text":"client-6","order_id":"47","status":"closed","filled_amount":"6","filled_total":"90","avg_deal_price":"15","trade_id":"12"}}`,
			client:  "client-6", order: "47", trade: "12", status: StatusFilled, filled: "6", average: "15",
		},
		{
			name: "Gate perpetual", venue: VenueGate, product: ProductPerpetual,
			payload: `{"channel":"futures.orders","time_ms":1700000000123,"result":[{"text":"client-6p","id":"47p","status":"open","filled_total":"0"}]}`,
			client:  "client-6p", order: "47p", status: StatusNew, filled: "0",
		},
		{
			name: "Aster perpetual", venue: VenueAster, product: ProductPerpetual,
			payload: `{"e":"ORDER_TRADE_UPDATE","E":1700000000123,"o":{"c":"client-7","i":48,"X":"FILLED","z":"2","ap":"16","t":13}}`,
			client:  "client-7", order: "48", trade: "13", status: StatusFilled, filled: "2", average: "16",
		},
		{
			name: "Hyperliquid order", venue: VenueHyperliquid, product: ProductPerpetual,
			payload: `{"channel":"orderUpdates","data":[{"order":{"oid":49,"cloid":"0xabc","sz":"1","origSz":"3","timestamp":1700000000123},"status":"open","statusTimestamp":1700000000124}]}`,
			client:  "0xabc", order: "49", status: StatusNew, filled: "2",
		},
		{
			name: "Lighter order", venue: VenueLighter, product: ProductPerpetual,
			payload: `{"type":"update/account_all_orders","nonce":99,"orders":{"0":[{"client_order_index":50,"order_id":"51","status":"filled","filled_base_amount":"2","filled_quote_amount":"34","updated_at":1700000000123}]}}`,
			client:  "50", order: "51", status: StatusFilled, filled: "2", average: "17",
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
		updates[0].LastPrice != "100" || updates[0].Type != UpdateTrade ||
		updates[0].CumulativeFilled != "" {
		t.Fatalf("unexpected fast fill: %+v", updates[0])
	}
}

func TestPrivateStreamsPreserveIOCFinalStateAndPostOnlyReason(t *testing.T) {
	received := time.Now().UTC()
	tests := []struct {
		name         string
		key          Key
		payload      string
		status       Status
		filled       string
		errorCode    string
		errorMessage string
	}{
		{
			name: "gate perpetual partial IOC",
			key:  Key{Account: "a", Venue: VenueGate, Product: ProductPerpetual},
			payload: `{"channel":"futures.orders","result":[{
				"id":"g-1","status":"finished","finish_as":"ioc","size":"10","left":"4"
			}]}`,
			status: StatusCanceled, filled: "6",
		},
		{
			name: "gate spot partial IOC",
			key:  Key{Account: "a", Venue: VenueGate, Product: ProductSpot},
			payload: `{"channel":"spot.orders","result":[{
				"id":"g-2","status":"closed","finish_as":"ioc","filled_amount":"0.4"
			}]}`,
			status: StatusCanceled, filled: "0.4",
		},
		{
			name: "bybit partial IOC",
			key:  Key{Account: "a", Venue: VenueBybit, Product: ProductPerpetual},
			payload: `{"topic":"order","data":[{
				"orderId":"b-1","orderStatus":"PartiallyFilledCanceled",
				"cumExecQty":"0.4","rejectReason":"EC_NoError","cancelType":"CancelByUser"
			}]}`,
			status: StatusCanceled, filled: "0.4",
			errorCode: "EC_NoError", errorMessage: "CancelByUser",
		},
		{
			name: "okx post only reason",
			key:  Key{Account: "a", Venue: VenueOKX, Product: ProductPerpetual},
			payload: `{"arg":{"channel":"orders"},"data":[{
				"ordId":"o-1","state":"canceled","accFillSz":"0","cancelSource":"31"
			}]}`,
			status: StatusCanceled, filled: "0", errorCode: "31",
		},
		{
			name: "bitget post only reason",
			key:  Key{Account: "a", Venue: VenueBitget, Product: ProductPerpetual},
			payload: `{"arg":{"topic":"order"},"data":[{
				"orderId":"bg-1","orderStatus":"canceled","cumExecQty":"0",
				"cancelReason":"post_only_fill_cancel"
			}]}`,
			status: StatusCanceled, filled: "0",
			errorCode: "post_only_fill_cancel",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			updates, matched, err := DefaultParsers()[test.key.Venue](
				test.key, []byte(test.payload), received,
			)
			if err != nil || !matched || len(updates) != 1 {
				t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
			}
			update := updates[0]
			if update.Status != test.status ||
				update.CumulativeFilled != test.filled ||
				update.ErrorCode != test.errorCode ||
				update.ErrorMessage != test.errorMessage {
				t.Fatalf("update=%+v", update)
			}
		})
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

func TestDEXParsersKeepUnknownStatesAndMapTerminalReasons(t *testing.T) {
	t.Parallel()
	received := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name      string
		venue     string
		payload   string
		status    Status
		filled    string
		errorCode string
	}{
		{
			name: "Aster expiration", venue: VenueAster,
			payload: `{"e":"ORDER_TRADE_UPDATE","o":{
				"c":"a","i":1,"X":"EXPIRED_IN_MATCH","z":"0.1",
				"r":"POST_ONLY_REJECT"
			}}`,
			status: StatusExpired, filled: "0.1", errorCode: "POST_ONLY_REJECT",
		},
		{
			name: "Hyperliquid margin cancel", venue: VenueHyperliquid,
			payload: `{"channel":"orderUpdates","data":[{
				"order":{"oid":2,"cloid":"0x2","origSz":"3","sz":"1"},
				"status":"marginCanceled","statusTimestamp":1700000000001
			}]}`,
			status: StatusCanceled, filled: "2", errorCode: "marginCanceled",
		},
		{
			name: "Hyperliquid scheduled cancel", venue: VenueHyperliquid,
			payload: `{"channel":"orderUpdates","data":[{
				"order":{"oid":6,"cloid":"0x6","origSz":"1","sz":"1"},
				"status":"scheduledCancel","statusTimestamp":1700000000001
			}]}`,
			status: StatusCanceled, filled: "0", errorCode: "scheduledCancel",
		},
		{
			name: "Hyperliquid rejected suffix", venue: VenueHyperliquid,
			payload: `{"channel":"orderUpdates","data":[{
				"order":{"oid":7,"cloid":"0x7","origSz":"1","sz":"1"},
				"status":"badAloPxRejected","statusTimestamp":1700000000001
			}]}`,
			status: StatusRejected, filled: "0", errorCode: "badAloPxRejected",
		},
		{
			name: "Hyperliquid unknown", venue: VenueHyperliquid,
			payload: `{"channel":"orderUpdates","data":[{
				"order":{"oid":3,"cloid":"0x3"},"status":"futureState"
			}]}`,
			status: StatusUnknown,
		},
		{
			name: "Lighter cancel reason", venue: VenueLighter,
			payload: `{"type":"update/account_all_orders","orders":{"0":[{
				"client_order_index":4,"order_id":5,
				"status":"canceled_due_to_insufficient_margin",
				"filled_base_amount":"0.4",
				"cancel_reason":"insufficient_margin"
			}]}}`,
			status: StatusCanceled, filled: "0.4", errorCode: "insufficient_margin",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			updates, matched, err := DefaultParsers()[test.venue](
				Key{Account: "a", Venue: test.venue, Product: ProductPerpetual},
				[]byte(test.payload),
				received,
			)
			if err != nil || !matched || len(updates) != 1 {
				t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
			}
			if update := updates[0]; update.Status != test.status ||
				update.CumulativeFilled != test.filled ||
				update.ErrorCode != test.errorCode {
				t.Fatalf("update=%+v", update)
			}
		})
	}
}

func TestHyperliquidFilledDoesNotGuessOrigSz(t *testing.T) {
	t.Parallel()
	received := time.Unix(1_700_000_000, 0).UTC()
	updates, matched, err := parseHyperliquid(
		Key{Account: "a", Venue: VenueHyperliquid, Product: ProductPerpetual},
		[]byte(`{"channel":"orderUpdates","data":[{
			"order":{"oid":8,"cloid":"0x8","origSz":"40"},
			"status":"filled","statusTimestamp":1700000000001
		}]}`),
		received,
	)
	if err != nil || !matched || len(updates) != 1 {
		t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
	}
	if updates[0].Status != StatusFilled || updates[0].CumulativeFilled != "" {
		t.Fatalf("must not guess origSz as fill: %+v", updates[0])
	}
}

func TestHyperliquidFilledUsesFilledSz(t *testing.T) {
	t.Parallel()
	received := time.Unix(1_700_000_000, 0).UTC()
	updates, matched, err := parseHyperliquid(
		Key{Account: "a", Venue: VenueHyperliquid, Product: ProductPerpetual},
		[]byte(`{"channel":"orderUpdates","data":[{
			"order":{"oid":9,"cloid":"0x9","origSz":"40","sz":"0","filledSz":"47"},
			"status":"filled","statusTimestamp":1700000000001
		}]}`),
		received,
	)
	if err != nil || !matched || len(updates) != 1 {
		t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
	}
	if updates[0].CumulativeFilled != "47" {
		t.Fatalf("filledSz=%q", updates[0].CumulativeFilled)
	}
}

func TestHyperliquidUserFills(t *testing.T) {
	t.Parallel()
	received := time.Unix(1_700_000_000, 0).UTC()
	key := Key{Account: "a", Venue: VenueHyperliquid, Product: ProductPerpetual}

	t.Run("maker and snapshot fills", func(t *testing.T) {
		t.Parallel()
		updates, matched, err := parseHyperliquid(key, []byte(`{"channel":"userFills","data":{"isSnapshot":true,"fills":[
			{"oid":11,"tid":1001,"px":"100","sz":"4","cloid":"0xmaker","time":1700000000100},
			{"oid":11,"tid":1002,"px":"110","sz":"6","cloid":"0xmaker","time":1700000000101}
		]}}`), received)
		if err != nil || !matched || len(updates) != 2 {
			t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
		}
		if updates[0].Type != UpdateTrade || updates[0].LastPrice != "100" ||
			updates[0].LastFilled != "4" || updates[0].VenueOrderID != "11" ||
			updates[0].TradeID != "1001" || updates[0].ClientOrderID != "0xmaker" {
			t.Fatalf("first=%+v", updates[0])
		}
		if updates[1].LastPrice != "110" || updates[1].LastFilled != "6" || updates[1].TradeID != "1002" {
			t.Fatalf("second=%+v", updates[1])
		}
	})

	t.Run("skips invalid px sz oid tid", func(t *testing.T) {
		t.Parallel()
		updates, matched, err := parseHyperliquid(key, []byte(`{"channel":"userFills","data":{"fills":[
			{"oid":11,"tid":1,"px":"0","sz":"1","time":1},
			{"oid":11,"tid":2,"px":"-1","sz":"1","time":1},
			{"oid":11,"tid":3,"px":"100","sz":"0","time":1},
			{"oid":11,"tid":"","px":"100","sz":"1","time":1},
			{"oid":"","tid":4,"px":"100","sz":"1","time":1},
			{"oid":11,"tid":5,"px":"100","sz":"1.5","cloid":"0xok","time":2}
		]}}`), received)
		if err != nil {
			t.Fatal(err)
		}
		if !matched || len(updates) != 1 || updates[0].TradeID != "5" || updates[0].LastFilled != "1.5" {
			t.Fatalf("updates=%+v matched=%v", updates, matched)
		}
	})

	t.Run("large integer tid", func(t *testing.T) {
		t.Parallel()
		updates, matched, err := parseHyperliquid(key, []byte(
			`{"channel":"userFills","data":{"fills":[{"oid":11,"tid":18446744073709551615,"px":"100","sz":"1","time":1}]}}`,
		), received)
		if err != nil || !matched || len(updates) != 1 {
			t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
		}
		if updates[0].TradeID != "18446744073709551615" {
			t.Fatalf("tid=%q", updates[0].TradeID)
		}
	})

	t.Run("all invalid unmatched", func(t *testing.T) {
		t.Parallel()
		updates, matched, err := parseHyperliquid(key, []byte(
			`{"channel":"userFills","data":{"fills":[{"oid":11,"tid":1,"px":"0","sz":"1","time":1}]}}`,
		), received)
		if err != nil || matched || len(updates) != 0 {
			t.Fatalf("updates=%+v matched=%v err=%v", updates, matched, err)
		}
	})
}
