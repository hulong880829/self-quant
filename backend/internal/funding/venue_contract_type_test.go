package funding

import "testing"

func TestDeriveVenueContractType(t *testing.T) {
	if got := DeriveVenueContractType("binance", "ZHIPUUSDT", []byte(`{"ContractType":"TRADIFI_PERPETUAL"}`)); got != VenueContractTypeTradifi {
		t.Fatalf("binance tradifi=%q", got)
	}
	if got := DeriveVenueContractType("binance", "BTCUSDT", []byte(`{"contractType":"PERPETUAL"}`)); got != VenueContractTypePerpetual {
		t.Fatalf("binance perp=%q", got)
	}
	if got := DeriveVenueContractType("hyperliquid", "xyz:ZHIPU", []byte(`{"dex":"xyz","name":"ZHIPU"}`)); got != VenueContractTypeHIP3 {
		t.Fatalf("hip3 metadata=%q", got)
	}
	if got := DeriveVenueContractType("hyperliquid", "xyz:ZHIPU", []byte(`{}`)); got != VenueContractTypeHIP3 {
		t.Fatalf("hip3 symbol=%q", got)
	}
	if got := DeriveVenueContractType("hyperliquid", "BTC", []byte(`{"dex":""}`)); got != VenueContractTypePerpetual {
		t.Fatalf("hl default=%q", got)
	}
	if got := DeriveVenueContractType("okx", "BTC-USDT-SWAP", nil); got != VenueContractTypePerpetual {
		t.Fatalf("other=%q", got)
	}
	if got := DeriveVenueContractType("entropy", "io:ANTH", []byte(`{"dex":"io"}`)); got != VenueContractTypeHIP3 {
		t.Fatalf("entropy=%q", got)
	}
}
