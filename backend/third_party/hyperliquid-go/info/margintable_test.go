package info

import (
	"encoding/json"
	"testing"
)

// TestMeta_UnmarshalMarginTablesTuple locks in that info.Meta
// default-unmarshals Hyperliquid's positional [id, {table}] marginTables
// form. This is what DecodeWebData2 relies on (its Meta field is a plain
// info.Meta); before MarginTable.UnmarshalJSON existed, a default
// json.Unmarshal died with "cannot unmarshal array into ... MarginTable".
func TestMeta_UnmarshalMarginTablesTuple(t *testing.T) {
	raw := []byte(`{"universe":[],"marginTables":[[50,{"description":"","marginTiers":[{"lowerBound":"0.0","maxLeverage":50}]}]]}`)
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Meta must default-unmarshal HL marginTables tuples: %v", err)
	}
	if len(m.MarginTables) != 1 {
		t.Fatalf("MarginTables len = %d, want 1", len(m.MarginTables))
	}
	mt := m.MarginTables[0]
	if mt.ID != 50 {
		t.Errorf("MarginTable.ID = %d, want 50", mt.ID)
	}
	if len(mt.MarginTiers) != 1 || mt.MarginTiers[0].MaxLeverage != 50 || mt.MarginTiers[0].LowerBound != "0.0" {
		t.Errorf("MarginTiers = %+v", mt.MarginTiers)
	}
}

// TestMarginTable_MalformedTuple ensures a wrong-shaped element surfaces
// an error rather than a silently zeroed table.
func TestMarginTable_MalformedTuple(t *testing.T) {
	var m Meta
	if err := json.Unmarshal([]byte(`{"universe":[],"marginTables":[[50]]}`), &m); err == nil {
		t.Fatal("expected error on 1-element marginTables tuple, got nil")
	}
}
