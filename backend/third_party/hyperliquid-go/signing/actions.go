package signing

import (
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// Action structs with deterministic field ordering for consistent MessagePack serialization
// The order of fields in these structs is critical for signature generation

// CancelOrderWire represents cancel order item wire format
type CancelOrderWire struct {
	Asset   int   `json:"a" msgpack:"a"`
	OrderID int64 `json:"o" msgpack:"o"`
}

// CancelAction represents the cancel action
type CancelAction struct {
	Type    string            `json:"type"    msgpack:"type"`
	Dex     string            `json:"dex,omitempty" msgpack:"dex,omitempty"`
	Cancels []CancelOrderWire `json:"cancels" msgpack:"cancels"`
}

// CancelByCloidWire represents cancel by cloid item wire format
// NB: the CancelByCloidWire MUST have `asset` and not `a` like CancelOrderWire
// See: https://github.com/hyperliquid-dex/hyperliquid-python-sdk/blob/master/hyperliquid/exchange.py
type CancelByCloidWire struct {
	Asset    int    `json:"asset" msgpack:"asset"`
	ClientID string `json:"cloid" msgpack:"cloid"`
}

// CancelByCloidAction represents the cancel by cloid action
type CancelByCloidAction struct {
	Type    string              `json:"type"    msgpack:"type"`
	Dex     string              `json:"dex,omitempty" msgpack:"dex,omitempty"`
	Cancels []CancelByCloidWire `json:"cancels" msgpack:"cancels"`
}

// UsdClassTransferAction represents USD class transfer
type UsdClassTransferAction struct {
	Type   string `json:"type"   msgpack:"type"`
	Amount string `json:"amount" msgpack:"amount"`
	ToPerp bool   `json:"toPerp" msgpack:"toPerp"`
	Nonce  int64  `json:"nonce"  msgpack:"nonce"`
}

// SpotTransferAction represents spot transfer
type SpotTransferAction struct {
	Type        string `json:"type"        msgpack:"type"`
	Destination string `json:"destination" msgpack:"destination"`
	Amount      string `json:"amount"      msgpack:"amount"`
	Token       string `json:"token"       msgpack:"token"`
	Time        int64  `json:"time"        msgpack:"time"`
}

// UsdTransferAction represents USD transfer
type UsdTransferAction struct {
	Type        string `json:"type"        msgpack:"type"`
	Destination string `json:"destination" msgpack:"destination"`
	Amount      string `json:"amount"      msgpack:"amount"`
	Time        int64  `json:"time"        msgpack:"time"`
}

// SubAccountTransferAction represents sub-account transfer
type SubAccountTransferAction struct {
	Type           string `json:"type"           msgpack:"type"`
	SubAccountUser string `json:"subAccountUser" msgpack:"subAccountUser"`
	IsDeposit      bool   `json:"isDeposit"      msgpack:"isDeposit"`
	Usd            int    `json:"usd"            msgpack:"usd"`
}

// VaultUsdTransferAction represents vault USD transfer
type VaultUsdTransferAction struct {
	Type         string `json:"type"         msgpack:"type"`
	VaultAddress string `json:"vaultAddress" msgpack:"vaultAddress"`
	IsDeposit    bool   `json:"isDeposit"    msgpack:"isDeposit"`
	Usd          int    `json:"usd"            msgpack:"usd"`
}

// UpdateLeverageAction represents leverage update
type UpdateLeverageAction struct {
	Type     string `json:"type"     msgpack:"type"`
	Asset    int    `json:"asset"    msgpack:"asset"`
	IsCross  bool   `json:"isCross"  msgpack:"isCross"`
	Leverage int    `json:"leverage" msgpack:"leverage"`
}

// UpdateIsolatedMarginAction represents isolated margin update.
// Ntli is the USD adjustment expressed as integer micro-USD (amount * 1e6),
// matching Python SDK's float_to_usd_int conversion.
type UpdateIsolatedMarginAction struct {
	Type  string `json:"type"  msgpack:"type"`
	Asset int    `json:"asset" msgpack:"asset"`
	IsBuy bool   `json:"isBuy" msgpack:"isBuy"`
	Ntli  int64  `json:"ntli"  msgpack:"ntli"`
}

// OrderWire represents the wire format for orders with deterministic field ordering
type OrderWire struct {
	Asset      int                 `json:"a"           msgpack:"a"`
	IsBuy      bool                `json:"b"           msgpack:"b"`
	LimitPx    string              `json:"p"           msgpack:"p"`
	Size       string              `json:"s"           msgpack:"s"`
	ReduceOnly bool                `json:"r"           msgpack:"r"`
	OrderType  types.OrderTypeWire `json:"t"           msgpack:"t"`
	Cloid      *string             `json:"c,omitempty" msgpack:"c,omitempty"`
}

// OrderAction represents the order action with deterministic field ordering
// CRITICAL: Field order MUST match Python SDK insertion order for msgpack hash consistency
type OrderAction struct {
	Type     string             `json:"type"              msgpack:"type"`
	Dex      string             `json:"dex,omitempty"     msgpack:"dex,omitempty"`
	Orders   []OrderWire        `json:"orders"            msgpack:"orders"`
	Grouping string             `json:"grouping"          msgpack:"grouping"`
	Builder  *types.BuilderInfo `json:"builder,omitempty" msgpack:"builder,omitempty"`
}

// ModifyAction represents a single order modification
type ModifyAction struct {
	Type  string    `json:"type,omitempty"  msgpack:"type,omitempty"`
	Dex   string    `json:"dex,omitempty"   msgpack:"dex,omitempty"`
	Oid   any       `json:"oid"             msgpack:"oid"`
	Order OrderWire `json:"order"           msgpack:"order"`
}

// BatchModifyAction represents multiple order modifications
type BatchModifyAction struct {
	Type     string         `json:"type"     msgpack:"type"`
	Dex      string         `json:"dex,omitempty" msgpack:"dex,omitempty"`
	Modifies []ModifyAction `json:"modifies" msgpack:"modifies"`
}

// PerpDexClassTransferAction represents perp dex class transfer
type PerpDexClassTransferAction struct {
	Type   string  `json:"type"   msgpack:"type"`
	Dex    string  `json:"dex"    msgpack:"dex"`
	Token  string  `json:"token"  msgpack:"token"`
	Amount float64 `json:"amount" msgpack:"amount"`
	ToPerp bool    `json:"toPerp" msgpack:"toPerp"`
}

// SubAccountSpotTransferAction represents sub-account spot transfer
type SubAccountSpotTransferAction struct {
	Type           string  `json:"type"           msgpack:"type"`
	SubAccountUser string  `json:"subAccountUser" msgpack:"subAccountUser"`
	IsDeposit      bool    `json:"isDeposit"      msgpack:"isDeposit"`
	Token          string  `json:"token"          msgpack:"token"`
	Amount         float64 `json:"amount"         msgpack:"amount"`
}

// ScheduleCancelAction represents schedule cancel action
type ScheduleCancelAction struct {
	Type string `json:"type"           msgpack:"type"`
	Time *int64 `json:"time,omitempty" msgpack:"time,omitempty"`
}

// SetReferrerAction represents set referrer action
type SetReferrerAction struct {
	Type string `json:"type" msgpack:"type"`
	Code string `json:"code" msgpack:"code"`
}

// CreateSubAccountAction represents create sub-account action
type CreateSubAccountAction struct {
	Type string `json:"type" msgpack:"type"`
	Name string `json:"name" msgpack:"name"`
}

// UseBigBlocksAction represents use big blocks action
type UseBigBlocksAction struct {
	Type           string `json:"type"           msgpack:"type"`
	UsingBigBlocks bool   `json:"usingBigBlocks" msgpack:"usingBigBlocks"`
}

// TokenDelegateAction represents token delegate action
type TokenDelegateAction struct {
	Type             string `json:"type"             msgpack:"type"`
	HyperliquidChain string `json:"hyperliquidChain" msgpack:"hyperliquidChain"`
	SignatureChainId string `json:"signatureChainId" msgpack:"signatureChainId"`
	Validator        string `json:"validator"        msgpack:"validator"`
	Wei              int    `json:"wei"              msgpack:"wei"`
	IsUndelegate     bool   `json:"isUndelegate"     msgpack:"isUndelegate"`
	Nonce            int64  `json:"nonce"            msgpack:"nonce"`
}

// WithdrawFromBridgeAction represents withdraw from bridge action
type WithdrawFromBridgeAction struct {
	Type             string `json:"type"             msgpack:"type"`
	HyperliquidChain string `json:"hyperliquidChain" msgpack:"hyperliquidChain"`
	SignatureChainId string `json:"signatureChainId" msgpack:"signatureChainId"`
	Destination      string `json:"destination"      msgpack:"destination"`
	Amount           string `json:"amount"           msgpack:"amount"`
	Time             int64  `json:"time"             msgpack:"time"`
}

// ApproveAgentAction represents approve agent action
type ApproveAgentAction struct {
	Type             string  `json:"type"                msgpack:"type"`
	HyperliquidChain string  `json:"hyperliquidChain"    msgpack:"hyperliquidChain"`
	SignatureChainId string  `json:"signatureChainId"    msgpack:"signatureChainId"`
	AgentAddress     string  `json:"agentAddress"        msgpack:"agentAddress"`
	AgentName        *string `json:"agentName,omitempty" msgpack:"agentName,omitempty"`
	Nonce            int64   `json:"nonce"               msgpack:"nonce"`
}

// ApproveBuilderFeeAction represents approve builder fee action
type ApproveBuilderFeeAction struct {
	Type             string `json:"type"             msgpack:"type"`
	HyperliquidChain string `json:"hyperliquidChain" msgpack:"hyperliquidChain"`
	SignatureChainId string `json:"signatureChainId" msgpack:"signatureChainId"`
	Builder          string `json:"builder"          msgpack:"builder"`
	MaxFeeRate       string `json:"maxFeeRate"       msgpack:"maxFeeRate"`
	Nonce            int64  `json:"nonce"            msgpack:"nonce"`
}

// ConvertToMultiSigUserAction represents convert to multi-sig user action
type ConvertToMultiSigUserAction struct {
	Type             string `json:"type"             msgpack:"type"`
	HyperliquidChain string `json:"hyperliquidChain" msgpack:"hyperliquidChain"`
	SignatureChainId string `json:"signatureChainId" msgpack:"signatureChainId"`
	Signers          string `json:"signers"          msgpack:"signers"`
	Nonce            int64  `json:"nonce"            msgpack:"nonce"`
}

// MultiSigAction represents multi-signature action
type MultiSigAction struct {
	Type       string         `json:"type"       msgpack:"type"`
	Action     map[string]any `json:"action"     msgpack:"action"`
	Signers    []string       `json:"signers"    msgpack:"signers"`
	Signatures []string       `json:"signatures" msgpack:"signatures"`
}

// TWAPOrderAction represents TWAP order action
type TWAPOrderAction struct {
	Type string        `json:"type" msgpack:"type"`
	TWAP TWAPOrderWire `json:"twap" msgpack:"twap"`
}

// TWAPOrderWire represents TWAP order wire format
type TWAPOrderWire struct {
	Asset      int    `json:"a" msgpack:"a"`
	IsBuy      bool   `json:"b" msgpack:"b"`
	Size       string `json:"s" msgpack:"s"`
	ReduceOnly bool   `json:"r" msgpack:"r"`
	Minutes    int    `json:"m" msgpack:"m"`
	Randomize  bool   `json:"t" msgpack:"t"`
}

// TWAPCancelAction represents TWAP cancel action
type TWAPCancelAction struct {
	Type   string `json:"type" msgpack:"type"`
	Asset  int    `json:"a"    msgpack:"a"`
	TWAPID int    `json:"t"    msgpack:"t"`
}

// ReserveRequestWeightAction represents reserve request weight action
type ReserveRequestWeightAction struct {
	Type   string `json:"type"   msgpack:"type"`
	Weight int    `json:"weight" msgpack:"weight"`
}

// HIP-4 userOutcome actions. Every variant shares the same envelope —
// type:"userOutcome" plus exactly one inner body keyed by the verb. The
// venue parses by which inner key is present.

// SplitOutcomeWire is the inner body for splitting the market's quote
// token (USDC since the USDH sunset) into Yes+No shares of an outcome.
// Amount is the quote-token notional, as a decimal string. The wire
// carries no token field — the collateral is implicit in the market.
type SplitOutcomeWire struct {
	Outcome uint64 `json:"outcome" msgpack:"outcome"`
	Amount  string `json:"amount"  msgpack:"amount"`
}

// MergeOutcomeWire is the inner body for merging Yes+No shares back into
// the market's quote token (USDC) for a single outcome. Amount=nil burns
// the maximum holdable.
type MergeOutcomeWire struct {
	Outcome uint64  `json:"outcome" msgpack:"outcome"`
	Amount  *string `json:"amount"  msgpack:"amount"`
}

// MergeQuestionWire is the inner body for collapsing X Yes shares from
// every named outcome of a question into X quote tokens (USDC). Amount=nil
// burns the maximum the caller holds across all buckets (min Yes balance).
type MergeQuestionWire struct {
	Question uint64  `json:"question" msgpack:"question"`
	Amount   *string `json:"amount"   msgpack:"amount"`
}

// NegateOutcomeWire is the inner body for converting X No shares of one
// outcome into X Yes shares of every OTHER outcome in the same question
// (because No(B) is equivalent to Yes of any non-B bucket).
type NegateOutcomeWire struct {
	Question uint64 `json:"question" msgpack:"question"`
	Outcome  uint64 `json:"outcome"  msgpack:"outcome"`
	Amount   string `json:"amount"   msgpack:"amount"`
}

// SplitOutcomeAction is { type:"userOutcome", splitOutcome:{ outcome, amount } }.
type SplitOutcomeAction struct {
	Type         string           `json:"type"         msgpack:"type"`
	SplitOutcome SplitOutcomeWire `json:"splitOutcome" msgpack:"splitOutcome"`
}

// MergeOutcomeAction is { type:"userOutcome", mergeOutcome:{ outcome, amount } }.
type MergeOutcomeAction struct {
	Type         string           `json:"type"         msgpack:"type"`
	MergeOutcome MergeOutcomeWire `json:"mergeOutcome" msgpack:"mergeOutcome"`
}

// MergeQuestionAction is { type:"userOutcome", mergeQuestion:{ question, amount } }.
type MergeQuestionAction struct {
	Type          string            `json:"type"          msgpack:"type"`
	MergeQuestion MergeQuestionWire `json:"mergeQuestion" msgpack:"mergeQuestion"`
}

// NegateOutcomeAction is { type:"userOutcome", negateOutcome:{ question, outcome, amount } }.
type NegateOutcomeAction struct {
	Type          string            `json:"type"          msgpack:"type"`
	NegateOutcome NegateOutcomeWire `json:"negateOutcome" msgpack:"negateOutcome"`
}
