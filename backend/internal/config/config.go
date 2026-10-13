package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type Funding struct {
	DatabaseURL               string
	GRPCAddress               string
	SyncInterval              time.Duration
	HTTPTimeout               time.Duration
	InstrumentSyncInterval    time.Duration
	ClickHouseAddr            string
	ClickHouseDatabase        string
	ClickHouseTable           string
	ClickHouseUser            string
	ClickHousePassword        string
	ClickHouseTLS             bool
	ClickHouseTLSSkip         bool
	RankingQueryTimeout       time.Duration
	RankingPoolSize           int
	RankingInterval           time.Duration
	RankingTimeout            time.Duration
	RankingHistoryIncrement   time.Duration
	RankingRateMaxAge         time.Duration
	RankingEnabled            bool
	RankingMinuteEnabled      bool
	RankingHistoryMaxRows     int
	LogLevel                  string
	Development               bool
	EnabledExchanges          map[string]bool
	PublishedExchanges        map[string]bool
	SpreadEnabledExchanges    map[string]bool
	RankingEnabledExchanges   map[string]bool
	AsterFundingAPIURL        string
	LighterFundingAPIURL      string
	TradefiPerpsEnabled       bool
	IBKRGatewayAddr           string
	IBKRDiscoveryClientID     int
	ArbitrageEnabledExchanges map[string]bool
}

type Account struct {
	DatabaseURL            string
	GRPCAddress            string
	TokenSecret            string
	GatewayInternalToken   string
	AdminInternalToken     string
	ReportInternalToken    string
	TraderInternalToken    string
	TokenTTL               time.Duration
	CredentialsKey         string
	PolymarketCLOBURL      string
	PolymarketDataURL      string
	BinancePortfolioURL    string
	OKXAccountURL          string
	BitgetAccountURL       string
	BybitAccountURL        string
	GateAccountURL         string
	HyperliquidInfoAPIURL  string
	AsterFuturesAPIURL     string
	LighterAPIURL          string
	AccountSnapshotTTL     time.Duration
	StatusCacheTTL         time.Duration
	EquitySnapshotInterval time.Duration
	HTTPTimeout            time.Duration
	LogLevel               string
	Development            bool
}

type Polymarket struct {
	DatabaseURL           string
	GRPCAddress           string
	AccountGRPCAddress    string
	GammaURL              string
	CLOBURL               string
	CLOBWSURL             string
	CLOBUserWSURL         string
	DataURL               string
	RTDSURL               string
	ChainlinkHistoryURL   string
	ChainlinkHistoryToken string
	HTTPTimeout           time.Duration
	MarketSyncInterval    time.Duration
	PositionActiveTTL     time.Duration
	PositionIdleTTL       time.Duration
	PriceRetention        time.Duration
	PriceSampleInterval   time.Duration
	LogLevel              string
	Development           bool
}

type Gateway struct {
	HTTPAddress           string
	FundingGRPCAddress    string
	AccountGRPCAddress    string
	PolymarketGRPCAddress string
	ReportGRPCAddress     string
	TraderGRPCAddress     string
	AIGRPCAddress         string
	SpreadGRPCAddress     string
	JevGRPCAddress        string
	AdminGRPCAddress      string
	SessionCookieName     string
	SessionCookieSecure   bool
	SessionCookieMaxAge   time.Duration
	GatewayInternalToken  string
	BrokerJWTPublicKey    string
	BrokerAuthCacheTTL    time.Duration
	CORSOrigin            string
	LogLevel              string
}

type Spread struct {
	GRPCAddress        string
	ClickHouseAddr     string
	ClickHouseDatabase string
	ClickHouseTable    string
	ClickHouseUser     string
	ClickHousePassword string
	ClickHouseTLS      bool
	ClickHouseTLSSkip  bool
	QueryTimeout       time.Duration
	PoolSize           int
	MaxConcurrentQuery int
	CacheTTL           time.Duration
	LogLevel           string
	Development        bool
}

type Jev struct {
	DatabaseURL                 string
	GRPCAddress                 string
	FundingGRPCAddress          string
	ClickHouseAddr              string
	ClickHouseDatabase          string
	ClickHouseTable             string
	ClickHouseUser              string
	ClickHousePassword          string
	ClickHouseTLS               bool
	ClickHouseTLSSkip           bool
	QueryTimeout                time.Duration
	PoolSize                    int
	Enabled                     bool
	TypeSafeAPIKey              string
	TypeSafeBaseURL             string
	TypeSafeModel               string
	TypeSafeTimeout             time.Duration
	CycleInterval               time.Duration
	FillWait                    time.Duration
	BBOMaxAge                   time.Duration
	FundingPriceMaxAge          time.Duration
	RateMaxAge                  time.Duration
	MinTurnoverUSD              decimal.Decimal
	MinDepthUSD                 decimal.Decimal
	PositionNotionalUSDT        decimal.Decimal
	PaperRoundTripCostBPS       decimal.Decimal
	PaperInitialEquity          decimal.Decimal
	MinExitProbability          decimal.Decimal
	MaxHoldDuration             time.Duration
	MaxConcurrentDecisions      int
	MaxEntryDecisionsPerCycle   int
	MaxOpenPositions            int
	USDTUSDCOneToOne            bool
	BasisLookback               time.Duration
	BasisForecastHorizon        time.Duration
	BasisRefreshInterval        time.Duration
	BasisCacheMaxAge            time.Duration
	MinExpectedNetReturn24h     decimal.Decimal
	MinConservativeNetReturn24h decimal.Decimal
	MinNetProfitProbability     decimal.Decimal
	MinBasisForecastConfidence  decimal.Decimal
	MinEnterProbability         decimal.Decimal
	MinEnterConfidence          decimal.Decimal
	BasisMatchBandBPS           decimal.Decimal
	BasisConfidenceSampleTarget int
	StopLossNetUSDT             decimal.Decimal
	TakeProfitPadUSDT           decimal.Decimal
	ExitConfirmCycles           int
	MissingBBOForceAfter        time.Duration
	ProbabilitySumTolerance     decimal.Decimal
	LogLevel                    string
	Development                 bool
}

type Admin struct {
	DatabaseURL          string
	GRPCAddress          string
	AccountGRPCAddress   string
	TraderGRPCAddress    string
	GatewayInternalToken string
	AdminInternalToken   string
	JWTPrivateKeyFile    string
	AccessTokenTTL       time.Duration
	LogLevel             string
	Development          bool
}

type AI struct {
	DatabaseURL                   string
	GRPCAddress                   string
	AccountGRPCAddress            string
	FundingGRPCAddress            string
	OpenRouterBaseURL             string
	OpenRouterModel               string
	OpenRouterReferer             string
	OpenRouterTitle               string
	HTTPTimeout                   time.Duration
	MaxInputRounds                int
	MaxInputChars                 int
	MaxOutputTokens               int
	ContextMaxSymbolRows          int
	ContextMaxRows                int
	ContextMinTurnover            float64
	ContextMaxChars               int
	ConversationRetention         time.Duration
	ConversationLease             time.Duration
	CleanupInterval               time.Duration
	CleanupBatch                  int
	CleanupMaxBatches             int
	ChatInputTokenBudget          int
	RecentMessagePairs            int
	SummaryTriggerTokens          int
	SummaryMaxChars               int
	SummaryMaxOutputTokens        int
	MaxConversationsPerUser       int
	MaxRawMessagesPerConversation int
	LogLevel                      string
	Development                   bool
}

type Trader struct {
	DatabaseURL                  string
	GRPCAddress                  string
	AccountGRPCAddress           string
	BinanceURL                   string
	OKXURL                       string
	BybitURL                     string
	BitgetURL                    string
	GateURL                      string
	HyperliquidURL               string
	AsterURL                     string
	LighterURL                   string
	HTTPTimeout                  time.Duration
	InternalToken                string
	AdminInternalToken           string
	ReconcileInterval            time.Duration
	ReconcileBatchSize           int
	ReconcileWorkers             int
	TwapScheduleInterval         time.Duration
	TwapScheduleLease            time.Duration
	TwapScheduleBatch            int
	TwapScheduleWorkers          int
	TwapRetention                time.Duration
	TwapCleanupInterval          time.Duration
	TwapCleanupBatch             int
	TwapCleanupMaxBatches        int
	ArbitrageControlInterval     time.Duration
	ArbitrageCoalesceWindow      time.Duration
	ArbitrageScheduleLease       time.Duration
	ArbitrageScheduleBatch       int
	ArbitrageScheduleWorkers     int
	ArbitrageBBOStale            time.Duration
	ArbitrageSignalBBOStale      time.Duration
	ArbitrageBBOReconnectInitial time.Duration
	ArbitrageBBOReconnectMax     time.Duration
	ArbitrageBBOHeartbeat        time.Duration
	ArbitrageBBOReadWait         time.Duration
	ArbitrageBBOWriteWait        time.Duration
	ArbitrageBBOAckWait          time.Duration
	ArbitrageRepriceTicks        int
	ArbitrageObserverInterval    time.Duration
	ArbitrageIOCProtectionBps    int
	ArbitrageIOCRetries          int
	ArbitrageHedgeEmergencyAfter time.Duration
	ArbitrageOrderStreamEnabled  bool
	BinanceOrderWSURL            string
	OKXOrderWSURL                string
	BybitOrderWSURL              string
	BitgetOrderWSURL             string
	GateOrderWSURL               string
	HyperliquidOrderWSURL        string
	AsterOrderWSURL              string
	LighterOrderWSURL            string
	OrderStreamReconnectInitial  time.Duration
	OrderStreamReconnectMax      time.Duration
	OrderStreamHeartbeat         time.Duration
	OrderStreamStale             time.Duration
	OrderStreamListenKeyRefresh  time.Duration
	OrderStreamRESTAudit         time.Duration
	OrderStreamSessionIdle       time.Duration
	ArbitrageMaxActiveAccount    int
	ArbitrageMaxActiveVenue      int
	ArbitrageRetention           time.Duration
	ArbitrageCleanupInterval     time.Duration
	ArbitrageCleanupBatch        int
	ArbitrageCleanupMaxBatches   int
	ArbitrageDryRun              bool
	ArbitrageEnabledExchanges    map[string]bool
	LogLevel                     string
	Development                  bool
}

type Report struct {
	DatabaseURL        string
	GRPCAddress        string
	AccountGRPCAddress string
	InternalToken      string
	Timezone           string
	LogLevel           string
	Development        bool
}

func FundingFromEnv() (Funding, error) {
	c := Funding{
		DatabaseURL:            env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:            env("FUNDING_GRPC_ADDRESS", ":9090"),
		SyncInterval:           duration("SYNC_INTERVAL", 10*time.Minute),
		HTTPTimeout:            duration("EXCHANGE_HTTP_TIMEOUT", 10*time.Second),
		InstrumentSyncInterval: duration("INSTRUMENT_SYNC_INTERVAL", 8*time.Hour),
		ClickHouseAddr:         env("CLICKHOUSE_ADDR", "127.0.0.1:9000"),
		ClickHouseDatabase:     env("CLICKHOUSE_DATABASE", "market_data"),
		ClickHouseTable:        env("CLICKHOUSE_TABLE", "crypto_bbo"),
		ClickHouseUser:         env("CLICKHOUSE_USER", "bbo_writer"),
		ClickHousePassword:     env("CLICKHOUSE_PASSWORD", env("CLICKHOUSE_BBO_PASSWORD", "")),
		ClickHouseTLS:          boolean("CLICKHOUSE_TLS", false),
		ClickHouseTLSSkip:      boolean("CLICKHOUSE_TLS_SKIP_VERIFY", false),
		RankingQueryTimeout:    duration("FUNDING_RANKING_QUERY_TIMEOUT", 30*time.Second),
		RankingPoolSize:        positiveInteger("FUNDING_RANKING_POOL_SIZE", 4),
		RankingInterval:        duration("FUNDING_RANKING_INTERVAL", 10*time.Minute),
		RankingTimeout:         duration("FUNDING_RANKING_TIMEOUT", 5*time.Minute),
		RankingHistoryIncrement: duration(
			"FUNDING_RANKING_HISTORY_INCREMENT_INTERVAL", 5*time.Minute,
		),
		RankingRateMaxAge:     duration("FUNDING_RANKING_RATE_MAX_AGE", 15*time.Minute),
		RankingEnabled:        boolean("FUNDING_RANKING_ENABLED", true),
		RankingMinuteEnabled:  boolean("FUNDING_RANKING_MINUTE_TABLE_ENABLED", true),
		RankingHistoryMaxRows: positiveInteger("FUNDING_RANKING_HISTORY_MAX_ROWS", 12_000_000),
		LogLevel:              env("LOG_LEVEL", "info"),
		Development:           boolean("DEVELOPMENT", false),
		EnabledExchanges: enabledExchanges(env(
			"ENABLED_EXCHANGES",
			defaultFundingCollectionExchanges,
		)),
		PublishedExchanges: enabledExchanges(env(
			"FUNDING_PUBLISHED_EXCHANGES",
			defaultFundingPublicationExchanges,
		)),
		SpreadEnabledExchanges: enabledExchanges(env(
			"FUNDING_SPREAD_ENABLED_EXCHANGES",
			defaultFundingPublicationExchanges,
		)),
		RankingEnabledExchanges: enabledExchanges(env(
			"FUNDING_RANKING_ENABLED_EXCHANGES",
			defaultFundingCollectionExchanges,
		)),
		AsterFundingAPIURL:    env("ASTER_FUNDING_API_URL", "https://fapi.asterdex.com"),
		LighterFundingAPIURL:  env("LIGHTER_FUNDING_API_URL", "https://mainnet.zklighter.elliot.ai"),
		TradefiPerpsEnabled:   boolean("TRADEFI_PERPS_ENABLED", false),
		IBKRGatewayAddr:       env("IBKR_GATEWAY_ADDR", "ib-gateway:4003"),
		IBKRDiscoveryClientID: positiveInteger("IBKR_DISCOVERY_CLIENT_ID", 30),
		ArbitrageEnabledExchanges: enabledExchanges(env(
			"TRADER_ARBITRAGE_ENABLED_EXCHANGES",
			"binance,okx,bybit,bitget,gate,hyperliquid,aster,lighter",
		)),
	}
	if c.SyncInterval <= 0 || c.HTTPTimeout <= 0 || c.InstrumentSyncInterval <= 0 ||
		c.RankingQueryTimeout <= 0 || c.RankingPoolSize <= 0 || c.RankingHistoryMaxRows <= 0 ||
		c.RankingInterval <= 0 || c.RankingTimeout <= 0 ||
		c.RankingHistoryIncrement <= 0 || c.RankingRateMaxAge <= 0 {
		return c, fmt.Errorf("durations must be positive")
	}
	if !identifier(c.ClickHouseDatabase) || !identifier(c.ClickHouseTable) {
		return c, fmt.Errorf("CLICKHOUSE_DATABASE and CLICKHOUSE_TABLE must be identifiers")
	}
	if err := validateFundingExchangeSets(c); err != nil {
		return c, err
	}
	if c.TradefiPerpsEnabled {
		if err := validateIBKRDiscovery(c.IBKRGatewayAddr, c.IBKRDiscoveryClientID); err != nil {
			return c, err
		}
	}
	return c, nil
}

func validateIBKRDiscovery(addr string, clientID int) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("IBKR_GATEWAY_ADDR must be host:port")
	}
	if clientID <= 0 {
		return fmt.Errorf("IBKR_DISCOVERY_CLIENT_ID must be a positive integer")
	}
	if clientID == 31 {
		return fmt.Errorf("IBKR_DISCOVERY_CLIENT_ID must not use the C++ producer client id 31")
	}
	return nil
}

const (
	defaultFundingCollectionExchanges  = "binance,okx,bybit,bitget,gate,hyperliquid,aster,lighter"
	defaultFundingPublicationExchanges = "binance,okx,bybit,bitget,gate,hyperliquid"
)

func validateFundingExchangeSets(c Funding) error {
	if err := requireExchangeSubset(
		c.PublishedExchanges, c.EnabledExchanges,
		"FUNDING_PUBLISHED_EXCHANGES", "ENABLED_EXCHANGES",
	); err != nil {
		return err
	}
	if err := requireExchangeSubset(
		c.SpreadEnabledExchanges, c.EnabledExchanges,
		"FUNDING_SPREAD_ENABLED_EXCHANGES", "ENABLED_EXCHANGES",
	); err != nil {
		return err
	}
	return requireExchangeSubset(
		c.RankingEnabledExchanges, c.EnabledExchanges,
		"FUNDING_RANKING_ENABLED_EXCHANGES", "ENABLED_EXCHANGES",
	)
}

func requireExchangeSubset(child, parent map[string]bool, childName, parentName string) error {
	for name := range child {
		if !parent[name] {
			return fmt.Errorf("%s must be a subset of %s (%s is not enabled)", childName, parentName, name)
		}
	}
	return nil
}

func enabledExchanges(value string) map[string]bool {
	result := make(map[string]bool)
	for _, name := range strings.Split(value, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			result[name] = true
		}
	}
	return result
}

func boolean(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func AccountFromEnv() (Account, error) {
	c := Account{
		DatabaseURL:            env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:            env("ACCOUNT_GRPC_ADDRESS", ":9091"),
		TokenSecret:            env("ACCOUNT_TOKEN_SECRET", ""),
		GatewayInternalToken:   env("GATEWAY_INTERNAL_TOKEN", ""),
		AdminInternalToken:     env("ADMIN_INTERNAL_TOKEN", ""),
		ReportInternalToken:    env("REPORT_INTERNAL_TOKEN", ""),
		TraderInternalToken:    env("TRADER_INTERNAL_TOKEN", ""),
		TokenTTL:               duration("ACCOUNT_TOKEN_TTL", 12*time.Hour),
		CredentialsKey:         env("ACCOUNT_CREDENTIALS_KEY", ""),
		PolymarketCLOBURL:      env("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"),
		PolymarketDataURL:      env("POLYMARKET_DATA_URL", "https://data-api.polymarket.com"),
		BinancePortfolioURL:    env("BINANCE_PORTFOLIO_API_URL", "https://papi.binance.com"),
		OKXAccountURL:          env("OKX_ACCOUNT_API_URL", "https://www.okx.com"),
		BitgetAccountURL:       env("BITGET_ACCOUNT_API_URL", "https://api.bitget.com"),
		BybitAccountURL:        env("BYBIT_ACCOUNT_API_URL", "https://api.bybit.com"),
		GateAccountURL:         env("GATE_ACCOUNT_API_URL", "https://api.gateio.ws"),
		HyperliquidInfoAPIURL:  env("HYPERLIQUID_INFO_API_URL", "https://api.hyperliquid.xyz"),
		AsterFuturesAPIURL:     env("ASTER_FUTURES_API_URL", "https://fapi.asterdex.com"),
		LighterAPIURL:          env("LIGHTER_API_URL", "https://mainnet.zklighter.elliot.ai"),
		AccountSnapshotTTL:     duration("ACCOUNT_SNAPSHOT_TTL", 3*time.Second),
		StatusCacheTTL:         duration("ACCOUNT_STATUS_CACHE_TTL", 30*time.Second),
		EquitySnapshotInterval: duration("ACCOUNT_EQUITY_SNAPSHOT_INTERVAL", 5*time.Minute),
		HTTPTimeout:            duration("ACCOUNT_SNAPSHOT_HTTP_TIMEOUT", 8*time.Second),
		LogLevel:               env("LOG_LEVEL", "info"),
		Development:            boolean("DEVELOPMENT", false),
	}
	if c.TokenSecret == "" {
		return c, fmt.Errorf("ACCOUNT_TOKEN_SECRET is required")
	}
	if c.CredentialsKey == "" {
		return c, fmt.Errorf("ACCOUNT_CREDENTIALS_KEY is required")
	}
	if c.GatewayInternalToken == "" {
		return c, fmt.Errorf("GATEWAY_INTERNAL_TOKEN is required")
	}
	if c.AdminInternalToken == "" {
		return c, fmt.Errorf("ADMIN_INTERNAL_TOKEN is required")
	}
	if c.ReportInternalToken == "" {
		return c, fmt.Errorf("REPORT_INTERNAL_TOKEN is required")
	}
	if c.TraderInternalToken == "" {
		return c, fmt.Errorf("TRADER_INTERNAL_TOKEN is required")
	}
	if err := requireDistinctTokens(
		c.GatewayInternalToken, c.AdminInternalToken, c.ReportInternalToken, c.TraderInternalToken,
	); err != nil {
		return c, err
	}
	if c.TokenTTL <= 0 || c.HTTPTimeout <= 0 || c.AccountSnapshotTTL <= 0 ||
		c.StatusCacheTTL <= 0 || c.EquitySnapshotInterval <= 0 {
		return c, fmt.Errorf("account durations must be positive")
	}
	return c, nil
}

func PolymarketFromEnv() (Polymarket, error) {
	c := Polymarket{
		DatabaseURL:           env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:           env("POLYMARKET_GRPC_ADDRESS", ":9092"),
		AccountGRPCAddress:    env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		GammaURL:              env("POLYMARKET_GAMMA_URL", "https://gamma-api.polymarket.com"),
		CLOBURL:               env("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"),
		CLOBWSURL:             env("POLYMARKET_CLOB_WS_URL", "wss://ws-subscriptions-clob.polymarket.com/ws/market"),
		CLOBUserWSURL:         env("POLYMARKET_CLOB_USER_WS_URL", "wss://ws-subscriptions-clob.polymarket.com/ws/user"),
		DataURL:               env("POLYMARKET_DATA_URL", "https://data-api.polymarket.com"),
		RTDSURL:               env("POLYMARKET_RTDS_URL", "wss://ws-live-data.polymarket.com"),
		ChainlinkHistoryURL:   env("POLYMARKET_CHAINLINK_HISTORY_URL", ""),
		ChainlinkHistoryToken: env("POLYMARKET_CHAINLINK_HISTORY_TOKEN", ""),
		HTTPTimeout:           duration("POLYMARKET_HTTP_TIMEOUT", 8*time.Second),
		MarketSyncInterval:    duration("POLYMARKET_MARKET_SYNC_INTERVAL", 45*time.Second),
		PositionActiveTTL:     duration("POLYMARKET_POSITION_ACTIVE_TTL", 15*time.Second),
		PositionIdleTTL:       duration("POLYMARKET_POSITION_IDLE_TTL", 90*time.Second),
		PriceRetention:        duration("POLYMARKET_PRICE_RETENTION", 48*time.Hour),
		PriceSampleInterval:   duration("POLYMARKET_PRICE_SAMPLE_INTERVAL", 5*time.Second),
		LogLevel:              env("LOG_LEVEL", "info"),
		Development:           boolean("DEVELOPMENT", false),
	}
	if c.HTTPTimeout <= 0 || c.MarketSyncInterval <= 0 ||
		c.PositionActiveTTL <= 0 || c.PositionIdleTTL <= 0 ||
		c.PriceRetention <= 0 || c.PriceSampleInterval <= 0 {
		return c, fmt.Errorf("polymarket durations must be positive")
	}
	return c, nil
}

func GatewayFromEnv() Gateway {
	return Gateway{
		HTTPAddress:           env("API_HTTP_ADDRESS", ":8080"),
		FundingGRPCAddress:    env("FUNDING_GRPC_TARGET", "localhost:9090"),
		AccountGRPCAddress:    env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		PolymarketGRPCAddress: env("POLYMARKET_GRPC_TARGET", "localhost:9092"),
		ReportGRPCAddress:     env("REPORT_GRPC_TARGET", "localhost:9094"),
		TraderGRPCAddress:     env("TRADER_GRPC_TARGET", "localhost:9095"),
		AIGRPCAddress:         env("AI_GRPC_TARGET", "localhost:9096"),
		SpreadGRPCAddress:     env("SPREAD_GRPC_TARGET", "localhost:9097"),
		JevGRPCAddress:        strings.TrimSpace(os.Getenv("JEV_GRPC_TARGET")),
		AdminGRPCAddress:      strings.TrimSpace(os.Getenv("ADMIN_GRPC_TARGET")),
		SessionCookieName:     env("ACCOUNT_SESSION_COOKIE", "sq_session"),
		SessionCookieSecure:   boolean("ACCOUNT_COOKIE_SECURE", false),
		SessionCookieMaxAge:   duration("ACCOUNT_TOKEN_TTL", 12*time.Hour),
		GatewayInternalToken:  strings.TrimSpace(os.Getenv("GATEWAY_INTERNAL_TOKEN")),
		BrokerJWTPublicKey:    strings.TrimSpace(os.Getenv("BROKER_JWT_PUBLIC_KEY_FILE")),
		BrokerAuthCacheTTL:    duration("BROKER_AUTH_STATE_CACHE_TTL", 30*time.Second),
		CORSOrigin:            env("GATEWAY_CORS_ORIGIN", ""),
		LogLevel:              env("LOG_LEVEL", "info"),
	}
}

func AIFromEnv() (AI, error) {
	c := AI{
		DatabaseURL:                   env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:                   env("AI_GRPC_ADDRESS", ":9096"),
		AccountGRPCAddress:            env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		FundingGRPCAddress:            env("FUNDING_GRPC_TARGET", "localhost:9090"),
		OpenRouterBaseURL:             env("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		OpenRouterModel:               env("OPENROUTER_MODEL", "openrouter/free"),
		OpenRouterReferer:             env("OPENROUTER_HTTP_REFERER", "https://self-quant.local"),
		OpenRouterTitle:               env("OPENROUTER_TITLE", "Self Quant"),
		HTTPTimeout:                   duration("AI_HTTP_TIMEOUT", 90*time.Second),
		MaxInputRounds:                positiveInteger("AI_MAX_INPUT_ROUNDS", 12),
		MaxInputChars:                 positiveInteger("AI_MAX_INPUT_CHARS", 16000),
		MaxOutputTokens:               positiveInteger("AI_MAX_OUTPUT_TOKENS", 2048),
		ContextMaxSymbolRows:          positiveInteger("AI_CONTEXT_MAX_SYMBOL_ROWS", 24),
		ContextMaxRows:                positiveInteger("AI_CONTEXT_MAX_ROWS", 40),
		ContextMinTurnover:            number("AI_CONTEXT_MIN_TURNOVER_USD", 5_000_000),
		ContextMaxChars:               positiveInteger("AI_CONTEXT_MAX_CHARS", 8000),
		ConversationRetention:         duration("AI_CONVERSATION_RETENTION", 168*time.Hour),
		ConversationLease:             duration("AI_CONVERSATION_LEASE", 2*time.Minute),
		CleanupInterval:               duration("AI_CLEANUP_INTERVAL", time.Hour),
		CleanupBatch:                  positiveInteger("AI_CLEANUP_BATCH", 500),
		CleanupMaxBatches:             positiveInteger("AI_CLEANUP_MAX_BATCHES", 10),
		ChatInputTokenBudget:          positiveInteger("AI_CHAT_INPUT_TOKEN_BUDGET", 12000),
		RecentMessagePairs:            positiveInteger("AI_RECENT_MESSAGE_PAIRS", 6),
		SummaryTriggerTokens:          positiveInteger("AI_SUMMARY_TRIGGER_TOKENS", 4000),
		SummaryMaxChars:               positiveInteger("AI_SUMMARY_MAX_CHARS", 2000),
		SummaryMaxOutputTokens:        positiveInteger("AI_SUMMARY_MAX_OUTPUT_TOKENS", 512),
		MaxConversationsPerUser:       positiveInteger("AI_MAX_CONVERSATIONS_PER_USER", 5),
		MaxRawMessagesPerConversation: positiveInteger("AI_MAX_RAW_MESSAGES_PER_CONVERSATION", 1000),
		LogLevel:                      env("LOG_LEVEL", "info"),
		Development:                   boolean("DEVELOPMENT", false),
	}
	if c.HTTPTimeout <= 0 || c.MaxInputRounds <= 0 || c.MaxInputChars <= 0 ||
		c.MaxOutputTokens <= 0 || c.ContextMaxSymbolRows <= 0 ||
		c.ContextMaxRows <= 0 || c.ContextMinTurnover < 0 || c.ContextMaxChars <= 0 ||
		c.ConversationRetention <= 0 || c.ConversationLease <= 0 ||
		c.CleanupInterval <= 0 || c.CleanupBatch <= 0 || c.CleanupMaxBatches <= 0 ||
		c.ChatInputTokenBudget <= 0 || c.RecentMessagePairs <= 0 ||
		c.SummaryTriggerTokens <= 0 || c.SummaryMaxChars <= 0 ||
		c.SummaryMaxOutputTokens <= 0 || c.MaxConversationsPerUser <= 0 ||
		c.MaxRawMessagesPerConversation <= 0 {
		return c, fmt.Errorf("ai limits and durations must be positive")
	}
	if _, err := url.ParseRequestURI(c.OpenRouterBaseURL); err != nil {
		return c, fmt.Errorf("invalid OPENROUTER_BASE_URL: %w", err)
	}
	return c, nil
}

func TraderFromEnv() (Trader, error) {
	c := Trader{
		DatabaseURL:                  env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:                  env("TRADER_GRPC_ADDRESS", ":9095"),
		AccountGRPCAddress:           env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		BinanceURL:                   env("BINANCE_PORTFOLIO_API_URL", "https://papi.binance.com"),
		OKXURL:                       env("OKX_ACCOUNT_API_URL", "https://www.okx.com"),
		BybitURL:                     env("BYBIT_ACCOUNT_API_URL", "https://api.bybit.com"),
		BitgetURL:                    env("BITGET_ACCOUNT_API_URL", "https://api.bitget.com"),
		GateURL:                      env("GATE_ACCOUNT_API_URL", "https://api.gateio.ws"),
		HyperliquidURL:               env("HYPERLIQUID_INFO_API_URL", "https://api.hyperliquid.xyz"),
		AsterURL:                     env("ASTER_FUTURES_API_URL", "https://fapi.asterdex.com"),
		LighterURL:                   env("LIGHTER_API_URL", "https://mainnet.zklighter.elliot.ai"),
		HTTPTimeout:                  duration("TRADER_HTTP_TIMEOUT", 12*time.Second),
		InternalToken:                strings.TrimSpace(os.Getenv("TRADER_INTERNAL_TOKEN")),
		AdminInternalToken:           strings.TrimSpace(os.Getenv("ADMIN_INTERNAL_TOKEN")),
		ReconcileInterval:            duration("TRADER_RECONCILE_INTERVAL", time.Second),
		ReconcileBatchSize:           positiveInteger("TRADER_RECONCILE_BATCH_SIZE", 20),
		ReconcileWorkers:             positiveInteger("TRADER_RECONCILE_WORKERS", 3),
		TwapScheduleInterval:         duration("TRADER_TWAP_SCHEDULE_INTERVAL", 500*time.Millisecond),
		TwapScheduleLease:            duration("TRADER_TWAP_SCHEDULE_LEASE", 30*time.Second),
		TwapScheduleBatch:            positiveInteger("TRADER_TWAP_SCHEDULE_BATCH", 20),
		TwapScheduleWorkers:          positiveInteger("TRADER_TWAP_SCHEDULE_WORKERS", 4),
		TwapRetention:                duration("TRADER_TWAP_RETENTION", 7*24*time.Hour),
		TwapCleanupInterval:          duration("TRADER_TWAP_CLEANUP_INTERVAL", time.Hour),
		TwapCleanupBatch:             positiveInteger("TRADER_TWAP_CLEANUP_BATCH", 100),
		TwapCleanupMaxBatches:        positiveInteger("TRADER_TWAP_CLEANUP_MAX_BATCHES", 10),
		ArbitrageControlInterval:     duration("TRADER_ARBITRAGE_CONTROL_INTERVAL", time.Second),
		ArbitrageCoalesceWindow:      duration("TRADER_ARBITRAGE_COALESCE_WINDOW", 50*time.Millisecond),
		ArbitrageScheduleLease:       duration("TRADER_ARBITRAGE_SCHEDULE_LEASE", 30*time.Second),
		ArbitrageScheduleBatch:       positiveInteger("TRADER_ARBITRAGE_SCHEDULE_BATCH", 50),
		ArbitrageScheduleWorkers:     positiveInteger("TRADER_ARBITRAGE_SCHEDULE_WORKERS", 16),
		ArbitrageBBOStale:            duration("TRADER_ARBITRAGE_BBO_STALE", 60*time.Second),
		ArbitrageSignalBBOStale:      duration("TRADER_ARBITRAGE_SIGNAL_BBO_STALE", 2*time.Second),
		ArbitrageBBOReconnectInitial: duration("TRADER_ARBITRAGE_BBO_RECONNECT_INITIAL", 250*time.Millisecond),
		ArbitrageBBOReconnectMax:     duration("TRADER_ARBITRAGE_BBO_RECONNECT_MAX", 30*time.Second),
		ArbitrageBBOHeartbeat:        duration("TRADER_ARBITRAGE_BBO_HEARTBEAT", 20*time.Second),
		ArbitrageBBOReadWait:         duration("TRADER_ARBITRAGE_BBO_READ_WAIT", 45*time.Second),
		ArbitrageBBOWriteWait:        duration("TRADER_ARBITRAGE_BBO_WRITE_WAIT", 5*time.Second),
		ArbitrageBBOAckWait:          duration("TRADER_ARBITRAGE_BBO_ACK_WAIT", 5*time.Second),
		ArbitrageRepriceTicks:        positiveInteger("TRADER_ARBITRAGE_REPRICE_TICKS", 2),
		ArbitrageObserverInterval:    duration("TRADER_ARBITRAGE_OBSERVER_INTERVAL", 200*time.Millisecond),
		ArbitrageIOCProtectionBps:    positiveInteger("TRADER_ARBITRAGE_IOC_PROTECTION_BPS", 10),
		ArbitrageIOCRetries:          positiveInteger("TRADER_ARBITRAGE_IOC_RETRIES", 3),
		ArbitrageOrderStreamEnabled:  boolean("TRADER_ARBITRAGE_ORDER_STREAM_ENABLED", true),
		BinanceOrderWSURL:            strings.TrimSpace(os.Getenv("BINANCE_ORDER_WS_URL")),
		OKXOrderWSURL:                strings.TrimSpace(os.Getenv("OKX_ORDER_WS_URL")),
		BybitOrderWSURL:              strings.TrimSpace(os.Getenv("BYBIT_ORDER_WS_URL")),
		BitgetOrderWSURL:             strings.TrimSpace(os.Getenv("BITGET_ORDER_WS_URL")),
		GateOrderWSURL:               strings.TrimSpace(os.Getenv("GATE_ORDER_WS_URL")),
		HyperliquidOrderWSURL:        strings.TrimSpace(os.Getenv("HYPERLIQUID_ORDER_WS_URL")),
		AsterOrderWSURL:              strings.TrimSpace(os.Getenv("ASTER_ORDER_WS_URL")),
		LighterOrderWSURL:            strings.TrimSpace(os.Getenv("LIGHTER_ORDER_WS_URL")),
		OrderStreamReconnectInitial:  duration("TRADER_ORDER_STREAM_RECONNECT_INITIAL", 250*time.Millisecond),
		OrderStreamReconnectMax:      duration("TRADER_ORDER_STREAM_RECONNECT_MAX", 30*time.Second),
		OrderStreamHeartbeat:         duration("TRADER_ORDER_STREAM_HEARTBEAT", 15*time.Second),
		OrderStreamStale:             duration("TRADER_ORDER_STREAM_STALE", 45*time.Second),
		OrderStreamListenKeyRefresh:  duration("TRADER_ORDER_STREAM_LISTEN_KEY_REFRESH", 30*time.Minute),
		OrderStreamRESTAudit:         duration("TRADER_ORDER_STREAM_REST_AUDIT", 30*time.Second),
		OrderStreamSessionIdle:       duration("TRADER_ORDER_STREAM_SESSION_IDLE", 5*time.Minute),
		ArbitrageMaxActiveAccount:    positiveInteger("TRADER_ARBITRAGE_MAX_ACTIVE_PER_ACCOUNT", 4),
		ArbitrageMaxActiveVenue:      positiveInteger("TRADER_ARBITRAGE_MAX_ACTIVE_PER_VENUE", 16),
		ArbitrageRetention:           duration("TRADER_ARBITRAGE_RETENTION", 7*24*time.Hour),
		ArbitrageCleanupInterval:     duration("TRADER_ARBITRAGE_CLEANUP_INTERVAL", time.Hour),
		ArbitrageCleanupBatch:        positiveInteger("TRADER_ARBITRAGE_CLEANUP_BATCH", 100),
		ArbitrageCleanupMaxBatches:   positiveInteger("TRADER_ARBITRAGE_CLEANUP_MAX_BATCHES", 10),
		ArbitrageDryRun:              boolean("TRADER_ARBITRAGE_DRY_RUN", true),
		ArbitrageEnabledExchanges: enabledExchanges(env(
			"TRADER_ARBITRAGE_ENABLED_EXCHANGES",
			"binance,okx,bybit,bitget,gate,hyperliquid,aster,lighter",
		)),
		LogLevel:    env("LOG_LEVEL", "info"),
		Development: boolean("DEVELOPMENT", false),
	}
	emergencyAfter, emergencySet, err := strictDuration("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER")
	if err != nil {
		return c, err
	}
	if !emergencySet {
		emergencyAfter = 2 * time.Second
	}
	if emergencyAfter < 0 {
		return c, fmt.Errorf("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER must be >= 0")
	}
	if emergencyAfter != 0 &&
		(emergencyAfter < 100*time.Millisecond || emergencyAfter > 30*time.Second) {
		return c, fmt.Errorf("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER must be 0 or between 100ms and 30s")
	}
	c.ArbitrageHedgeEmergencyAfter = emergencyAfter
	if c.HTTPTimeout <= 0 || c.ReconcileInterval <= 0 ||
		c.ReconcileBatchSize <= 0 || c.ReconcileWorkers <= 0 ||
		c.TwapScheduleInterval <= 0 || c.TwapScheduleLease <= 0 ||
		c.TwapScheduleBatch <= 0 || c.TwapScheduleWorkers <= 0 ||
		c.TwapRetention <= 0 || c.TwapCleanupInterval <= 0 ||
		c.TwapCleanupBatch <= 0 || c.TwapCleanupMaxBatches <= 0 ||
		c.ArbitrageControlInterval <= 0 || c.ArbitrageScheduleLease <= 0 ||
		c.ArbitrageScheduleBatch <= 0 || c.ArbitrageScheduleWorkers <= 0 ||
		c.ArbitrageBBOStale <= 0 ||
		c.ArbitrageSignalBBOStale <= 0 ||
		c.ArbitrageBBOReconnectInitial <= 0 ||
		c.ArbitrageBBOReconnectMax < c.ArbitrageBBOReconnectInitial ||
		c.ArbitrageBBOHeartbeat <= 0 ||
		c.ArbitrageBBOReadWait <= c.ArbitrageBBOHeartbeat ||
		c.ArbitrageBBOWriteWait <= 0 || c.ArbitrageBBOAckWait <= 0 ||
		c.ArbitrageRepriceTicks <= 0 ||
		c.ArbitrageObserverInterval <= 0 || c.ArbitrageIOCProtectionBps <= 0 ||
		c.ArbitrageIOCRetries <= 0 || c.OrderStreamReconnectInitial <= 0 ||
		c.OrderStreamReconnectMax < c.OrderStreamReconnectInitial ||
		c.OrderStreamHeartbeat <= 0 || c.OrderStreamStale <= c.OrderStreamHeartbeat ||
		c.OrderStreamListenKeyRefresh <= 0 || c.OrderStreamRESTAudit <= 0 ||
		c.OrderStreamSessionIdle <= 0 ||
		c.ArbitrageMaxActiveAccount <= 0 ||
		c.ArbitrageMaxActiveVenue <= 0 || c.ArbitrageRetention <= 0 ||
		c.ArbitrageCleanupInterval <= 0 || c.ArbitrageCleanupBatch <= 0 ||
		c.ArbitrageCleanupMaxBatches <= 0 {
		return c, fmt.Errorf("trader durations must be positive")
	}
	if c.ArbitrageCoalesceWindow < 20*time.Millisecond ||
		c.ArbitrageCoalesceWindow > 100*time.Millisecond {
		return c, fmt.Errorf("TRADER_ARBITRAGE_COALESCE_WINDOW must be between 20ms and 100ms")
	}
	if c.InternalToken == "" {
		return c, fmt.Errorf("TRADER_INTERNAL_TOKEN is required")
	}
	if c.AdminInternalToken == "" {
		return c, fmt.Errorf("ADMIN_INTERNAL_TOKEN is required")
	}
	if err := requireDistinctTokens(c.InternalToken, c.AdminInternalToken); err != nil {
		return c, err
	}
	return c, nil
}

func AdminFromEnv() (Admin, error) {
	c := Admin{
		DatabaseURL:          env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:          env("ADMIN_GRPC_ADDRESS", ":9099"),
		AccountGRPCAddress:   env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		TraderGRPCAddress:    env("TRADER_GRPC_TARGET", "localhost:9095"),
		GatewayInternalToken: strings.TrimSpace(os.Getenv("GATEWAY_INTERNAL_TOKEN")),
		AdminInternalToken:   strings.TrimSpace(os.Getenv("ADMIN_INTERNAL_TOKEN")),
		JWTPrivateKeyFile:    strings.TrimSpace(os.Getenv("BROKER_JWT_PRIVATE_KEY_FILE")),
		AccessTokenTTL:       duration("BROKER_ACCESS_TOKEN_TTL", 24*time.Hour),
		LogLevel:             env("LOG_LEVEL", "info"),
		Development:          boolean("DEVELOPMENT", false),
	}
	if c.GatewayInternalToken == "" {
		return c, fmt.Errorf("GATEWAY_INTERNAL_TOKEN is required")
	}
	if c.AdminInternalToken == "" {
		return c, fmt.Errorf("ADMIN_INTERNAL_TOKEN is required")
	}
	if c.JWTPrivateKeyFile == "" {
		return c, fmt.Errorf("BROKER_JWT_PRIVATE_KEY_FILE is required")
	}
	if err := requireDistinctTokens(c.GatewayInternalToken, c.AdminInternalToken); err != nil {
		return c, err
	}
	if c.AccessTokenTTL <= 0 {
		return c, fmt.Errorf("BROKER_ACCESS_TOKEN_TTL must be positive")
	}
	return c, nil
}

func SpreadFromEnv() (Spread, error) {
	c := Spread{
		GRPCAddress:        env("SPREAD_GRPC_ADDRESS", ":9097"),
		ClickHouseAddr:     env("CLICKHOUSE_ADDR", "127.0.0.1:9000"),
		ClickHouseDatabase: env("CLICKHOUSE_DATABASE", "market_data"),
		ClickHouseTable:    env("CLICKHOUSE_TABLE", "crypto_bbo"),
		ClickHouseUser:     env("CLICKHOUSE_USER", "bbo_writer"),
		ClickHousePassword: env("CLICKHOUSE_PASSWORD", env("CLICKHOUSE_BBO_PASSWORD", "")),
		ClickHouseTLS:      boolean("CLICKHOUSE_TLS", false),
		ClickHouseTLSSkip:  boolean("CLICKHOUSE_TLS_SKIP_VERIFY", false),
		QueryTimeout:       duration("CLICKHOUSE_QUERY_TIMEOUT", 5*time.Second),
		PoolSize:           positiveInteger("CLICKHOUSE_POOL_SIZE", 8),
		MaxConcurrentQuery: positiveInteger("SPREAD_MAX_CONCURRENT_QUERIES", 8),
		CacheTTL:           duration("SPREAD_CACHE_TTL", 30*time.Second),
		LogLevel:           env("LOG_LEVEL", "info"),
		Development:        boolean("DEVELOPMENT", false),
	}
	if strings.TrimSpace(c.ClickHouseAddr) == "" {
		return c, fmt.Errorf("CLICKHOUSE_ADDR is required")
	}
	if !identifier(c.ClickHouseDatabase) || !identifier(c.ClickHouseTable) {
		return c, fmt.Errorf("CLICKHOUSE_DATABASE and CLICKHOUSE_TABLE must be identifiers")
	}
	if c.QueryTimeout <= 0 || c.CacheTTL <= 0 || c.PoolSize <= 0 || c.MaxConcurrentQuery <= 0 {
		return c, fmt.Errorf("spread durations and pool limits must be positive")
	}
	return c, nil
}

func JevFromEnv() (Jev, error) {
	c := Jev{
		DatabaseURL:                 env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:                 env("JEV_GRPC_ADDRESS", ":9098"),
		FundingGRPCAddress:          env("FUNDING_GRPC_TARGET", "localhost:9090"),
		ClickHouseAddr:              env("CLICKHOUSE_ADDR", "127.0.0.1:9000"),
		ClickHouseDatabase:          env("CLICKHOUSE_DATABASE", "market_data"),
		ClickHouseTable:             env("CLICKHOUSE_TABLE", "crypto_bbo"),
		ClickHouseUser:              env("CLICKHOUSE_USER", "bbo_writer"),
		ClickHousePassword:          env("CLICKHOUSE_PASSWORD", env("CLICKHOUSE_BBO_PASSWORD", "")),
		ClickHouseTLS:               boolean("CLICKHOUSE_TLS", false),
		ClickHouseTLSSkip:           boolean("CLICKHOUSE_TLS_SKIP_VERIFY", false),
		QueryTimeout:                duration("CLICKHOUSE_QUERY_TIMEOUT", 5*time.Second),
		PoolSize:                    positiveInteger("CLICKHOUSE_POOL_SIZE", 8),
		Enabled:                     boolean("JEV_ENABLED", true),
		TypeSafeAPIKey:              strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")),
		TypeSafeBaseURL:             env("TYPESAFE_BASE_URL", "https://api.typesafe.ai/v1/systemone"),
		TypeSafeModel:               env("TYPESAFE_MODEL", "jev-latest"),
		TypeSafeTimeout:             duration("JEV_TYPESAFE_TIMEOUT", 3*time.Second),
		CycleInterval:               duration("JEV_CYCLE_INTERVAL", 60*time.Second),
		FillWait:                    duration("JEV_FILL_WAIT", 5*time.Second),
		BBOMaxAge:                   duration("JEV_BBO_MAX_AGE", 60*time.Second),
		FundingPriceMaxAge:          duration("JEV_FUNDING_PRICE_MAX_AGE", 60*time.Second),
		RateMaxAge:                  duration("JEV_RATE_MAX_AGE", 15*time.Minute),
		MaxHoldDuration:             duration("JEV_MAX_HOLD_DURATION", 72*time.Hour),
		MaxConcurrentDecisions:      positiveInteger("JEV_MAX_CONCURRENT_DECISIONS", 2),
		MaxEntryDecisionsPerCycle:   positiveInteger("JEV_MAX_ENTRY_DECISIONS_PER_CYCLE", 8),
		MaxOpenPositions:            positiveInteger("JEV_MAX_OPEN_POSITIONS", 8),
		USDTUSDCOneToOne:            boolean("JEV_USDT_USDC_ONE_TO_ONE", true),
		BasisLookback:               duration("JEV_BASIS_LOOKBACK", 7*24*time.Hour),
		BasisForecastHorizon:        duration("JEV_BASIS_FORECAST_HORIZON", 24*time.Hour),
		BasisRefreshInterval:        duration("JEV_BASIS_REFRESH_INTERVAL", 10*time.Minute),
		BasisCacheMaxAge:            duration("JEV_BASIS_CACHE_MAX_AGE", 20*time.Minute),
		BasisConfidenceSampleTarget: positiveInteger("JEV_BASIS_CONFIDENCE_SAMPLE_TARGET", 48),
		ExitConfirmCycles:           positiveInteger("JEV_EXIT_CONFIRM_CYCLES", 3),
		MissingBBOForceAfter:        duration("JEV_MISSING_BBO_FORCE_AFTER", 15*time.Minute),
		LogLevel:                    env("LOG_LEVEL", "info"),
		Development:                 boolean("DEVELOPMENT", false),
	}
	var err error
	if c.MinTurnoverUSD, err = positiveDecimalEnv("JEV_MIN_TURNOVER_USD", "1000000"); err != nil {
		return c, err
	}
	if c.MinDepthUSD, err = positiveDecimalEnv("JEV_MIN_DEPTH_USD", "5000"); err != nil {
		return c, err
	}
	if c.PositionNotionalUSDT, err = positiveDecimalEnv("JEV_POSITION_NOTIONAL_USDT", "100"); err != nil {
		return c, err
	}
	if c.PaperRoundTripCostBPS, err = positiveDecimalEnv("JEV_PAPER_ROUND_TRIP_COST_BPS", "12"); err != nil {
		return c, err
	}
	if c.PaperInitialEquity, err = positiveDecimalEnv("JEV_PAPER_INITIAL_EQUITY", "10000"); err != nil {
		return c, err
	}
	if c.MinExitProbability, err = unitIntervalDecimalEnv("JEV_MIN_EXIT_PROBABILITY", "0.6"); err != nil {
		return c, err
	}
	if c.MinExpectedNetReturn24h, err = nonNegativeDecimalEnv("JEV_MIN_EXPECTED_NET_RETURN_24H", "0.002"); err != nil {
		return c, err
	}
	if c.MinConservativeNetReturn24h, err = nonNegativeDecimalEnv("JEV_MIN_CONSERVATIVE_NET_RETURN_24H", "0.0005"); err != nil {
		return c, err
	}
	if c.MinNetProfitProbability, err = unitIntervalDecimalEnv("JEV_MIN_NET_PROFIT_PROBABILITY", "0.60"); err != nil {
		return c, err
	}
	if c.MinBasisForecastConfidence, err = unitIntervalDecimalEnv("JEV_MIN_BASIS_FORECAST_CONFIDENCE", "0.60"); err != nil {
		return c, err
	}
	if c.MinEnterProbability, err = unitIntervalDecimalEnv("JEV_MIN_ENTER_PROBABILITY", "0.70"); err != nil {
		return c, err
	}
	if c.MinEnterConfidence, err = unitIntervalDecimalEnv("JEV_MIN_ENTER_CONFIDENCE", "0.70"); err != nil {
		return c, err
	}
	if c.BasisMatchBandBPS, err = positiveDecimalEnv("JEV_BASIS_MATCH_BAND_BPS", "20"); err != nil {
		return c, err
	}
	if c.StopLossNetUSDT, err = decimalEnv("JEV_STOP_LOSS_NET_USDT", "-1"); err != nil {
		return c, err
	}
	if c.StopLossNetUSDT.IsPositive() {
		return c, fmt.Errorf("JEV_STOP_LOSS_NET_USDT must be <= 0")
	}
	if c.TakeProfitPadUSDT, err = nonNegativeDecimalEnv("JEV_TAKE_PROFIT_PAD_USDT", "0.05"); err != nil {
		return c, err
	}
	if c.ProbabilitySumTolerance, err = unitIntervalDecimalEnv("JEV_PROBABILITY_SUM_TOLERANCE", "0.02"); err != nil {
		return c, err
	}
	if strings.TrimSpace(c.ClickHouseAddr) == "" {
		return c, fmt.Errorf("CLICKHOUSE_ADDR is required")
	}
	if !identifier(c.ClickHouseDatabase) || !identifier(c.ClickHouseTable) {
		return c, fmt.Errorf("CLICKHOUSE_DATABASE and CLICKHOUSE_TABLE must be identifiers")
	}
	if strings.TrimSpace(c.TypeSafeBaseURL) == "" || strings.TrimSpace(c.TypeSafeModel) == "" {
		return c, fmt.Errorf("TYPESAFE_BASE_URL and TYPESAFE_MODEL are required")
	}
	if c.QueryTimeout <= 0 || c.TypeSafeTimeout <= 0 || c.CycleInterval <= 0 ||
		c.FillWait <= 0 || c.BBOMaxAge <= 0 || c.FundingPriceMaxAge <= 0 ||
		c.RateMaxAge <= 0 || c.MaxHoldDuration <= 0 || c.MissingBBOForceAfter <= 0 {
		return c, fmt.Errorf("jev durations must be positive")
	}
	if c.PoolSize <= 0 || c.MaxConcurrentDecisions <= 0 || c.MaxEntryDecisionsPerCycle <= 0 ||
		c.MaxOpenPositions <= 0 || c.BasisConfidenceSampleTarget <= 0 || c.ExitConfirmCycles <= 0 {
		return c, fmt.Errorf("jev pool and position limits must be positive")
	}
	return c, nil
}

func positiveDecimalEnv(key, fallback string) (decimal.Decimal, error) {
	parsed, err := decimalEnv(key, fallback)
	if err != nil || !parsed.IsPositive() {
		return decimal.Zero, fmt.Errorf("%s must be a decimal > 0", key)
	}
	return parsed, nil
}

func nonNegativeDecimalEnv(key, fallback string) (decimal.Decimal, error) {
	parsed, err := decimalEnv(key, fallback)
	if err != nil || parsed.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be a decimal >= 0", key)
	}
	return parsed, nil
}

func decimalEnv(key, fallback string) (decimal.Decimal, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		value = fallback
	}
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s must be a decimal", key)
	}
	return parsed, nil
}

func unitIntervalDecimalEnv(key, fallback string) (decimal.Decimal, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		value = fallback
	}
	parsed, err := decimal.NewFromString(value)
	if err != nil || parsed.IsNegative() || parsed.GreaterThan(decimal.NewFromInt(1)) {
		return decimal.Zero, fmt.Errorf("%s must be a decimal in [0, 1]", key)
	}
	return parsed, nil
}

func identifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func ReportFromEnv() (Report, error) {
	c := Report{
		DatabaseURL:        env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:        env("REPORT_GRPC_ADDRESS", ":9094"),
		AccountGRPCAddress: env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		InternalToken:      strings.TrimSpace(os.Getenv("REPORT_INTERNAL_TOKEN")),
		Timezone:           env("REPORT_TIMEZONE", "Asia/Shanghai"),
		LogLevel:           env("LOG_LEVEL", "info"),
		Development:        boolean("DEVELOPMENT", false),
	}
	if c.InternalToken == "" {
		return c, fmt.Errorf("REPORT_INTERNAL_TOKEN is required")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return c, fmt.Errorf("invalid REPORT_TIMEZONE: %w", err)
	}
	return c, nil
}

func LoggerLevel(s string) int {
	switch s {
	case "debug":
		return -4
	case "warn":
		return 4
	case "error":
		return 8
	default:
		return 0
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func strictDuration(key string) (time.Duration, bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, false, nil
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed, true, nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second, true, nil
	}
	return 0, true, fmt.Errorf("%s must be a duration", key)
}

func positiveInteger(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func number(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func requireDistinctTokens(tokens ...string) error {
	for i := 0; i < len(tokens); i++ {
		left := strings.TrimSpace(tokens[i])
		if left == "" {
			return fmt.Errorf("internal service tokens must be non-empty")
		}
		for j := i + 1; j < len(tokens); j++ {
			if left == strings.TrimSpace(tokens[j]) {
				return fmt.Errorf("internal service tokens must be unique")
			}
		}
	}
	return nil
}
