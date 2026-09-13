package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"selfquant/backend/internal/funding/ranking"
)

func TestHistoryUpdateShouldWarn(t *testing.T) {
	if historyUpdateShouldWarn(nil) {
		t.Fatal("nil error must not warn")
	}
	deferred := fmt.Errorf(
		"%w until %s",
		ranking.ErrHistoryRetryDeferred,
		time.Unix(0, 0).UTC().Format(time.RFC3339),
	)
	if historyUpdateShouldWarn(deferred) {
		t.Fatal("deferred history retry must not warn or mark failure")
	}
	if !historyUpdateShouldWarn(errors.New("clickhouse unavailable")) {
		t.Fatal("unrelated history errors must warn")
	}
}

func TestIncrementalHistoryAllowed(t *testing.T) {
	if incrementalHistoryAllowed(0) {
		t.Fatal("generation 0 must skip 5m incremental")
	}
	if !incrementalHistoryAllowed(1) {
		t.Fatal("published generation must allow incremental")
	}
}

func TestRankingTimeoutContextsAreIndependent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	history, compute, cancelHistory, cancelCompute := rankingTimeoutContexts(parent, time.Minute)
	defer cancelCompute()
	cancelHistory()
	if history.Err() == nil {
		t.Fatal("history context must cancel")
	}
	if compute.Err() != nil {
		t.Fatal("compute context must stay independent of history cancel")
	}
}
