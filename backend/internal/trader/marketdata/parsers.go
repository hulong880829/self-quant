package marketdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func DefaultParsers() map[string]Parser {
	return map[string]Parser{
		VenueBinance: parseBinance,
		VenueOKX:     parseOKX,
		VenueBybit:   parseBybit,
		VenueBitget:  parseBitget,
		VenueGate:    parseGate,
	}
}

func parseBinance(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	// A map is intentional: encoding/json matches struct fields case-insensitively,
	// while Binance uses b/B and a/A for price/quantity.
	var message map[string]json.RawMessage
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	var symbol, bid, ask string
	if err := json.Unmarshal(message["s"], &symbol); err != nil && len(message["s"]) > 0 {
		return BBO{}, false, fmt.Errorf("decode symbol: %w", err)
	}
	if err := json.Unmarshal(message["b"], &bid); err != nil && len(message["b"]) > 0 {
		return BBO{}, false, fmt.Errorf("decode bid: %w", err)
	}
	if err := json.Unmarshal(message["a"], &ask); err != nil && len(message["a"]) > 0 {
		return BBO{}, false, fmt.Errorf("decode ask: %w", err)
	}
	if bid == "" || ask == "" {
		return BBO{}, false, nil
	}
	if symbol != "" && !strings.EqualFold(symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	return makeBBO(key, bid, ask, message["E"], received)
}

func parseOKX(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Data []struct {
			Instrument string          `json:"instId"`
			Bid        string          `json:"bidPx"`
			Ask        string          `json:"askPx"`
			Time       json.RawMessage `json:"ts"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	for _, item := range message.Data {
		if item.Bid != "" && item.Ask != "" && strings.EqualFold(item.Instrument, key.Symbol) {
			return makeBBO(key, item.Bid, item.Ask, item.Time, received)
		}
	}
	return BBO{}, false, nil
}

func parseBybit(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Topic string          `json:"topic"`
		Time  json.RawMessage `json:"ts"`
		Data  struct {
			Symbol string `json:"symbol"`
			Bid    string `json:"bid1Price"`
			Ask    string `json:"ask1Price"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Data.Bid == "" || message.Data.Ask == "" {
		return BBO{}, false, nil
	}
	if message.Data.Symbol != "" && !strings.EqualFold(message.Data.Symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	return makeBBO(key, message.Data.Bid, message.Data.Ask, message.Time, received)
}

func parseBitget(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Argument struct {
			Instrument string `json:"instId"`
		} `json:"arg"`
		Data []struct {
			Bid  string          `json:"bidPr"`
			Ask  string          `json:"askPr"`
			Time json.RawMessage `json:"ts"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Argument.Instrument != "" &&
		!strings.EqualFold(message.Argument.Instrument, key.Symbol) {
		return BBO{}, false, nil
	}
	for _, item := range message.Data {
		if item.Bid != "" && item.Ask != "" {
			return makeBBO(key, item.Bid, item.Ask, item.Time, received)
		}
	}
	return BBO{}, false, nil
}

func parseGate(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Time   json.RawMessage `json:"time_ms"`
		Result struct {
			Symbol string          `json:"s"`
			Bid    string          `json:"b"`
			Ask    string          `json:"a"`
			Time   json.RawMessage `json:"t"`
		} `json:"result"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Result.Bid == "" || message.Result.Ask == "" {
		return BBO{}, false, nil
	}
	if message.Result.Symbol != "" && !strings.EqualFold(message.Result.Symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	timestamp := message.Result.Time
	if len(bytes.TrimSpace(timestamp)) == 0 {
		timestamp = message.Time
	}
	return makeBBO(key, message.Result.Bid, message.Result.Ask, timestamp, received)
}

func decodeJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func makeBBO(
	key Key,
	bid string,
	ask string,
	rawTimestamp json.RawMessage,
	received time.Time,
) (BBO, bool, error) {
	venueTime, err := millisecondsTime(rawTimestamp)
	if err != nil {
		return BBO{}, false, err
	}
	return BBO{
		Key:              key,
		BidPrice:         bid,
		AskPrice:         ask,
		VenueTimestamp:   venueTime,
		ReceiveTimestamp: received,
	}, true, nil
}

func millisecondsTime(raw json.RawMessage) (time.Time, error) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return time.Time{}, nil
	}
	milliseconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse venue timestamp %q: %w", text, err)
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}
