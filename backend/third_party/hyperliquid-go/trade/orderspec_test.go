package trade

import (
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

func TestALOConstructor(t *testing.T) {
	s := ALO("BTC", types.Buy, 0.5, 50000, WithCloid("0xabc"))
	if s.Method != "alo" || s.TIF != types.TifAlo {
		t.Errorf("ALO: method/TIF = %s/%s", s.Method, s.TIF)
	}
	if s.Coin != "BTC" || s.Side != types.Buy || s.Size != 0.5 || s.Price != 50000 {
		t.Errorf("ALO core fields = %+v", s)
	}
	if s.Cloid != "0xabc" {
		t.Errorf("ALO cloid = %q", s.Cloid)
	}
}

func TestIOCConstructor(t *testing.T) {
	s := IOC("ETH", types.Sell, 1, 3000)
	if s.Method != "ioc" || s.TIF != types.TifIoc {
		t.Errorf("IOC: method/TIF = %s/%s", s.Method, s.TIF)
	}
	if s.Side != types.Sell {
		t.Errorf("IOC side = %s", s.Side)
	}
}

func TestGTCConstructor(t *testing.T) {
	s := GTC("SOL", types.Buy, 2, 150)
	if s.Method != "gtc" || s.TIF != types.TifGtc {
		t.Errorf("GTC: method/TIF = %s/%s", s.Method, s.TIF)
	}
}

func TestMarketConstructor(t *testing.T) {
	s := Market("BTC", types.Buy, 0.1, WithSlippage(0.02))
	if s.Method != "market" || s.TIF != types.TifIoc {
		t.Errorf("Market: method/TIF = %s/%s", s.Method, s.TIF)
	}
	if s.Price != 0 {
		t.Errorf("Market.Price should be unset until resolved against mid: %v", s.Price)
	}
	if s.Slippage != 0.02 {
		t.Errorf("Market slippage = %v", s.Slippage)
	}
}

func TestTriggerConstructor(t *testing.T) {
	s := Trigger("BTC", types.Sell, 0.5, 49000)
	if s.Method != "trigger" {
		t.Errorf("Trigger method = %s", s.Method)
	}
	if s.TriggerPx != 49000 {
		t.Errorf("Trigger px = %v", s.TriggerPx)
	}
	if !s.IsMarket {
		t.Errorf("Trigger default should be IsMarket=true")
	}
	if s.Price != 49000 {
		t.Errorf("Trigger seeds Price with triggerPx for stop-market: %v", s.Price)
	}
}

func TestTriggerWithAsLimit(t *testing.T) {
	s := Trigger("BTC", types.Sell, 0.5, 49000, AsLimit(48900))
	if s.IsMarket {
		t.Errorf("AsLimit must turn off IsMarket")
	}
	if s.Price != 48900 {
		t.Errorf("AsLimit price = %v, want 48900", s.Price)
	}
	if s.TriggerPx != 49000 {
		t.Errorf("Trigger px preserved: %v", s.TriggerPx)
	}
}

func TestConstructorsApplyBracketOpts(t *testing.T) {
	s := GTC("BTC", types.Buy, 1, 100, WithBracket(110, 90), WithTPCloid("tp"), WithSLCloid("sl"))
	if s.TakeProfit != 110 || s.StopLoss != 90 {
		t.Errorf("bracket prices not applied: %+v", s)
	}
	if s.TPCloid != "tp" || s.SLCloid != "sl" {
		t.Errorf("bracket cloids not applied: %+v", s)
	}
}
