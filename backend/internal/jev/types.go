package jev

import (
	"errors"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	PromptVersion     = "DecisionPromptV4"
	PolicyVersion     = "JEVPolicyV6"
	StrategyVersion   = "JEVStrategyV6"
	BasisModelVersion = "conditional_mid_replay_v2"

	KindSingle = "single"
	KindCross  = "cross"

	ProductSpot       = "spot"
	ProductPerpetual  = "perpetual"
	PositionOpen      = "open"
	PositionClosed    = "closed"
	DecisionHold      = "hold"
	DecisionEnter     = "enter"
	DecisionExit      = "exit"
	PolicySuggested   = "suggested"
	PolicyAccepted    = "accepted"
	PolicyRejected    = "rejected"
	ExecNotApplicable = "not_applicable"
	ExecPending       = "pending"
	ExecFilled        = "filled"
	ExecExpired       = "expired"
	ExecPortfolioRej  = "portfolio_rejected"
	ExecFillFailed    = "fill_failed"

	StatusRunning  = "running"
	StatusDisabled = "disabled"
	StatusDegraded = "degraded"
	StatusStale    = "stale"

	ExitPendingMarketData    = "exit_pending_market_data"
	MandatoryExitPending     = "mandatory_exit_pending"
	AccountingIncomplete     = "accounting_incomplete"
	CanonicalUnresolved      = "canonical_unresolved"
	BasisHistoryInsufficient = "basis_history_insufficient"
	ModelFailedLocalRules    = "模型失败，采用本地规则"

	VenuePerpetual = "PERPETUAL"
	VenueTradifi   = "TRADIFI_PERPETUAL"
	VenueHIP3      = "HIP3"

	DefaultListLimit = 50
	MaxListLimit     = 100
	MaxEquityPoints  = 2000
	TopEntryCount    = 20
	HistoryLimit     = 100

	ModelStatusNotCalled    = "not_called"
	ModelStatusSuccess      = "success"
	ModelStatusError        = "error"
	ModelStatusUnauthorized = "unauthorized"

	LockKey int64 = 0x4a455650 // JEVP
)

var (
	ErrNoSnapshot      = errors.New("jev snapshot unavailable")
	ErrUnavailable     = errors.New("jev service unavailable")
	ErrInvalidArgument = errors.New("invalid argument")
	ErrUnauthorized    = errors.New("typesafe unauthorized")
)

type MarketKey struct {
	Venue    string
	Symbol   string
	Product  string
	Exchange string
}

func (k MarketKey) QuoteKey() string {
	return k.Venue + "\x00" + k.Symbol + "\x00" + k.Product
}

func CanonicalSymbol(base, quote string) string {
	return strings.ToUpper(strings.TrimSpace(base)) + strings.ToUpper(strings.TrimSpace(quote))
}

func NewCanonicalMarketKey(venue, canonical, product string) MarketKey {
	return MarketKey{
		Venue:    strings.ToLower(strings.TrimSpace(venue)),
		Symbol:   strings.ToUpper(strings.TrimSpace(canonical)),
		Product:  product,
		Exchange: strings.ToLower(strings.TrimSpace(venue)),
	}
}

func (k MarketKey) CanonicalOK() bool {
	return strings.TrimSpace(k.Venue) != "" && strings.TrimSpace(k.Symbol) != "" && strings.TrimSpace(k.Product) != ""
}

type Quote struct {
	Key       MarketKey
	Bid       decimal.Decimal
	Ask       decimal.Decimal
	BidQty    decimal.Decimal
	AskQty    decimal.Decimal
	TS        time.Time
	Available bool
}

func (q Quote) Mid() decimal.Decimal {
	if !q.Bid.IsPositive() || !q.Ask.IsPositive() {
		return decimal.Zero
	}
	return q.Bid.Add(q.Ask).Div(decimal.NewFromInt(2))
}

func (q Quote) Tradable() bool {
	return q.Available && q.Bid.IsPositive() && q.Ask.IsPositive() &&
		q.BidQty.IsPositive() && q.AskQty.IsPositive()
}

func (q Quote) Fresh(now time.Time, maxAge time.Duration) bool {
	if q.TS.IsZero() || maxAge <= 0 {
		return false
	}
	age := now.Sub(q.TS)
	return age >= 0 && age <= maxAge
}

func (q Quote) DepthUSD() decimal.Decimal {
	bidNotional := q.Bid.Mul(q.BidQty)
	askNotional := q.Ask.Mul(q.AskQty)
	if bidNotional.LessThan(askNotional) {
		return bidNotional
	}
	return askNotional
}

type SpotInstrument struct {
	Exchange       string
	ExchangeSymbol string
	BaseAsset      string
	QuoteAsset     string
	GlobalSymbol   string
}

type Rate struct {
	Exchange               string
	ExchangeSymbol         string
	GlobalSymbol           string
	BaseAsset              string
	QuoteAsset             string
	FundingRate            decimal.Decimal
	NextFundingRate        *decimal.Decimal
	Cumulative24h          decimal.Decimal
	AnnualizedRate         decimal.Decimal
	Turnover24hUSD         decimal.Decimal
	PositionNotionalUSD    decimal.Decimal
	FundingIntervalSeconds int32
	SourceUpdatedAt        time.Time
	NextFundingAt          time.Time
	History24hComplete     bool
	VenueContractType      string
	Stale                  bool
}

type HistoryPoint struct {
	Rate      decimal.Decimal
	SettledAt time.Time
}

type Candidate struct {
	Key                       string
	Kind                      string
	BaseAsset                 string
	QuoteAsset                string
	GlobalSymbol              string
	LongExchange              string
	LongSymbol                string
	LongProduct               string
	LongCanonicalSymbol       string
	LongQuoteAsset            string
	ShortExchange             string
	ShortSymbol               string
	ShortProduct              string
	ShortCanonicalSymbol      string
	ShortQuoteAsset           string
	Cumulative24hLong         decimal.Decimal
	Cumulative24hShort        decimal.Decimal
	Edge24h                   decimal.Decimal
	FundingReturn24h          decimal.Decimal
	ExpectedNetReturn24h      decimal.Decimal
	ConservativeNetReturn24h  decimal.Decimal
	HoldingHorizonNetReturn   decimal.Decimal
	MatchedBasisReturns       []decimal.Decimal
	MatchBandBPS              *decimal.Decimal
	ExpectedPaybackMinutes    *decimal.Decimal
	CurrentFundingLong        decimal.Decimal
	CurrentFundingShort       decimal.Decimal
	NextFundingLong           *decimal.Decimal
	NextFundingShort          *decimal.Decimal
	TurnoverLong              decimal.Decimal
	TurnoverShort             decimal.Decimal
	LongQuote                 Quote
	ShortQuote                Quote
	CurrentMidBasisBPS        *decimal.Decimal
	BasisChange1hBPS          *decimal.Decimal
	BasisChange8hBPS          *decimal.Decimal
	ExpectedBasisReturn24h    *decimal.Decimal
	BasisReturnP10            *decimal.Decimal
	BasisProfitProbability    *decimal.Decimal
	BasisForecastConfidence   *decimal.Decimal
	BasisSampleCount          *int
	BasisForecastCalculatedAt *time.Time
	BasisModelVersion         *string
}

func (c Candidate) LongKey() MarketKey {
	return NewCanonicalMarketKey(c.LongExchange, c.LongCanonicalSymbol, c.LongProduct)
}

func (c Candidate) ShortKey() MarketKey {
	return NewCanonicalMarketKey(c.ShortExchange, c.ShortCanonicalSymbol, c.ShortProduct)
}

func (c Candidate) CanonicalOK() bool {
	return strings.TrimSpace(c.LongCanonicalSymbol) != "" && strings.TrimSpace(c.ShortCanonicalSymbol) != "" &&
		strings.TrimSpace(c.BaseAsset) != ""
}

type Run struct {
	ID                        int64
	StrategyVersion           string
	Status                    string
	InitialEquity             decimal.Decimal
	Equity                    decimal.Decimal
	PeakEquity                decimal.Decimal
	NetPnL                    decimal.Decimal
	FundingPnL                decimal.Decimal
	RealizedSpreadPnL         decimal.Decimal
	UnrealizedSpreadPnL       decimal.Decimal
	CumulativeCost            decimal.Decimal
	MaxDrawdown               decimal.Decimal
	SnapshotVersion           int64
	LastSuccessfulAt          *time.Time
	LastDecisionAt            *time.Time
	LastMarketDataAt          *time.Time
	ArchivedRealizedSpreadPnL decimal.Decimal
	ArchivedFundingPnL        decimal.Decimal
	ArchivedCostPaid          decimal.Decimal
	ArchivedPositionCount     int64
}

type Position struct {
	ID                         int64
	RunID                      int64
	StrategyVersion            string
	Kind                       string
	Status                     string
	CandidateKey               string
	BaseAsset                  string
	QuoteAsset                 string
	GlobalSymbol               string
	LongExchange               string
	LongSymbol                 string
	LongProduct                string
	LongCanonicalSymbol        string
	LongQuoteAsset             string
	ShortExchange              string
	ShortSymbol                string
	ShortProduct               string
	ShortCanonicalSymbol       string
	ShortQuoteAsset            string
	EntryMidBasisBPS           *decimal.Decimal
	SharedBaseQty              decimal.Decimal
	LongQty                    decimal.Decimal
	ShortQty                   decimal.Decimal
	LongEntryPrice             decimal.Decimal
	ShortEntryPrice            decimal.Decimal
	LongMarkPrice              decimal.Decimal
	ShortMarkPrice             decimal.Decimal
	RealizedSpreadPnL          decimal.Decimal
	UnrealizedSpreadPnL        decimal.Decimal
	FundingPnL                 decimal.Decimal
	CostPaid                   decimal.Decimal
	FundingCheckedThroughLong  *time.Time
	FundingCheckedThroughShort *time.Time
	ExitPendingReason          string
	DegradedReason             string
	ExitConfirmCount           int
	NetIfExit                  *decimal.Decimal
	NetIfExitAt                *time.Time
	BBOMissingSince            *time.Time
	OpenedAt                   time.Time
	ClosedAt                   *time.Time
	LastMarkAt                 *time.Time
}

func (p Position) NetPnL() decimal.Decimal {
	return p.RealizedSpreadPnL.Add(p.UnrealizedSpreadPnL).Add(p.FundingPnL).Sub(p.CostPaid)
}

func (p Position) LongKey() MarketKey {
	return NewCanonicalMarketKey(p.LongExchange, p.LongCanonicalSymbol, p.LongProduct)
}

func (p Position) ShortKey() MarketKey {
	return NewCanonicalMarketKey(p.ShortExchange, p.ShortCanonicalSymbol, p.ShortProduct)
}

func (p Position) CanonicalOK() bool {
	return strings.TrimSpace(p.LongCanonicalSymbol) != "" && strings.TrimSpace(p.ShortCanonicalSymbol) != "" &&
		strings.TrimSpace(p.BaseAsset) != ""
}

func NormalizeMarketKey(exchange, symbol, product string) MarketKey {
	return MarketKey{
		Venue:    strings.ToLower(strings.TrimSpace(exchange)),
		Symbol:   strings.ToUpper(strings.TrimSpace(symbol)),
		Product:  product,
		Exchange: strings.ToLower(strings.TrimSpace(exchange)),
	}
}

type Decision struct {
	ID                        int64
	RunID                     int64
	PositionID                *int64
	CandidateKey              string
	Kind                      string
	Decision                  string
	PolicyStatus              string
	ExecutionStatus           string
	EnterProbability          decimal.Decimal
	ExitProbability           decimal.Decimal
	Confidence                decimal.Decimal
	ExpectedNetReturn24h      *decimal.Decimal
	FundingReturn24h          *decimal.Decimal
	HoldingHorizonNetReturn   *decimal.Decimal
	ExpectedPaybackMinutes    *decimal.Decimal
	CurrentMidBasisBPS        *decimal.Decimal
	EntryMidBasisBPS          *decimal.Decimal
	BasisChangeSinceEntryBPS  *decimal.Decimal
	ExpectedBasisReturn24h    *decimal.Decimal
	BasisReturnP10            *decimal.Decimal
	BasisProfitProbability    *decimal.Decimal
	BasisForecastConfidence   *decimal.Decimal
	BasisSampleCount          *int
	BasisModelVersion         *string
	BasisForecastCalculatedAt *time.Time
	Rationale                 string
	PromptVersion             string
	PolicyVersion             string
	StrategyVersion           string
	ModelRequested            string
	ModelActual               string
	RawResponse               []byte
	ErrorMessage              string
	ParseErrorType            string
	DecisionAt                time.Time
	ExecutionStartedAt        *time.Time
	FillDeadline              *time.Time
	BaseAsset                 string
	QuoteAsset                string
	LongCanonicalSymbol       string
	LongQuoteAsset            string
	ShortCanonicalSymbol      string
	ShortQuoteAsset           string
}

type Trade struct {
	ID              int64
	RunID           int64
	PositionID      int64
	DecisionID      *int64
	Side            string
	SharedBaseQty   decimal.Decimal
	LongFillPrice   decimal.Decimal
	ShortFillPrice  decimal.Decimal
	Notional        decimal.Decimal
	Cost            decimal.Decimal
	FilledAt        time.Time
	StrategyVersion string
	BaseAsset       string
	Kind            string
	LongExchange    string
	LongSymbol      string
	LongProduct     string
	ShortExchange   string
	ShortSymbol     string
	ShortProduct    string
}

type EquityPoint struct {
	TS     time.Time
	Equity decimal.Decimal
	NetPnL decimal.Decimal
}

type Snapshot struct {
	Version                      int64
	ETag                         string
	Status                       string
	DegradedReasons              []string
	LastSuccessfulAt             *time.Time
	LastDecisionAt               *time.Time
	LastMarketDataAt             *time.Time
	ServerTime                   time.Time
	Model                        string
	ModelRequested               string
	PromptVersion                string
	PolicyVersion                string
	StrategyVersion              string
	Simulated                    bool
	Equity                       decimal.Decimal
	NetPnL                       decimal.Decimal
	FundingPnL                   decimal.Decimal
	RealizedSpreadPnL            decimal.Decimal
	UnrealizedSpreadPnL          decimal.Decimal
	CumulativeCost               decimal.Decimal
	MaxDrawdown                  decimal.Decimal
	InitialEquity                decimal.Decimal
	LifecycleEquity              decimal.Decimal
	LifecycleNetPnL              decimal.Decimal
	LifecycleFundingPnL          decimal.Decimal
	LifecycleRealizedSpreadPnL   decimal.Decimal
	LifecycleUnrealizedSpreadPnL decimal.Decimal
	LifecycleCumulativeCost      decimal.Decimal
	PaperRoundTripCostBPS        decimal.Decimal
	USDTUSDCOneToOne             bool
	SingleEntryEmptyReason       string
	CrossEntryEmptyReason        string
	StrategySingleStatus         string
	StrategyCrossStatus          string
	OpenPositionCount            int
	EntryCandidateCount          int
	TypeSafeConfigured           bool
	SingleFilter                 SingleFilterStats
	CrossFilter                  CrossFilterStats
	DecisionJobs                 int
	TypeSafeRequests             int
	TypeSafeSuccess              int
	TypeSafeError                int
	ModelStatus                  string
	LastModelCallAt              *time.Time
	AvailableStrategyVersions    []string
}

type SingleFilterStats struct {
	Input               int
	ContractPassed      int
	HistoryComplete     int
	Positive24hPassed   int
	FreshPassed         int
	TurnoverPassed      int
	SpotMatched         int
	BBOAvailable        int
	BBOFreshPassed      int
	DepthPassed         int
	PaybackPassed       int
	BasisForecastPassed int
	Final               int
}

type CrossFilterStats struct {
	Input               int
	ContractPassed      int
	HistoryComplete     int
	PositiveEdgePassed  int
	FreshPassed         int
	TurnoverPassed      int
	BBOAvailable        int
	BBOFreshPassed      int
	DepthPassed         int
	PaybackPassed       int
	BasisForecastPassed int
	Final               int
}

type ModelCycleStats struct {
	DecisionJobs    int
	Requests        int
	Success         int
	Error           int
	LastModelCallAt *time.Time
	Status          string
	ModelActual     string
}

type ModelAnswer struct {
	Decision                  string
	EnterProbability          decimal.Decimal
	ExitProbability           decimal.Decimal
	Confidence                decimal.Decimal
	ExpectedNetReturn24h      *decimal.Decimal
	FundingReturn24h          *decimal.Decimal
	HoldingHorizonNetReturn   *decimal.Decimal
	ExpectedPaybackMinutes    *decimal.Decimal
	CurrentMidBasisBPS        *decimal.Decimal
	EntryMidBasisBPS          *decimal.Decimal
	BasisChangeSinceEntryBPS  *decimal.Decimal
	ExpectedBasisReturn24h    *decimal.Decimal
	BasisReturnP10            *decimal.Decimal
	BasisProfitProbability    *decimal.Decimal
	BasisForecastConfidence   *decimal.Decimal
	BasisSampleCount          *int
	BasisModelVersion         *string
	BasisForecastCalculatedAt *time.Time
	Rationale                 string
	ModelActual               string
	Raw                       []byte
	ParseErrorType            string
}

type CycleResult struct {
	DegradedReasons []string
	DecisionAt      *time.Time
	MarketDataAt    *time.Time
	TypeSafeAborted bool
}
