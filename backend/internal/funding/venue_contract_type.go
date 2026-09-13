package funding

import (
	"encoding/json"
	"strings"
)

const (
	VenueContractTypePerpetual = "PERPETUAL"
	VenueContractTypeTradifi   = "TRADIFI_PERPETUAL"
	VenueContractTypeHIP3      = "HIP3"
)

func DeriveVenueContractType(exchange, exchangeSymbol string, metadata json.RawMessage) string {
	fields := map[string]any{}
	_ = json.Unmarshal(metadata, &fields)
	switch strings.ToLower(strings.TrimSpace(exchange)) {
	case "binance":
		for _, key := range []string{"contractType", "ContractType"} {
			value, _ := fields[key].(string)
			if strings.EqualFold(strings.TrimSpace(value), VenueContractTypeTradifi) {
				return VenueContractTypeTradifi
			}
		}
	case "entropy":
		return VenueContractTypeHIP3
	case "hyperliquid":
		dex, _ := fields["dex"].(string)
		if strings.TrimSpace(dex) != "" || strings.Contains(exchangeSymbol, ":") {
			return VenueContractTypeHIP3
		}
	}
	return VenueContractTypePerpetual
}
