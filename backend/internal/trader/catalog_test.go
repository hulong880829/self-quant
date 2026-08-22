package trader

import "testing"

func TestLinearSettlementEmptySettleUsesQuote(t *testing.T) {
	if !linearSettlement(Instrument{
		Exchange: "bitget", ContractType: "perpetual",
		QuoteAsset: "USDT", SettleAsset: "",
	}) {
		t.Fatal("empty settle with USDT quote should be linear")
	}
	if !linearSettlement(Instrument{
		Exchange: "bitget", ContractType: "perpetual",
		QuoteAsset: "USDC", SettleAsset: "",
	}) {
		t.Fatal("empty settle with USDC quote should be linear")
	}
	if linearSettlement(Instrument{
		Exchange: "bitget", ContractType: "perpetual",
		QuoteAsset: "USD", SettleAsset: "",
	}) {
		t.Fatal("empty settle with USD quote should be rejected")
	}
	if linearSettlement(Instrument{
		Exchange: "bitget", ContractType: "perpetual",
		QuoteAsset: "USDT", SettleAsset: "",
		Metadata: map[string]any{"contractModel": "inverse"},
	}) {
		t.Fatal("inverse contract model should be rejected")
	}
}
