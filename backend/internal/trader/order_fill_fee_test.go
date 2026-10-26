package trader

import "testing"

func TestNormalizeStoredFillFee(t *testing.T) {
	amount, currency, role := normalizeStoredFillFee(OrderFill{
		TradeID: "t1", FeeAmount: "1.25", FeeCurrency: "usdt", LiquidityRole: "maker",
	})
	if amount != "1.25" || currency != "USDT" || role != "maker" {
		t.Fatalf("amount=%v currency=%v role=%s", amount, currency, role)
	}
	amount, currency, role = normalizeStoredFillFee(OrderFill{
		TradeID: "t1", FeeAmount: "-0.1", FeeCurrency: "usdt",
	})
	if amount != "-0.1" || currency != "USDT" || role != "unknown" {
		t.Fatalf("rebate amount=%v currency=%v role=%s", amount, currency, role)
	}
	amount, currency, role = normalizeStoredFillFee(OrderFill{TradeID: "t1", FeeAmount: "9"})
	if amount != nil || currency != nil || role != "unknown" {
		t.Fatalf("unconfirmed fee amount=%v currency=%v role=%s", amount, currency, role)
	}
	if orderLiquidityRole(2, 0, 0) != "maker" || orderLiquidityRole(0, 3, 0) != "taker" ||
		orderLiquidityRole(1, 1, 0) != "mixed" || orderLiquidityRole(1, 0, 1) != "unknown" {
		t.Fatal("liquidity aggregate mismatch")
	}
}

func TestEventSchemaVersionIsAddedOnce(t *testing.T) {
	payload := withEventSchema(map[string]any{"reason": "user"})
	if payload["schemaVersion"] != 1 || payload["reason"] != "user" {
		t.Fatalf("payload=%v", payload)
	}
	payload["schemaVersion"] = 2
	again := withEventSchema(payload)
	if again["schemaVersion"] != 2 {
		t.Fatalf("overwrote schemaVersion: %v", again)
	}
	if withEventSchema(nil)["schemaVersion"] != 1 {
		t.Fatal("nil payload was not stamped")
	}
}
