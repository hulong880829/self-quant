package types

// Result is the trader-friendly outcome of a single placement call.
// Fields are populated best-effort from the underlying server response;
// callers should check Error before relying on OID / AvgPx.
type Result struct {
	OID     int64
	Cloid   string
	Status  string
	AvgPx   string
	TotalSz string
	Error   string
}

// BatchResult is the outcome of a multi-leg placement call. One Result
// per leg is returned, in the same order as the input specs.
type BatchResult struct {
	Results []Result
	Error   string
}

// CancelResult is the outcome of a single cancel call.
type CancelResult struct {
	Status string
	Error  string
}

// BatchCancelResult is the outcome of a multi-cancel call. One
// CancelResult per cancellation attempt is returned.
type BatchCancelResult struct {
	Results []CancelResult
	Error   string
}
