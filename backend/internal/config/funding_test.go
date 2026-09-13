package config

import "testing"

func TestFundingFromEnvDefaultsCollectNewVenuesWithoutPublishing(t *testing.T) {
	t.Setenv("ENABLED_EXCHANGES", "")
	t.Setenv("FUNDING_PUBLISHED_EXCHANGES", "")
	t.Setenv("FUNDING_SPREAD_ENABLED_EXCHANGES", "")
	t.Setenv("FUNDING_RANKING_ENABLED_EXCHANGES", "")
	cfg, err := FundingFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"binance", "okx", "bybit", "bitget", "gate", "hyperliquid"} {
		if !cfg.EnabledExchanges[name] || !cfg.PublishedExchanges[name] ||
			!cfg.SpreadEnabledExchanges[name] || !cfg.RankingEnabledExchanges[name] {
			t.Fatalf("default missing %s: %+v", name, cfg)
		}
	}
	for _, name := range []string{"aster", "lighter"} {
		if !cfg.EnabledExchanges[name] || cfg.PublishedExchanges[name] ||
			cfg.SpreadEnabledExchanges[name] || !cfg.RankingEnabledExchanges[name] {
			t.Fatalf("default must collect and rank %s without publishing: %+v", name, cfg)
		}
	}
	if cfg.EnabledExchanges["entropy"] || cfg.PublishedExchanges["entropy"] ||
		cfg.SpreadEnabledExchanges["entropy"] || cfg.RankingEnabledExchanges["entropy"] {
		t.Fatal("default funding sets must not include entropy")
	}
}

func TestFundingPublishedMustBeSubsetOfEnabled(t *testing.T) {
	t.Setenv("ENABLED_EXCHANGES", "binance,okx")
	t.Setenv("FUNDING_PUBLISHED_EXCHANGES", "aster")
	if _, err := FundingFromEnv(); err == nil {
		t.Fatal("published aster without collection must fail")
	}
}

func TestFundingRankingAllowsAsterAndLighterWhenCollected(t *testing.T) {
	t.Setenv("ENABLED_EXCHANGES", "binance,aster,lighter")
	t.Setenv("FUNDING_PUBLISHED_EXCHANGES", "binance")
	t.Setenv("FUNDING_SPREAD_ENABLED_EXCHANGES", "binance")
	t.Setenv("FUNDING_RANKING_ENABLED_EXCHANGES", "binance,aster,lighter")
	cfg, err := FundingFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RankingEnabledExchanges["aster"] || !cfg.RankingEnabledExchanges["lighter"] {
		t.Fatalf("ranking venues=%v", cfg.RankingEnabledExchanges)
	}
}

func TestFundingCollectOnlyAllowsNewVenuesWithoutPublish(t *testing.T) {
	t.Setenv("ENABLED_EXCHANGES", "binance,aster,lighter")
	t.Setenv("FUNDING_PUBLISHED_EXCHANGES", "binance")
	t.Setenv("FUNDING_SPREAD_ENABLED_EXCHANGES", "binance")
	t.Setenv("FUNDING_RANKING_ENABLED_EXCHANGES", "binance")
	cfg, err := FundingFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EnabledExchanges["aster"] || cfg.PublishedExchanges["aster"] {
		t.Fatalf("collect-only aster=%v published=%v", cfg.EnabledExchanges["aster"], cfg.PublishedExchanges["aster"])
	}
}
