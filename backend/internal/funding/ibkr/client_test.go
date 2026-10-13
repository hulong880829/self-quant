package ibkr

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestLookupDedupesSameConID(t *testing.T) {
	contracts := lookupAgainst(t, func(conn net.Conn) {
		writeReady(t, conn, "9\x001\x001\x00")
		if _, err := readFields(conn); err != nil {
			t.Error(err)
			return
		}
		detail := "10\x001\x00AAPL\x00STK\x00\x000\x00\x00SMART\x00USD\x00AAPL\x00NMS\x00NMS\x00265598\x000.01\x00\x00\x00\x001\x000\x00Apple\x00NASDAQ\x00"
		_ = writePrefixed(conn, detail)
		_ = writePrefixed(conn, detail)
		_ = writePrefixed(conn, "52\x008\x001\x00")
	})
	if len(contracts) != 2 || contracts[0].ConID != 265598 || contracts[1].ConID != 265598 {
		t.Fatalf("contracts=%+v", contracts)
	}
	if contracts[0].SecType != "STK" || contracts[0].Currency != "USD" || contracts[0].PrimaryExchange != "NASDAQ" {
		t.Fatalf("contract=%+v", contracts[0])
	}
}

func TestLookupWaitsForManagedAccountsBeforeRequest(t *testing.T) {
	request := make(chan []string, 1)
	lookupAgainst(t, func(conn net.Conn) {
		rejectEarlyRequest(t, conn)
		writeReady(t, conn, "15\x001\x00DU12345\x00")
		fields, err := readFields(conn)
		if err != nil {
			t.Error(err)
			return
		}
		request <- fields
		_ = writePrefixed(conn, "52\x008\x001\x00")
	})
	fields := <-request
	if len(fields) == 0 || fields[0] != "9" || fields[4] != "AAPL" {
		t.Fatalf("request=%v", fields)
	}
}

func TestLookupIgnoresFarmStatusUntilReady(t *testing.T) {
	lookupAgainst(t, func(conn net.Conn) {
		rejectEarlyRequest(t, conn)
		_ = writePrefixed(conn, "4\x002\x00-1\x002104\x00farm ok\x00")
		_ = writePrefixed(conn, "52\x008\x001\x00")
		_ = writePrefixed(conn, "4\x002\x001\x00200\x00No security definition\x00")
		writeReady(t, conn, "9\x001\x001\x00")
		if _, err := readFields(conn); err != nil {
			t.Error(err)
			return
		}
		_ = writePrefixed(conn, "52\x008\x001\x00")
	})
}

func TestLookupEOFBeforeReady(t *testing.T) {
	listener := serveLookup(t, func(conn net.Conn) {
		if err := readHandshake(conn); err != nil {
			return
		}
		conn.Close()
	})
	_, err := NewClient(listener.Addr().String(), 30).LookupUSStock(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("expected EOF before ready")
	}
}

func TestLookupTimesOutBeforeReady(t *testing.T) {
	listener := serveLookup(t, func(conn net.Conn) {
		if err := readHandshake(conn); err != nil {
			return
		}
		time.Sleep(time.Second)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := NewClient(listener.Addr().String(), 30).LookupUSStock(ctx, "AAPL")
	if err == nil {
		t.Fatal("expected timeout before ready")
	}
}

func TestLookupFatalErrorBeforeReady(t *testing.T) {
	listener := serveLookup(t, func(conn net.Conn) {
		if err := readHandshake(conn); err != nil {
			return
		}
		_ = writePrefixed(conn, "4\x002\x00-1\x00502\x00fatal\x00")
	})
	_, err := NewClient(listener.Addr().String(), 30).LookupUSStock(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("expected fatal error")
	}
}

func TestLookupRequestUsesContractDetailsOnly(t *testing.T) {
	fields := contractRequestFields("AAPL", 176)
	if fields[0] != "9" || fields[3] != "0" || fields[4] != "AAPL" || fields[5] != "STK" || fields[10] != "SMART" || fields[12] != "USD" {
		t.Fatalf("request=%v", fields)
	}
	byID := contractQueryFields(Query{ConID: 265598}, 176)
	if byID[3] != "265598" || byID[4] != "" {
		t.Fatalf("conId request=%v", byID)
	}
	if _, err := strconv.Atoi(fields[2]); err != nil || fields[2] != "1" {
		t.Fatalf("req id=%v", fields[2])
	}
	cash := contractQueryFields(Query{Symbol: "USD", SecType: "CASH", Currency: "JPY", Exchange: "IDEALPRO"}, 176)
	if cash[5] != "CASH" || cash[10] != "IDEALPRO" || cash[12] != "JPY" {
		t.Fatalf("cash=%v", cash)
	}
	metal := contractQueryFields(Query{Symbol: "XAUUSD", SecType: "CMDTY", Currency: "USD", Exchange: "SMART"}, 176)
	if metal[5] != "CMDTY" || metal[10] != "SMART" || metal[4] != "XAUUSD" {
		t.Fatalf("metal=%v", metal)
	}
}

func lookupAgainst(t *testing.T, script func(net.Conn)) []Contract {
	t.Helper()
	listener := serveLookup(t, func(conn net.Conn) {
		if err := readHandshake(conn); err != nil {
			return
		}
		script(conn)
	})
	contracts, err := NewClient(listener.Addr().String(), 30).LookupUSStock(context.Background(), "AAPL")
	if err != nil {
		t.Fatal(err)
	}
	return contracts
}

func serveLookup(t *testing.T, script func(net.Conn)) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}()
	return listener
}

func readHandshake(conn net.Conn) error {
	handshake := make([]byte, 4)
	if _, err := readFull(conn, handshake); err != nil {
		return err
	}
	if string(handshake) != "API\x00" {
		return io.ErrUnexpectedEOF
	}
	if _, err := readFields(conn); err != nil {
		return err
	}
	if err := writePrefixed(conn, "176\x0020261008-00:00:00\x00"); err != nil {
		return err
	}
	_, err := readFields(conn)
	return err
}

func rejectEarlyRequest(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	defer conn.SetReadDeadline(time.Time{})
	if _, err := readFields(conn); err == nil {
		t.Error("contract request arrived before ready")
	}
}

func writeReady(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if err := writePrefixed(conn, payload); err != nil {
		t.Error(err)
	}
}

func readFull(r io.Reader, buf []byte) (int, error) {
	return io.ReadFull(r, buf)
}
