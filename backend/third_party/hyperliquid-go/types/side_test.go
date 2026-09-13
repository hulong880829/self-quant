package types

import "testing"

func TestSideIsBuy(t *testing.T) {
	if !Buy.IsBuy() {
		t.Errorf("Buy.IsBuy() must be true")
	}
	if Sell.IsBuy() {
		t.Errorf("Sell.IsBuy() must be false")
	}
}

func TestSideWireConstants(t *testing.T) {
	if string(Buy) != "B" {
		t.Errorf("Buy wire = %q, want %q", string(Buy), "B")
	}
	if string(Sell) != "A" {
		t.Errorf("Sell wire = %q, want %q", string(Sell), "A")
	}
	if SideBid != Buy {
		t.Errorf("SideBid alias diverged from Buy")
	}
	if SideAsk != Sell {
		t.Errorf("SideAsk alias diverged from Sell")
	}
}

func TestTIFConstants(t *testing.T) {
	if tifALO != "Alo" || tifIOC != "Ioc" || tifGTC != "Gtc" {
		t.Errorf("TIF wire constants drifted: %s/%s/%s", tifALO, tifIOC, tifGTC)
	}
}

func TestMarginMode(t *testing.T) {
	if Cross == Isolated {
		t.Errorf("Cross and Isolated must differ")
	}
	if Cross != 0 || Isolated != 1 {
		t.Errorf("MarginMode iota order changed: Cross=%d Isolated=%d", Cross, Isolated)
	}
}
