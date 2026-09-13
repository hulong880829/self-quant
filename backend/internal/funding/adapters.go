package funding

import (
	"errors"
	"fmt"
	"strings"

	"selfquant/backend/internal/config"
	"selfquant/backend/internal/exchange"
)

var (
	ErrAdapterNotRegistered = errors.New("funding adapter is not registered")
	ErrAdapterNotEnabled    = errors.New("funding adapter is not enabled")
	ErrExchangesRequired    = errors.New("exchanges are required")
)

func FundingAdapterList(cfg config.Funding) []exchange.Adapter {
	return []exchange.Adapter{
		exchange.NewBinance(cfg.HTTPTimeout),
		exchange.NewOKX(cfg.HTTPTimeout),
		exchange.NewBybit(cfg.HTTPTimeout),
		exchange.NewBitget(cfg.HTTPTimeout),
		exchange.NewGate(cfg.HTTPTimeout),
		exchange.NewHyperliquid(cfg.HTTPTimeout),
		exchange.NewEntropy(cfg.HTTPTimeout),
		exchange.NewAster(cfg.HTTPTimeout, cfg.AsterFundingAPIURL),
		exchange.NewLighter(cfg.HTTPTimeout, cfg.LighterFundingAPIURL),
	}
}

func FundingAdapters(cfg config.Funding) map[string]exchange.Adapter {
	list := FundingAdapterList(cfg)
	registry := make(map[string]exchange.Adapter, len(list))
	for _, adapter := range list {
		registry[adapter.Name()] = adapter
	}
	return registry
}

func EnabledFundingAdapters(cfg config.Funding) []exchange.Adapter {
	var enabled []exchange.Adapter
	for _, adapter := range FundingAdapterList(cfg) {
		if cfg.EnabledExchanges[adapter.Name()] {
			enabled = append(enabled, adapter)
		}
	}
	return enabled
}

func SelectEnabledAdapters(
	registry map[string]exchange.Adapter,
	enabled map[string]bool,
	requested []string,
) ([]exchange.Adapter, error) {
	names := normalizeRequestedExchanges(requested)
	if len(names) == 0 {
		return nil, ErrExchangesRequired
	}
	var missing, disabled []string
	seen := make(map[string]struct{}, len(names))
	selected := make([]exchange.Adapter, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		adapter, registered := registry[name]
		if !registered {
			missing = append(missing, name)
			continue
		}
		if !enabled[name] {
			disabled = append(disabled, name)
			continue
		}
		selected = append(selected, adapter)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrAdapterNotRegistered, strings.Join(missing, ","))
	}
	if len(disabled) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrAdapterNotEnabled, strings.Join(disabled, ","))
	}
	return selected, nil
}

func ParseExchangeList(value string) []string {
	return normalizeRequestedExchanges([]string{value})
}

func ZeroContractExchanges(requested []string, instruments []FundingInstrument) []string {
	counts := make(map[string]int, len(requested))
	for _, name := range requested {
		counts[name] = 0
	}
	for _, instrument := range instruments {
		counts[instrument.Exchange]++
	}
	var zero []string
	for _, name := range requested {
		if counts[name] == 0 {
			zero = append(zero, name)
		}
	}
	return zero
}

func normalizeRequestedExchanges(requested []string) []string {
	var names []string
	for _, raw := range requested {
		for _, part := range strings.Split(raw, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}
