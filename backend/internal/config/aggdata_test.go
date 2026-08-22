package config

import (
	"testing"
	"time"
)

func TestAggDataFairPriceDefaults(t *testing.T) {
	t.Setenv("MDS_GATEWAY_TOKEN", "test-token")
	config, err := AggDataFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !config.FairPriceEnabled || config.FairPriceDepth != 20 ||
		config.FairPriceStreamInterval != 5*time.Millisecond ||
		config.FairPriceRetention != 24*time.Hour ||
		config.FairPriceDeleteBatch != 10_000 {
		t.Fatalf("unexpected fair price defaults: %+v", config)
	}
}

func TestAggDataRejectsInvalidFairPriceValues(t *testing.T) {
	t.Setenv("MDS_GATEWAY_TOKEN", "test-token")
	t.Setenv("AGGDATA_FAIRPRICE_STREAM_INTERVAL", "500us")
	if _, err := AggDataFromEnv(); err == nil {
		t.Fatal("sub-millisecond fair price interval was accepted")
	}

	t.Setenv("AGGDATA_FAIRPRICE_STREAM_INTERVAL", "5ms")
	t.Setenv("AGGDATA_FAIRPRICE_LAMBDA_PER_BP", "not-a-number")
	if _, err := AggDataFromEnv(); err == nil {
		t.Fatal("invalid fair price lambda was accepted")
	}
}
