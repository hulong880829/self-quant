//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// TestStream_PostInfo_MatchesREST issues a Meta request over the WS
// PostInfo channel and compares the result to a REST Info.Meta call.
// Both must return the same number of perp assets.
func TestStream_PostInfo_MatchesREST(t *testing.T) {
	c := newStreamingClient(t)

	restMeta, err := c.Info.Meta()
	if err != nil {
		t.Fatalf("REST Meta: %v", err)
	}

	raw, err := c.Stream.PostInfo(map[string]any{"type": "meta"}, 10*time.Second)
	if err != nil {
		t.Fatalf("Stream.PostInfo: %v", err)
	}

	// Hyperliquid wraps the WS info payload one level deeper than REST.
	// Try the direct shape first; on mismatch, peel a layer of "data" /
	// "payload" / "meta" to find the universe array.
	var wsMeta struct {
		Universe []map[string]any `json:"universe"`
	}
	_ = json.Unmarshal(raw, &wsMeta)
	if len(wsMeta.Universe) == 0 {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err == nil {
			for _, key := range []string{"data", "payload", "meta", "response"} {
				if inner, ok := envelope[key]; ok {
					if err := json.Unmarshal(inner, &wsMeta); err == nil && len(wsMeta.Universe) > 0 {
						break
					}
				}
			}
		}
	}

	if len(wsMeta.Universe) != len(restMeta.Universe) {
		t.Errorf("universe length mismatch: ws=%d rest=%d (raw=%s)",
			len(wsMeta.Universe), len(restMeta.Universe), truncateLog(raw, 400))
	}
}

func truncateLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
