package ibkr

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	minClientVersion = 100
	maxClientVersion = 178
	sizeRulesVersion = 164
	issuerIDVersion  = 176

	lookupTimeout = 20 * time.Second
)

type Contract struct {
	ConID           int64
	Symbol          string
	SecType         string
	Currency        string
	PrimaryExchange string
	LocalSymbol     string
	TradingClass    string
	LongName        string
}

type Query struct {
	ConID    int64
	SecType  string
	Symbol   string
	Currency string
	Exchange string
}

type Client struct {
	addr     string
	clientID int
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
}

func NewClient(addr string, clientID int) *Client {
	return &Client{addr: addr, clientID: clientID, dial: (&net.Dialer{}).DialContext}
}

func (c *Client) LookupUSStock(ctx context.Context, symbol string) ([]Contract, error) {
	return c.Lookup(ctx, Query{SecType: "STK", Symbol: symbol, Currency: "USD"})
}

func (c *Client) Lookup(ctx context.Context, query Query) ([]Contract, error) {
	query.Symbol = strings.ToUpper(strings.TrimSpace(query.Symbol))
	query.SecType = strings.ToUpper(strings.TrimSpace(query.SecType))
	query.Currency = strings.ToUpper(strings.TrimSpace(query.Currency))
	query.Exchange = strings.ToUpper(strings.TrimSpace(query.Exchange))
	if query.Exchange == "" {
		if query.SecType == "CASH" {
			query.Exchange = "IDEALPRO"
		} else {
			query.Exchange = "SMART"
		}
	}
	if query.ConID < 0 {
		return nil, fmt.Errorf("ib contract query conId must be positive")
	}
	if query.ConID == 0 && (query.Symbol == "" || query.SecType == "" || query.Currency == "") {
		return nil, fmt.Errorf("ib contract query requires symbol, sec type, and currency")
	}
	conn, serverVersion, err := c.readySession(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := writeFields(conn, contractQueryFields(query, serverVersion)...); err != nil {
		return nil, err
	}
	return readContractDetails(conn, serverVersion)
}

func (c *Client) readySession(ctx context.Context) (net.Conn, int, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	conn, err := c.dial(attemptCtx, "tcp", c.addr)
	if err != nil {
		return nil, 0, fmt.Errorf("connect ib gateway: %w", err)
	}
	if deadline, ok := attemptCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := writeHandshake(conn); err != nil {
		conn.Close()
		return nil, 0, err
	}
	hello, err := readFields(conn)
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("read ib handshake: %w", err)
	}
	if len(hello) < 1 {
		conn.Close()
		return nil, 0, fmt.Errorf("empty ib handshake")
	}
	serverVersion, err := strconv.Atoi(hello[0])
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("ib server version: %w", err)
	}
	if err := writeFields(conn, startAPIFields(c.clientID)...); err != nil {
		conn.Close()
		return nil, 0, err
	}
	if err := waitUntilReady(conn); err != nil {
		conn.Close()
		return nil, 0, err
	}
	return conn, serverVersion, nil
}

func waitUntilReady(reader io.Reader) error {
	for {
		fields, err := readFields(reader)
		if err != nil {
			return fmt.Errorf("ib session not ready: %w", err)
		}
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "9", "15":
			return nil
		case "4":
			code, _, fatal := classifyError(fields)
			if code == 200 || !fatal {
				continue
			}
			return fmt.Errorf("ib contract lookup failed: code %d", code)
		}
	}
}

func readContractDetails(reader io.Reader, serverVersion int) ([]Contract, error) {
	var contracts []Contract
	for {
		fields, err := readFields(reader)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "10":
			contract, err := decodeContractDetails(serverVersion, fields)
			if err != nil {
				return nil, err
			}
			contracts = append(contracts, contract)
		case "52":
			return contracts, nil
		case "4":
			code, reqID, fatal := classifyError(fields)
			if code == 200 && (reqID == 1 || reqID == -1) {
				return contracts, nil
			}
			if fatal {
				return nil, fmt.Errorf("ib contract lookup failed: code %d", code)
			}
		}
	}
}

func writeHandshake(w io.Writer) error {
	version := fmt.Sprintf("v%d..%d", minClientVersion, maxClientVersion)
	if _, err := io.WriteString(w, "API\x00"); err != nil {
		return err
	}
	return writePrefixed(w, version)
}

func startAPIFields(clientID int) []string {
	return []string{"71", "2", strconv.Itoa(clientID), ""}
}

func contractRequestFields(symbol string, serverVersion int) []string {
	return contractQueryFields(Query{Symbol: symbol, SecType: "STK", Currency: "USD"}, serverVersion)
}

func contractQueryFields(query Query, serverVersion int) []string {
	venue := strings.ToUpper(strings.TrimSpace(query.Exchange))
	if venue == "" {
		if strings.EqualFold(query.SecType, "CASH") {
			venue = "IDEALPRO"
		} else {
			venue = "SMART"
		}
	}
	conID := "0"
	if query.ConID > 0 {
		conID = strconv.FormatInt(query.ConID, 10)
	}
	fields := []string{
		"9", "8", "1",
		conID, query.Symbol, query.SecType, "", "", "", "",
		venue, "", query.Currency, "", "",
		"0", "", "",
	}
	if serverVersion >= issuerIDVersion {
		fields = append(fields, "")
	}
	return fields
}

func decodeContractDetails(serverVersion int, fields []string) (Contract, error) {
	if serverVersion < sizeRulesVersion {
		return Contract{}, fmt.Errorf("ib server version %d is below %d", serverVersion, sizeRulesVersion)
	}
	const offset = 1
	if len(fields) < offset+20 {
		return Contract{}, fmt.Errorf("short contract details message")
	}
	conID, err := strconv.ParseInt(fields[offset+11], 10, 64)
	if err != nil {
		return Contract{}, fmt.Errorf("contract con_id: %w", err)
	}
	return Contract{
		Symbol:          fields[offset+1],
		SecType:         fields[offset+2],
		Currency:        fields[offset+7],
		LocalSymbol:     fields[offset+8],
		TradingClass:    fields[offset+10],
		ConID:           conID,
		PrimaryExchange: fields[offset+19],
		LongName:        fields[offset+18],
	}, nil
}

func classifyError(fields []string) (code int, reqID int, fatal bool) {
	if len(fields) < 5 {
		return 0, -1, true
	}
	reqID, _ = strconv.Atoi(fields[2])
	code, _ = strconv.Atoi(fields[3])
	if code >= 2100 && code < 2200 {
		return code, reqID, false
	}
	return code, reqID, true
}

func writeFields(w io.Writer, fields ...string) error {
	return writePrefixed(w, strings.Join(fields, "\x00")+"\x00")
}

func writePrefixed(w io.Writer, payload string) error {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, payload)
	return err
}

func readFields(r io.Reader) ([]string, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length > 8<<20 {
		return nil, fmt.Errorf("ib message too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	parts := strings.Split(string(payload), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, nil
}
