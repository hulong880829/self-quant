package marketdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

func DefaultParsers() map[string]Parser {
	lighterParser := &lighterBookParser{}
	return map[string]Parser{
		VenueBinance:     parseBinance,
		VenueOKX:         parseOKX,
		VenueBybit:       parseBybit,
		VenueBitget:      parseBitget,
		VenueGate:        parseGate,
		VenueHyperliquid: parseHyperliquid,
		VenueAster:       parseAster,
		VenueLighter:     lighterParser.parse,
	}
}

func defaultParserFactories() map[string]func() Parser {
	return map[string]func() Parser{
		VenueLighter: func() Parser {
			parser := &lighterBookParser{}
			return parser.parse
		},
	}
}

func parseBinance(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Data struct {
			Bids      [][]string      `json:"bids"`
			Asks      [][]string      `json:"asks"`
			ShortBids [][]string      `json:"b"`
			ShortAsks [][]string      `json:"a"`
			Time      json.RawMessage `json:"E"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	bids, asks := message.Data.Bids, message.Data.Asks
	if len(bids) == 0 && len(asks) == 0 {
		bids, asks = message.Data.ShortBids, message.Data.ShortAsks
	}
	if len(bids) == 0 || len(asks) == 0 ||
		len(bids[0]) == 0 || len(asks[0]) == 0 {
		return BBO{}, false, nil
	}
	bidQty, askQty := "", ""
	if len(bids[0]) > 1 {
		bidQty = bids[0][1]
	}
	if len(asks[0]) > 1 {
		askQty = asks[0][1]
	}
	return makeBBO(
		key, bids[0][0], asks[0][0], bidQty, askQty,
		message.Data.Time, received,
	)
}

func parseOKX(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Argument struct {
			Instrument string `json:"instId"`
			Channel    string `json:"channel"`
		} `json:"arg"`
		Data []struct {
			Bids [][]string      `json:"bids"`
			Asks [][]string      `json:"asks"`
			Time json.RawMessage `json:"ts"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Argument.Channel != "books5" ||
		!strings.EqualFold(message.Argument.Instrument, key.Symbol) ||
		len(message.Data) == 0 {
		return BBO{}, false, nil
	}
	item := message.Data[0]
	if len(item.Bids) == 0 || len(item.Asks) == 0 ||
		len(item.Bids[0]) == 0 || len(item.Asks[0]) == 0 {
		return BBO{}, false, nil
	}
	bidQty, askQty := "", ""
	if len(item.Bids[0]) > 1 {
		bidQty = item.Bids[0][1]
	}
	if len(item.Asks[0]) > 1 {
		askQty = item.Asks[0][1]
	}
	return makeBBO(key, item.Bids[0][0], item.Asks[0][0], bidQty, askQty, item.Time, received)
}

func parseBybit(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Topic string          `json:"topic"`
		Type  string          `json:"type"`
		Time  json.RawMessage `json:"ts"`
		Data  struct {
			Symbol string     `json:"s"`
			Bids   [][]string `json:"b"`
			Asks   [][]string `json:"a"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Type != "snapshot" || len(message.Data.Bids) == 0 || len(message.Data.Asks) == 0 ||
		len(message.Data.Bids[0]) == 0 || len(message.Data.Asks[0]) == 0 {
		return BBO{}, false, nil
	}
	if message.Data.Symbol != "" && !strings.EqualFold(message.Data.Symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	bidQty, askQty := "", ""
	if len(message.Data.Bids[0]) > 1 {
		bidQty = message.Data.Bids[0][1]
	}
	if len(message.Data.Asks[0]) > 1 {
		askQty = message.Data.Asks[0][1]
	}
	return makeBBO(key, message.Data.Bids[0][0], message.Data.Asks[0][0], bidQty, askQty, message.Time, received)
}

func parseBitget(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Action   string `json:"action"`
		Argument struct {
			Instrument string `json:"instId"`
			Channel    string `json:"channel"`
		} `json:"arg"`
		Data []struct {
			Bids [][]string      `json:"bids"`
			Asks [][]string      `json:"asks"`
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
	if message.Argument.Channel != "books15" || message.Action != "snapshot" {
		return BBO{}, false, nil
	}
	for _, item := range message.Data {
		if len(item.Bids) > 0 && len(item.Asks) > 0 &&
			len(item.Bids[0]) > 0 && len(item.Asks[0]) > 0 {
			bidQty, askQty := "", ""
			if len(item.Bids[0]) > 1 {
				bidQty = item.Bids[0][1]
			}
			if len(item.Asks[0]) > 1 {
				askQty = item.Asks[0][1]
			}
			return makeBBO(key, item.Bids[0][0], item.Asks[0][0], bidQty, askQty, item.Time, received)
		}
	}
	return BBO{}, false, nil
}

func parseGate(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Channel string          `json:"channel"`
		Event   string          `json:"event"`
		Time    json.RawMessage `json:"time_ms"`
		Result  json.RawMessage `json:"result"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	expectedChannel := "spot.order_book"
	expectedEvent := "update"
	if key.Product == ProductPerpetual {
		expectedChannel = "futures.book_ticker"
	}
	if message.Channel != expectedChannel || message.Event != expectedEvent ||
		len(bytes.TrimSpace(message.Result)) == 0 {
		return BBO{}, false, nil
	}
	var result map[string]json.RawMessage
	if err := decodeJSON(message.Result, &result); err != nil {
		return BBO{}, false, err
	}
	symbol, err := rawString(result["s"])
	if err != nil {
		return BBO{}, false, err
	}
	if symbol == "" {
		symbol, err = rawString(result["contract"])
		if err != nil {
			return BBO{}, false, err
		}
	}
	if !strings.EqualFold(symbol, key.Symbol) {
		return BBO{}, false, nil
	}
	var bid, ask, bidQty, askQty string
	var bidOK, askOK bool
	if key.Product == ProductPerpetual {
		bid, err = rawString(result["b"])
		if err != nil {
			return BBO{}, false, err
		}
		ask, err = rawString(result["a"])
		if err != nil {
			return BBO{}, false, err
		}
		bidQty, err = rawString(result["B"])
		if err != nil {
			return BBO{}, false, err
		}
		askQty, err = rawString(result["A"])
		if err != nil {
			return BBO{}, false, err
		}
		bidOK, askOK = bid != "", ask != ""
		if len(bytes.TrimSpace(result["t"])) > 0 {
			message.Time = result["t"]
		}
	} else {
		bid, bidQty, bidOK, err = firstGateLevel(result["bids"])
		if err != nil {
			return BBO{}, false, err
		}
		ask, askQty, askOK, err = firstGateLevel(result["asks"])
		if err != nil {
			return BBO{}, false, err
		}
	}
	if !bidOK || !askOK {
		return BBO{}, false, nil
	}
	return makeBBO(key, bid, ask, bidQty, askQty, message.Time, received)
}

func parseHyperliquid(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Channel string `json:"channel"`
		Data    struct {
			Coin string          `json:"coin"`
			Time json.RawMessage `json:"time"`
			BBO  [2]*struct {
				Price string `json:"px"`
				Size  string `json:"sz"`
			} `json:"bbo"`
		} `json:"data"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Channel != "bbo" ||
		!strings.EqualFold(message.Data.Coin, key.Symbol) ||
		message.Data.BBO[0] == nil || message.Data.BBO[1] == nil {
		return BBO{}, false, nil
	}
	bid, ask := message.Data.BBO[0], message.Data.BBO[1]
	if !isPositiveDecimal(bid.Price) || !isPositiveDecimal(bid.Size) ||
		!isPositiveDecimal(ask.Price) || !isPositiveDecimal(ask.Size) {
		return BBO{}, false, nil
	}
	if comparison, err := compareDecimal(bid.Price, ask.Price); err != nil || comparison > 0 {
		return BBO{}, false, nil
	}
	return makeBBO(key, bid.Price, ask.Price, bid.Size, ask.Size, message.Data.Time, received)
}

func parseAster(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	var message struct {
		Event           string          `json:"e"`
		Symbol          string          `json:"s"`
		BidPrice        string          `json:"b"`
		BidQuantity     string          `json:"B"`
		AskPrice        string          `json:"a"`
		AskQuantity     string          `json:"A"`
		EventTime       json.RawMessage `json:"E"`
		TransactionTime json.RawMessage `json:"T"`
	}
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	if message.Event != "bookTicker" ||
		!strings.EqualFold(message.Symbol, key.Symbol) ||
		!isPositiveDecimal(message.BidPrice) || !isPositiveDecimal(message.AskPrice) ||
		!isPositiveDecimal(message.BidQuantity) || !isPositiveDecimal(message.AskQuantity) {
		return BBO{}, false, nil
	}
	if comparison, err := compareDecimal(
		message.BidPrice, message.AskPrice,
	); err != nil || comparison > 0 {
		return BBO{}, false, nil
	}
	timestamp := message.EventTime
	if len(bytes.TrimSpace(timestamp)) == 0 {
		timestamp = message.TransactionTime
	}
	return makeBBO(key, message.BidPrice, message.AskPrice, message.BidQuantity, message.AskQuantity, timestamp, received)
}

type lighterBookParser struct {
	mu          sync.Mutex
	hasSnapshot bool
	nonce       uint64
	bids        map[string]lighterLevel
	asks        map[string]lighterLevel
}

type lighterBookMessage struct {
	Type      string          `json:"type"`
	Channel   string          `json:"channel"`
	Timestamp json.RawMessage `json:"timestamp"`
	OrderBook struct {
		Nonce      json.Number    `json:"nonce"`
		BeginNonce json.Number    `json:"begin_nonce"`
		Bids       []lighterLevel `json:"bids"`
		Asks       []lighterLevel `json:"asks"`
	} `json:"order_book"`
}

type lighterLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

func parseLighter(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	parser := &lighterBookParser{}
	return parser.parse(key, payload, received)
}

func (p *lighterBookParser) parse(
	key Key,
	payload []byte,
	received time.Time,
) (BBO, bool, error) {
	var message lighterBookMessage
	if err := decodeJSON(payload, &message); err != nil {
		return BBO{}, false, err
	}
	snapshot := message.Type == "subscribed/order_book"
	delta := message.Type == "update/order_book"
	if !snapshot && !delta {
		return BBO{}, false, nil
	}
	if !strings.HasPrefix(message.Channel, "order_book:") {
		return BBO{}, false, nil
	}
	nonce, err := message.OrderBook.Nonce.Int64()
	if err != nil || nonce <= 0 {
		return BBO{}, false, fmt.Errorf("lighter order book nonce: %w", ErrSequenceGap)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if snapshot {
		p.bids = make(map[string]lighterLevel, len(message.OrderBook.Bids))
		p.asks = make(map[string]lighterLevel, len(message.OrderBook.Asks))
		p.hasSnapshot = true
	} else {
		beginNonce, parseErr := message.OrderBook.BeginNonce.Int64()
		if parseErr != nil || !p.hasSnapshot || beginNonce <= 0 ||
			uint64(beginNonce) != p.nonce {
			return BBO{}, false, fmt.Errorf(
				"%w: lighter begin_nonce=%s previous_nonce=%d nonce=%d",
				ErrSequenceGap, message.OrderBook.BeginNonce.String(), p.nonce, nonce,
			)
		}
	}
	if err := applyLighterLevels(p.bids, message.OrderBook.Bids); err != nil {
		return BBO{}, false, fmt.Errorf("%w: %v", ErrBookUnavailable, err)
	}
	if err := applyLighterLevels(p.asks, message.OrderBook.Asks); err != nil {
		return BBO{}, false, fmt.Errorf("%w: %v", ErrBookUnavailable, err)
	}
	p.nonce = uint64(nonce)

	bid, bidQty, bidOK, err := bestLighterLevel(p.bids, true)
	if err != nil {
		return BBO{}, false, fmt.Errorf("%w: %v", ErrBookUnavailable, err)
	}
	ask, askQty, askOK, err := bestLighterLevel(p.asks, false)
	if err != nil {
		return BBO{}, false, fmt.Errorf("%w: %v", ErrBookUnavailable, err)
	}
	if !bidOK || !askOK {
		if delta {
			return BBO{}, false, fmt.Errorf("%w: lighter empty book side", ErrBookUnavailable)
		}
		return BBO{}, false, nil
	}
	if comparison, err := compareDecimal(bid, ask); err != nil || comparison > 0 {
		return BBO{}, false, fmt.Errorf("%w: lighter crossed order book", ErrBookUnavailable)
	}
	return makeBBO(key, bid, ask, bidQty, askQty, message.Timestamp, received)
}

func applyLighterLevels(book map[string]lighterLevel, levels []lighterLevel) error {
	for _, level := range levels {
		if strings.TrimSpace(level.Price) == "" || strings.TrimSpace(level.Size) == "" {
			return fmt.Errorf("lighter order book level is missing price or size")
		}
		price, ok := new(big.Rat).SetString(level.Price)
		if !ok || price.Sign() <= 0 {
			return fmt.Errorf("invalid lighter price %q", level.Price)
		}
		size, ok := new(big.Rat).SetString(level.Size)
		if !ok || size.Sign() < 0 {
			return fmt.Errorf("invalid lighter size %q", level.Size)
		}
		priceKey := price.RatString()
		if size.Sign() == 0 {
			delete(book, priceKey)
		} else {
			book[priceKey] = lighterLevel{Price: level.Price, Size: level.Size}
		}
	}
	return nil
}

func bestLighterLevel(book map[string]lighterLevel, bid bool) (string, string, bool, error) {
	var bestPrice, bestSize string
	var best *big.Rat
	for _, level := range book {
		price, ok := new(big.Rat).SetString(level.Price)
		if !ok || price.Sign() <= 0 {
			return "", "", false, fmt.Errorf("invalid lighter price %q", level.Price)
		}
		if !isPositiveDecimal(level.Size) {
			continue
		}
		if best == nil || (bid && price.Cmp(best) > 0) || (!bid && price.Cmp(best) < 0) {
			bestPrice, bestSize, best = level.Price, level.Size, price
		}
	}
	return bestPrice, bestSize, best != nil, nil
}

func bestLighterPrice(book map[string]lighterLevel, bid bool) (string, bool, error) {
	price, _, ok, err := bestLighterLevel(book, bid)
	return price, ok, err
}

func isPositiveDecimal(value string) bool {
	number, ok := new(big.Rat).SetString(value)
	return ok && number.Sign() > 0
}

func compareDecimal(left, right string) (int, error) {
	leftNumber, leftOK := new(big.Rat).SetString(left)
	rightNumber, rightOK := new(big.Rat).SetString(right)
	if !leftOK || !rightOK {
		return 0, fmt.Errorf("invalid decimal comparison %q %q", left, right)
	}
	return leftNumber.Cmp(rightNumber), nil
}

func firstGateLevel(raw json.RawMessage) (string, string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", "", false, nil
	}
	var levels []json.RawMessage
	if err := decodeJSON(raw, &levels); err != nil {
		return "", "", false, err
	}
	if len(levels) == 0 {
		return "", "", false, nil
	}
	var tuple []json.RawMessage
	if err := decodeJSON(levels[0], &tuple); err == nil {
		if len(tuple) == 0 {
			return "", "", false, nil
		}
		price, err := rawString(tuple[0])
		if err != nil {
			return "", "", false, err
		}
		qty := ""
		if len(tuple) > 1 {
			qty, err = rawString(tuple[1])
			if err != nil {
				return "", "", false, err
			}
		}
		return price, qty, price != "", nil
	}
	var level map[string]json.RawMessage
	if err := decodeJSON(levels[0], &level); err != nil {
		return "", "", false, err
	}
	price, err := rawString(level["p"])
	if err != nil {
		return "", "", false, err
	}
	qty, err := rawString(level["s"])
	if err != nil {
		return "", "", false, err
	}
	return price, qty, price != "", nil
}

func firstGateLevelPrice(raw json.RawMessage) (string, bool, error) {
	price, _, ok, err := firstGateLevel(raw)
	return price, ok, err
}

func rawString(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", err
	}
	return number.String(), nil
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
	bidQty string,
	askQty string,
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
		BidQuantity:      bidQty,
		AskQuantity:      askQty,
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
