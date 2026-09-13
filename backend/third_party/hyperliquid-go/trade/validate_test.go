package trade

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/info"
	xtransport "github.com/Simon-Busch/hyperliquid-go/internal/transport"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// stubInfo builds a minimal *info.Client with the supplied (coin, sz-decimals)
// table registered. Asset ids are assigned sequentially starting at 0;
// the first coin therefore has id 0 (the legitimate "first asset" case
// guarded by isFirstAsset).
func stubInfo(t *testing.T, baseURL string, coins map[string]int) *info.Client {
	t.Helper()
	coinToAsset := make(map[string]int)
	nameToCoin := make(map[string]string)
	assetToDecimal := make(map[int]int)
	// Stable id assignment so callers can rely on the first listed coin
	// being asset 0.
	id := 0
	for c, sz := range coins {
		coinToAsset[c] = id
		nameToCoin[c] = c
		assetToDecimal[id] = sz
		id++
	}
	return info.NewForTest(xtransport.New(baseURL, nil), coinToAsset, nameToCoin, assetToDecimal)
}

// newUserStateServer returns an httptest server that responds to
// /info clearinghouseState lookups with the supplied UserState.
func newUserStateServer(t *testing.T, state info.UserState) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(state)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertValidationCode checks that err is a *types.ValidationError with
// the expected Code. Fails the test otherwise.
func assertValidationCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected ValidationError code=%q, got nil", want)
	}
	var ve *types.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *types.ValidationError, got %T: %v", err, err)
	}
	if ve.Code != want {
		t.Fatalf("ValidationError.Code = %q, want %q (full err: %v)", ve.Code, want, err)
	}
}

func TestValidateOptionCompatibility(t *testing.T) {
	cases := []struct {
		name string
		spec types.OrderSpec
		want string // "" means no error
	}{
		{"slippage on PlaceMarket allowed", types.OrderSpec{Method: "market", Slippage: 0.05}, ""},
		{"slippage on ClosePosition allowed", types.OrderSpec{Method: "close", Slippage: 0.05}, ""},
		{"slippage on PlaceALO rejected", types.OrderSpec{Method: "alo", Slippage: 0.05}, "unsupported_option"},
		{"WithSize on Modify allowed", types.OrderSpec{Method: "modify", OverrideSize: 0.5}, ""},
		{"WithSize on Close allowed", types.OrderSpec{Method: "close", OverrideSize: 0.5}, ""},
		{"WithSize on PlaceGTC rejected", types.OrderSpec{Method: "gtc", OverrideSize: 0.5}, "unsupported_option"},
		{"WithLimit on Modify allowed", types.OrderSpec{Method: "modify", LimitPrice: 100}, ""},
		{"WithLimit on Close allowed", types.OrderSpec{Method: "close", LimitPrice: 100}, ""},
		{"WithLimit on PlaceIOC rejected", types.OrderSpec{Method: "ioc", LimitPrice: 100}, "unsupported_option"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOptionCompatibility(&tc.spec)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			assertValidationCode(t, err, tc.want)
		})
	}
}

func TestValidateModify(t *testing.T) {
	t.Run("missing target", func(t *testing.T) {
		err := validateModify(&types.OrderSpec{Method: "modify"})
		assertValidationCode(t, err, "modify_target_required")
	})
	t.Run("oid only without change", func(t *testing.T) {
		err := validateModify(&types.OrderSpec{Method: "modify", ModifyOID: 42})
		assertValidationCode(t, err, "modify_no_change")
	})
	t.Run("cloid only without change", func(t *testing.T) {
		err := validateModify(&types.OrderSpec{Method: "modify", ModifyCloid: "0xabc"})
		assertValidationCode(t, err, "modify_no_change")
	})
	t.Run("oid + WithLimit ok", func(t *testing.T) {
		if err := validateModify(&types.OrderSpec{Method: "modify", ModifyOID: 42, LimitPrice: 100}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("oid + WithSize ok", func(t *testing.T) {
		if err := validateModify(&types.OrderSpec{Method: "modify", ModifyOID: 42, OverrideSize: 0.5}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestValidateBracket(t *testing.T) {
	t.Run("no bracket skipped", func(t *testing.T) {
		if err := validateBracket(&types.OrderSpec{Side: types.Buy, Price: 100, Size: 1}); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("entry=0 skipped", func(t *testing.T) {
		if err := validateBracket(&types.OrderSpec{Side: types.Buy, Size: 1, TakeProfit: 110, StopLoss: 90}); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("Buy tp must exceed entry", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Buy, Price: 100, Size: 1, TakeProfit: 90})
		assertValidationCode(t, err, "tp_wrong_side_buy")
	})
	t.Run("Buy sl must be below entry", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Buy, Price: 100, Size: 1, StopLoss: 110})
		assertValidationCode(t, err, "sl_wrong_side_buy")
	})
	t.Run("Sell tp must be below entry", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Sell, Price: 100, Size: 1, TakeProfit: 110})
		assertValidationCode(t, err, "tp_wrong_side_sell")
	})
	t.Run("Sell sl must exceed entry", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Sell, Price: 100, Size: 1, StopLoss: 90})
		assertValidationCode(t, err, "sl_wrong_side_sell")
	})
	t.Run("TPSize exceeds entry", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Buy, Price: 100, Size: 1, TakeProfit: 110, TPSize: 2})
		assertValidationCode(t, err, "bracket_size_exceeds_entry")
	})
	t.Run("SLSize exceeds entry", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Buy, Price: 100, Size: 1, StopLoss: 90, SLSize: 2})
		assertValidationCode(t, err, "bracket_size_exceeds_entry")
	})
	t.Run("partial bracket sizes ok", func(t *testing.T) {
		err := validateBracket(&types.OrderSpec{Side: types.Buy, Price: 100, Size: 1, TakeProfit: 110, StopLoss: 90, TPSize: 0.5, SLSize: 0.5})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
}

func TestValidateSignificantFigures(t *testing.T) {
	t.Run("zero ok", func(t *testing.T) {
		if err := validateSignificantFigures(0); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("five sf ok", func(t *testing.T) {
		if err := validateSignificantFigures(12345); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("six sf rejected", func(t *testing.T) {
		err := validateSignificantFigures(123456)
		assertValidationCode(t, err, "significant_figures")
	})
	t.Run("0.12345 ok", func(t *testing.T) {
		if err := validateSignificantFigures(0.12345); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
}

func TestIsMultipleOf(t *testing.T) {
	if !isMultipleOf(0.3, 0.1) {
		t.Errorf("0.3 should be a multiple of 0.1")
	}
	if isMultipleOf(0.05, 0.1) {
		t.Errorf("0.05 should not be a multiple of 0.1")
	}
	if !isMultipleOf(1, 0) {
		t.Errorf("zero step is treated as no constraint")
	}
}

func TestValidatePositionState_ReduceOnly(t *testing.T) {
	state := &info.UserState{
		AssetPositions: []info.AssetPosition{
			{Position: info.Position{Coin: "BTC", Szi: "0.5"}},
		},
	}
	tr := NewForTest(nil, nil, state, "")
	// Buy reduce-only on a long position must reject.
	err := tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Side: types.Buy, ReduceOnly: true, Method: "ioc"})
	assertValidationCode(t, err, "wrong_side_for_reduce")
	// Sell reduce-only on a long position is fine.
	if err := tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Side: types.Sell, ReduceOnly: true, Method: "ioc"}); err != nil {
		t.Fatalf("Sell reduce-only on long should pass: %v", err)
	}
}

func TestValidatePositionState_ReduceOnly_Short(t *testing.T) {
	state := &info.UserState{
		AssetPositions: []info.AssetPosition{
			{Position: info.Position{Coin: "BTC", Szi: "-0.5"}},
		},
	}
	tr := NewForTest(nil, nil, state, "")
	// Sell reduce-only on a short position must reject.
	err := tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Side: types.Sell, ReduceOnly: true, Method: "ioc"})
	assertValidationCode(t, err, "wrong_side_for_reduce")
	// Buy reduce-only on a short position is fine.
	if err := tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Side: types.Buy, ReduceOnly: true, Method: "ioc"}); err != nil {
		t.Fatalf("Buy reduce-only on short should pass: %v", err)
	}
}

func TestValidatePositionState_Close(t *testing.T) {
	// no position -> no_position
	tr := NewForTest(nil, nil, &info.UserState{}, "")
	err := tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Method: "close"})
	assertValidationCode(t, err, "no_position")

	// zero szi position still no_position
	tr.userState = &info.UserState{AssetPositions: []info.AssetPosition{{Position: info.Position{Coin: "BTC", Szi: "0"}}}}
	err = tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Method: "close"})
	assertValidationCode(t, err, "no_position")

	// partial close larger than size -> close_size_exceeds_position
	tr.userState = &info.UserState{AssetPositions: []info.AssetPosition{{Position: info.Position{Coin: "BTC", Szi: "0.5"}}}}
	err = tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Method: "close", OverrideSize: 1})
	assertValidationCode(t, err, "close_size_exceeds_position")

	// nil userState -> rules skipped
	tr.userState = nil
	if err := tr.validatePositionState(&types.OrderSpec{Coin: "BTC", Method: "close", OverrideSize: 1}); err != nil {
		t.Fatalf("nil userState should skip rules: %v", err)
	}
}

func TestValidate_TopLevel_CoinAndSize(t *testing.T) {
	// httptest server returns a non-empty UserState so RefreshState succeeds.
	srv := newUserStateServer(t, info.UserState{})
	infoC := stubInfo(t, srv.URL, map[string]int{"BTC": 5, "ETH": 4})
	tr := NewForTest(infoC.Transport(), infoC, nil, "0xtest")

	// coin_required
	err := tr.validate(&types.OrderSpec{Method: "gtc", Size: 1, Price: 100})
	assertValidationCode(t, err, "coin_required")

	// unknown_coin
	err = tr.validate(&types.OrderSpec{Method: "gtc", Coin: "XRP", Size: 1, Price: 100})
	assertValidationCode(t, err, "unknown_coin")

	// price_non_positive
	err = tr.validate(&types.OrderSpec{Method: "gtc", Coin: "ETH", Side: types.Buy, Size: 1, Price: 0})
	assertValidationCode(t, err, "price_non_positive")

	// size_below_min: BTC has szDecimals=5 → MinSize 1e-5
	err = tr.validate(&types.OrderSpec{Method: "gtc", Coin: "BTC", Side: types.Buy, Size: 1e-6, Price: 100})
	assertValidationCode(t, err, "size_below_min")

	// size_step_violation: BTC step 1e-5; 1.000001 % 1e-5 != 0
	err = tr.validate(&types.OrderSpec{Method: "gtc", Coin: "BTC", Side: types.Buy, Size: 0.000015, Price: 100})
	assertValidationCode(t, err, "size_step_violation")

	// significant_figures: 6sf price
	err = tr.validate(&types.OrderSpec{Method: "gtc", Coin: "ETH", Side: types.Buy, Size: 0.1, Price: 123456})
	assertValidationCode(t, err, "significant_figures")

	// SkipValidate bypasses
	if err := tr.validate(&types.OrderSpec{SkipValidate: true, Method: "gtc"}); err != nil {
		t.Fatalf("SkipValidate should bypass: %v", err)
	}
}

func TestValidate_TopLevel_HappyPath(t *testing.T) {
	srv := newUserStateServer(t, info.UserState{})
	infoC := stubInfo(t, srv.URL, map[string]int{"ETH": 4})
	tr := NewForTest(infoC.Transport(), infoC, nil, "0xtest")

	// ETH has szDecimals=4 → MinSize 1e-4; size 0.01 is a multiple.
	if err := tr.validate(&types.OrderSpec{Method: "gtc", Coin: "ETH", Side: types.Buy, Size: 0.01, Price: 1234.5}); err != nil {
		t.Fatalf("happy path should pass: %v", err)
	}
}

func TestPositionFor(t *testing.T) {
	state := &info.UserState{AssetPositions: []info.AssetPosition{
		{Position: info.Position{Coin: "BTC", Szi: "0.5"}},
		{Position: info.Position{Coin: "ETH", Szi: "-1"}},
	}}
	p, szi := positionFor(state, "ETH")
	if p == nil || szi != -1 {
		t.Errorf("ETH position = %+v / %v", p, szi)
	}
	p, szi = positionFor(state, "SOL")
	if p != nil || szi != 0 {
		t.Errorf("SOL absent = %+v / %v", p, szi)
	}
}

func TestIsFirstAsset(t *testing.T) {
	infoC := info.NewForTest(nil, map[string]int{"BTC": 0, "ETH": 1}, nil, nil)
	if !isFirstAsset(infoC, "BTC") {
		t.Errorf("BTC should be the first asset")
	}
	if isFirstAsset(infoC, "ETH") {
		t.Errorf("ETH is not the first asset")
	}
	if isFirstAsset(nil, "BTC") {
		t.Errorf("nil info returns false")
	}
}
