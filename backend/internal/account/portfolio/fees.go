package portfolio

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

const (
	FeeStatusUnknown     = "unknown"
	FeeStatusOK          = "ok"
	FeeStatusUnsupported = "unsupported"

	FeeMarketSpot     = "spot"
	FeeMarketContract = "contract"
)

type MarketFee struct {
	Status string
	Maker  string
	Taker  string
}

type AccountFeeRates struct {
	Spot     MarketFee
	Contract MarketFee
	Markets  map[string]MarketFee
	Source   string
}

type FeeRateReader interface {
	AccountFeeRates(context.Context, Credentials) (AccountFeeRates, error)
}

func (r *Registry) AccountFeeRates(
	ctx context.Context,
	exchange string,
	credentials Credentials,
) (AccountFeeRates, error) {
	adapter, ok := r.adapters[strings.ToLower(strings.TrimSpace(exchange))]
	if !ok {
		return AccountFeeRates{}, ErrUnsupported
	}
	if limited, ok := adapter.(*limitedAdapter); ok {
		adapter = limited.delegate
	}
	reader, ok := adapter.(FeeRateReader)
	if !ok {
		return AccountFeeRates{}, ErrUnsupported
	}
	return reader.AccountFeeRates(ctx, credentials)
}

func unsupportedMarket() MarketFee {
	return MarketFee{Status: FeeStatusUnsupported}
}

func unknownMarket() MarketFee {
	return MarketFee{Status: FeeStatusUnknown}
}

func parseMarketFee(maker, taker string) (MarketFee, error) {
	maker = strings.TrimSpace(maker)
	taker = strings.TrimSpace(taker)
	if maker == "" || taker == "" {
		return MarketFee{}, fmt.Errorf("missing maker/taker fee")
	}
	makerDec, err := decimal.NewFromString(maker)
	if err != nil {
		return MarketFee{}, fmt.Errorf("invalid maker fee %q", maker)
	}
	takerDec, err := decimal.NewFromString(taker)
	if err != nil {
		return MarketFee{}, fmt.Errorf("invalid taker fee %q", taker)
	}
	return MarketFee{
		Status: FeeStatusOK,
		Maker:  makerDec.String(),
		Taker:  takerDec.String(),
	}, nil
}

func firstNonEmptyFee(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
