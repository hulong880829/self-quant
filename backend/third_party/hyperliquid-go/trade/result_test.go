package trade

import (
	"encoding/json"
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

func TestResultFromResponse_Nil(t *testing.T) {
	r := resultFromResponse(nil)
	if r != (types.Result{}) {
		t.Errorf("nil response should yield zero Result, got %+v", r)
	}
}

func TestResultFromResponse_Error(t *testing.T) {
	resp := &types.APIResponse[OrderResponse]{Ok: false, Err: "boom"}
	r := resultFromResponse(resp)
	if r.Error != "boom" {
		t.Errorf("Error = %q", r.Error)
	}
}

func TestResultFromResponse_RestingOrder(t *testing.T) {
	// JSON field for cloid on the wire is "cid".
	raw := json.RawMessage(`{"resting":{"oid":42,"cid":"0xabc","status":"open"}}`)
	resp := &types.APIResponse[OrderResponse]{Ok: true, Data: OrderResponse{Statuses: types.MixedArray{
		types.MixedValue(raw),
	}}}
	r := resultFromResponse(resp)
	if r.OID != 42 || r.Cloid != "0xabc" || r.Status != "open" {
		t.Errorf("resting parse mismatch: %+v", r)
	}
}

func TestResultFromResponse_FilledOrder(t *testing.T) {
	raw := json.RawMessage(`{"filled":{"oid":7,"avgPx":"100.5","totalSz":"0.5"}}`)
	resp := &types.APIResponse[OrderResponse]{Ok: true, Data: OrderResponse{Statuses: types.MixedArray{
		types.MixedValue(raw),
	}}}
	r := resultFromResponse(resp)
	if r.OID != 7 || r.AvgPx != "100.5" || r.TotalSz != "0.5" || r.Status != "filled" {
		t.Errorf("filled parse mismatch: %+v", r)
	}
}

func TestResultFromResponse_StringStatus(t *testing.T) {
	// String statuses are forwarded as the raw JSON value (including
	// surrounding quotes) — this locks the current behaviour.
	raw := json.RawMessage(`"waitingForOpen"`)
	resp := &types.APIResponse[OrderResponse]{Ok: true, Data: OrderResponse{Statuses: types.MixedArray{
		types.MixedValue(raw),
	}}}
	r := resultFromResponse(resp)
	if r.Status != `"waitingForOpen"` {
		t.Errorf("string status = %q", r.Status)
	}
}

func TestBatchResultFromResponse_Multi(t *testing.T) {
	resp := &types.APIResponse[OrderResponse]{Ok: true, Data: OrderResponse{Statuses: types.MixedArray{
		types.MixedValue(`{"resting":{"oid":1,"cid":"","status":"open"}}`),
		types.MixedValue(`{"filled":{"oid":2,"avgPx":"100","totalSz":"1"}}`),
	}}}
	br := batchResultFromResponse(resp)
	if len(br.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(br.Results))
	}
	if br.Results[0].OID != 1 || br.Results[0].Status != "open" {
		t.Errorf("leg 0: %+v", br.Results[0])
	}
	if br.Results[1].OID != 2 || br.Results[1].Status != "filled" {
		t.Errorf("leg 1: %+v", br.Results[1])
	}
}

func TestBatchResultFromResponse_Error(t *testing.T) {
	resp := &types.APIResponse[OrderResponse]{Ok: false, Err: "rejected"}
	br := batchResultFromResponse(resp)
	if br.Error != "rejected" {
		t.Errorf("Error = %q", br.Error)
	}
}

func TestCancelResultOrError_Nil(t *testing.T) {
	r, err := cancelResultOrError(nil)
	if r != (types.CancelResult{}) {
		t.Errorf("expected zero result, got %+v", r)
	}
	if err == nil {
		t.Errorf("expected error on nil response")
	}
}

func TestCancelResultOrError_TransportError(t *testing.T) {
	r, err := cancelResultOrError(&types.APIResponse[CancelOrderResponse]{Ok: false, Err: "bad oid"})
	if r.Error != "bad oid" {
		t.Errorf("Error = %q", r.Error)
	}
	if err == nil {
		t.Errorf("expected error when resp.Ok == false")
	}
}

func TestCancelResultOrError_Success(t *testing.T) {
	resp := &types.APIResponse[CancelOrderResponse]{Ok: true, Data: CancelOrderResponse{Statuses: types.MixedArray{
		types.MixedValue(`"success"`),
	}}}
	r, err := cancelResultOrError(resp)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// The raw MixedValue is preserved verbatim, including the surrounding
	// JSON quotes — callers parse it if they need the unquoted form.
	if r.Status != `"success"` {
		t.Errorf("Status = %q", r.Status)
	}
}

func TestCancelResultOrError_PerOrderError(t *testing.T) {
	resp := &types.APIResponse[CancelOrderResponse]{Ok: true, Data: CancelOrderResponse{Statuses: types.MixedArray{
		types.MixedValue(`{"error":"Order was never placed, already canceled, or filled. asset=1"}`),
	}}}
	r, err := cancelResultOrError(resp)
	if err == nil {
		t.Fatalf("expected error from {\"error\": ...} status")
	}
	if r.Error == "" {
		t.Errorf("expected r.Error to be populated, got empty")
	}
}

func TestCancelBatchFromResponse_Multi(t *testing.T) {
	resp := &types.APIResponse[CancelOrderResponse]{Ok: true, Data: CancelOrderResponse{Statuses: types.MixedArray{
		types.MixedValue(`"success"`),
		types.MixedValue(`"success"`),
	}}}
	br := cancelBatchFromResponse(resp)
	if len(br.Results) != 2 {
		t.Errorf("len(Results) = %d", len(br.Results))
	}
}
