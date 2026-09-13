//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// TestApproveAgent_AndPlace generates a fresh agent via the owner's key,
// builds a second Client signing with the agent's private key but acting
// on behalf of the owner's account, places a far-from-mid ALO and
// cancels it.
func TestApproveAgent_AndPlace(t *testing.T) {
	owner := newClient(t)
	skipIfNoBalance(t, owner)

	// Hyperliquid caps agent names at 16 characters; use the trailing
	// six digits of the timestamp for uniqueness.
	name := fmt.Sprintf("itest-%06d", time.Now().UnixMilli()%1_000_000)
	agent, err := owner.Trade.ApproveAgent(name)
	if err != nil {
		// Hyperliquid caps active extra agents per account by cumulative
		// volume traded (default 3). Treat the cap as a skip — the SDK
		// path is the same regardless of whether the venue accepts the
		// new agent on this particular run.
		if strings.Contains(err.Error(), "Too many extra agents") {
			t.Skipf("account already at the agent cap: %v", err)
		}
		t.Fatalf("ApproveAgent: %v", err)
	}
	if agent.Address == "" || agent.PrivateKey == nil {
		t.Fatalf("ApproveAgent returned blank agent: %+v", agent)
	}

	cfg, _ := loadConfig()
	agentClient, err := hl.New(
		hl.WithBaseURL(cfg.BaseURL),
		hl.WithPrivateKey(agent.PrivateKey),
		hl.WithAccount(cfg.AccountAddr),
		hl.WithSkipStream(true),
	)
	if err != nil {
		t.Fatalf("hl.New(agent): %v", err)
	}

	coin := testCoin(t)
	m := mid(t, agentClient, coin)
	px := snapPrice(m*0.5, agentClient, coin)
	size := testSizeForLimit(t, agentClient, coin, px)

	// Agent approval needs a moment to propagate on L1 — retry a few
	// times with backoff before giving up.
	var res types.Result
	for attempt := 0; attempt < 6; attempt++ {
		res, err = agentClient.Trade.PlaceALO(coin, types.Buy, size, px)
		if err == nil && res.OID != 0 && res.Error == "" {
			break
		}
		if err == nil && res.Error != "" {
			t.Logf("attempt %d: agent placement returned error %q; retrying", attempt+1, res.Error)
		}
		time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("PlaceALO via agent (after retries): %v", err)
	}
	t.Cleanup(func() { cancelIfResting(t, agentClient, coin, res.OID) })
	if res.OID == 0 {
		t.Fatalf("PlaceALO returned no oid: %+v", res)
	}
	if _, err := agentClient.Trade.Cancel(coin, res.OID); err != nil {
		t.Fatalf("Cancel via agent: %v", err)
	}
}
