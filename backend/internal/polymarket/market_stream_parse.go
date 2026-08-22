package polymarket

import (
	"bytes"
	"encoding/json"
)

type quoteDelta struct {
	tokenID string
	bid     string
	ask     string
}

type clobQuoteEvent struct {
	AssetID      string           `json:"asset_id"`
	AssetIDCamel string           `json:"assetId"`
	BestBid      json.RawMessage  `json:"best_bid"`
	BestBidCamel json.RawMessage  `json:"bestBid"`
	BestAsk      json.RawMessage  `json:"best_ask"`
	BestAskCamel json.RawMessage  `json:"bestAsk"`
	PriceChanges []clobQuoteEvent `json:"price_changes"`
}

func parseCLOBQuoteMessages(body []byte) []quoteDelta {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || !looksLikeQuoteMessage(trimmed) {
		return nil
	}
	if trimmed[0] == '[' {
		var events []clobQuoteEvent
		if json.Unmarshal(trimmed, &events) != nil {
			return nil
		}
		return collectQuoteDeltas(events)
	}
	var event clobQuoteEvent
	if json.Unmarshal(trimmed, &event) != nil {
		return nil
	}
	return collectQuoteDeltas([]clobQuoteEvent{event})
}

func looksLikeQuoteMessage(body []byte) bool {
	// Full order-book dumps are large and unused for quotes — skip without parsing.
	if bytes.Contains(body, []byte(`"event_type":"book"`)) ||
		bytes.Contains(body, []byte(`"event_type": "book"`)) {
		return false
	}
	return bytes.Contains(body, []byte("best_bid")) ||
		bytes.Contains(body, []byte("bestBid")) ||
		bytes.Contains(body, []byte("best_ask")) ||
		bytes.Contains(body, []byte("bestAsk")) ||
		bytes.Contains(body, []byte("price_changes"))
}

func collectQuoteDeltas(events []clobQuoteEvent) []quoteDelta {
	result := make([]quoteDelta, 0, len(events))
	for _, event := range events {
		result = append(result, quoteDeltasFromEvent(event)...)
	}
	return result
}

func quoteDeltasFromEvent(event clobQuoteEvent) []quoteDelta {
	tokenID := event.AssetID
	if tokenID == "" {
		tokenID = event.AssetIDCamel
	}
	bid := rawJSONString(event.BestBid)
	if bid == "" {
		bid = rawJSONString(event.BestBidCamel)
	}
	ask := rawJSONString(event.BestAsk)
	if ask == "" {
		ask = rawJSONString(event.BestAskCamel)
	}
	if tokenID != "" && (bid != "" || ask != "") {
		return []quoteDelta{{tokenID: tokenID, bid: bid, ask: ask}}
	}
	if len(event.PriceChanges) == 0 {
		return nil
	}
	return collectQuoteDeltas(event.PriceChanges)
}

func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var asNumber json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&asNumber); err == nil {
		return asNumber.String()
	}
	return ""
}
