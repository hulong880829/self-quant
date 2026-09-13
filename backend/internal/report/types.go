package report

import (
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("product not found")
	ErrUnauthenticated = errors.New("authentication required")
	ErrInvalidInput    = errors.New("invalid report input")
	ErrSourceDisabled  = errors.New("account source is not configured")
	ErrDuplicate       = errors.New("duplicate cash flow")
)

type Product struct {
	ID               int64
	OwnerUsername    string
	Name             string
	DisplayName      string
	Category         string
	Strategy         string
	BaseCurrency     string
	Timezone         string
	Active           bool
	InceptionDate    string
	AccountCount     int
	LatestEquityUSD  string
	LatestPnLUSD     string
	LatestReturnRate string
	UpdatedAt        time.Time
}

type DailySnapshot struct {
	ID                int64
	ProductID         int64
	ReportDate        string
	OpeningEquityUSD  string
	ClosingEquityUSD  string
	NetCashFlowUSD    string
	PnLUSD            string
	ReturnRate        string
	AbsoluteReturn    string
	AnnualizedReturn  string
	Annualized7D      string
	Annualized30D     string
	MaxDrawdown       string
	Sharpe            string
	Volume24hUSD      string
	SampleCount       int
	Status            string
	FinalizedAt       time.Time
	SubscriptionUSD   string
	RedemptionUSD     string
	CashFlowCount     int
	PeriodRuleVersion int
	PeriodStart       time.Time
	PeriodEnd         time.Time
}

type CashFlow struct {
	ID                int64
	ProductID         int64
	FlowDate          string
	OccurredAt        time.Time
	AmountUSD         string
	FlowType          string
	Note              string
	Confirmed         bool
	ConfirmedBy       string
	ConfirmedAt       time.Time
	CreatedBy         string
	CreatedAt         time.Time
	RecomputeStatus   string
	IdempotencyKey    string
	PeriodRuleVersion int
}

type AccountEquity struct {
	TradingAccountID  int64
	EquityUSD         string
	AvailableFundsUSD string
	SourceUpdatedAt   time.Time
}

type DateRange struct {
	From  string
	To    string
	Limit int
}

type ProductDetail struct {
	Product         Product
	LatestSnapshot  *DailySnapshot
	Daily           []DailySnapshot
	RecentCashFlows []CashFlow
}
