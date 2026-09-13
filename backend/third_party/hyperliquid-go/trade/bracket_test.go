package trade

import (
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

func TestBracketOrders_None(t *testing.T) {
	if got := bracketOrders(&types.OrderSpec{Side: types.Buy, Size: 1, Price: 100}); got != nil {
		t.Errorf("expected nil bracket, got %v", got)
	}
}

func TestBracketOrders_BuyEntryProducesTwoLegs(t *testing.T) {
	spec := &types.OrderSpec{
		Coin: "BTC", Side: types.Buy, Size: 1, Price: 100,
		TakeProfit: 110, StopLoss: 90,
	}
	got := bracketOrders(spec)
	if len(got) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(got))
	}
	tp, sl := got[0], got[1]
	if tp.Coin != "BTC" || tp.IsBuy {
		t.Errorf("TP leg should be opposite side: %+v", tp)
	}
	if tp.Price != 110 || tp.Size != 1 || !tp.ReduceOnly {
		t.Errorf("TP leg shape: %+v", tp)
	}
	if tp.OrderType.Trigger == nil ||
		tp.OrderType.Trigger.TriggerPx != 110 ||
		!tp.OrderType.Trigger.IsMarket ||
		tp.OrderType.Trigger.Tpsl != "tp" {
		t.Errorf("TP trigger shape: %+v", tp.OrderType.Trigger)
	}
	if sl.Price != 90 || sl.Size != 1 || !sl.ReduceOnly {
		t.Errorf("SL leg shape: %+v", sl)
	}
	if sl.OrderType.Trigger == nil ||
		sl.OrderType.Trigger.TriggerPx != 90 ||
		!sl.OrderType.Trigger.IsMarket ||
		sl.OrderType.Trigger.Tpsl != "sl" {
		t.Errorf("SL trigger shape: %+v", sl.OrderType.Trigger)
	}
}

func TestBracketOrders_SellEntryFlipsExit(t *testing.T) {
	spec := &types.OrderSpec{
		Coin: "BTC", Side: types.Sell, Size: 1, Price: 100,
		TakeProfit: 90, StopLoss: 110,
	}
	got := bracketOrders(spec)
	if len(got) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(got))
	}
	if !got[0].IsBuy || !got[1].IsBuy {
		t.Errorf("Sell entry should produce Buy bracket legs: %+v / %+v", got[0], got[1])
	}
}

func TestBracketOrders_PartialSizesAndCloids(t *testing.T) {
	cloidTP := "0xtp"
	cloidSL := "0xsl"
	spec := &types.OrderSpec{
		Coin: "BTC", Side: types.Buy, Size: 1, Price: 100,
		TakeProfit: 110, StopLoss: 90,
		TPSize: 0.4, SLSize: 0.6,
		TPCloid: cloidTP, SLCloid: cloidSL,
	}
	got := bracketOrders(spec)
	if got[0].Size != 0.4 {
		t.Errorf("partial TP size = %v, want 0.4", got[0].Size)
	}
	if got[1].Size != 0.6 {
		t.Errorf("partial SL size = %v, want 0.6", got[1].Size)
	}
	if got[0].ClientOrderID == nil || *got[0].ClientOrderID != cloidTP {
		t.Errorf("TP cloid not propagated: %v", got[0].ClientOrderID)
	}
	if got[1].ClientOrderID == nil || *got[1].ClientOrderID != cloidSL {
		t.Errorf("SL cloid not propagated: %v", got[1].ClientOrderID)
	}
}

func TestBracketOrders_TPOnly(t *testing.T) {
	spec := &types.OrderSpec{Coin: "BTC", Side: types.Buy, Size: 1, Price: 100, TakeProfit: 110}
	got := bracketOrders(spec)
	if len(got) != 1 {
		t.Fatalf("TP-only should produce 1 leg, got %d", len(got))
	}
	if got[0].OrderType.Trigger.Tpsl != "tp" {
		t.Errorf("got %s", got[0].OrderType.Trigger.Tpsl)
	}
}

func TestBracketOrders_SLOnly(t *testing.T) {
	spec := &types.OrderSpec{Coin: "BTC", Side: types.Buy, Size: 1, Price: 100, StopLoss: 90}
	got := bracketOrders(spec)
	if len(got) != 1 {
		t.Fatalf("SL-only should produce 1 leg, got %d", len(got))
	}
	if got[0].OrderType.Trigger.Tpsl != "sl" {
		t.Errorf("got %s", got[0].OrderType.Trigger.Tpsl)
	}
}

func TestBracketGrouping(t *testing.T) {
	if bracketGrouping(&types.OrderSpec{}) != types.GroupingNA {
		t.Errorf("no bracket → GroupingNA")
	}
	if bracketGrouping(&types.OrderSpec{TakeProfit: 110}) != types.GroupingNormalTpsl {
		t.Errorf("TP-only → GroupingNormalTpsl")
	}
	if bracketGrouping(&types.OrderSpec{StopLoss: 90}) != types.GroupingNormalTpsl {
		t.Errorf("SL-only → GroupingNormalTpsl")
	}
}
