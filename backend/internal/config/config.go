package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Funding struct {
	DatabaseURL            string
	GRPCAddress            string
	SyncInterval           time.Duration
	HTTPTimeout            time.Duration
	InstrumentSyncInterval time.Duration
	ClickHouseAddr         string
	ClickHouseDatabase     string
	ClickHouseTable        string
	ClickHouseUser         string
	ClickHousePassword     string
	ClickHouseTLS          bool
	ClickHouseTLSSkip      bool
	RankingQueryTimeout    time.Duration
	RankingPoolSize        int
	Ranking1hInterval      time.Duration
	RankingSlowInterval    time.Duration
	RankingEnabled         bool
	RankingCacheEnabled    bool
	RankingMinuteEnabled   bool
	RankingHistoryMaxRows  int
	LogLevel               string
	Development            bool
	EnabledExchanges       map[string]bool
}

type Account struct {
	DatabaseURL         string
	GRPCAddress         string
	TokenSecret         string
	ReportInternalToken string
	TraderInternalToken string
	TokenTTL            time.Duration
	CredentialsKey      string
	PolymarketCLOBURL   string
	PolymarketDataURL   string
	BinancePortfolioURL string
	OKXAccountURL       string
	BitgetAccountURL    string
	BybitAccountURL     string
	GateAccountURL      string
	AccountSnapshotTTL  time.Duration
	HTTPTimeout         time.Duration
	LogLevel            string
	Development         bool
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
	SessionCookieName     string
	SessionCookieSecure   bool
	SessionCookieMaxAge   time.Duration
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
	DatabaseURL                 string
	GRPCAddress                 string
	AccountGRPCAddress          string
	BinanceURL                  string
	OKXURL                      string
	BybitURL                    string
	BitgetURL                   string
	GateURL                     string
	HTTPTimeout                 time.Duration
	InternalToken               string
	ReconcileInterval           time.Duration
	ReconcileBatchSize          int
	ReconcileWorkers            int
	TwapScheduleInterval        time.Duration
	TwapScheduleLease           time.Duration
	TwapScheduleBatch           int
	TwapScheduleWorkers         int
	TwapRetention               time.Duration
	TwapCleanupInterval         time.Duration
	TwapCleanupBatch            int
	TwapCleanupMaxBatches       int
	ArbitrageScheduleInterval   time.Duration
	ArbitrageScheduleLease      time.Duration
	ArbitrageScheduleBatch      int
	ArbitrageScheduleWorkers    int
	ArbitrageBBOStale           time.Duration
	ArbitrageRepriceTicks       int
	ArbitrageObserverInterval   time.Duration
	ArbitrageIOCProtectionTicks int
	ArbitrageIOCRetries         int
	ArbitrageOrderStreamEnabled bool
	BinanceOrderWSURL           string
	OKXOrderWSURL               string
	BybitOrderWSURL             string
	BitgetOrderWSURL            string
	GateOrderWSURL              string
	OrderStreamReconnectInitial time.Duration
	OrderStreamReconnectMax     time.Duration
	OrderStreamHeartbeat        time.Duration
	OrderStreamStale            time.Duration
	OrderStreamListenKeyRefresh time.Duration
	OrderStreamRESTAudit        time.Duration
	OrderStreamSessionIdle      time.Duration
	ArbitrageMaxActiveAccount   int
	ArbitrageMaxActiveVenue     int
	ArbitrageRetention          time.Duration
	ArbitrageCleanupInterval    time.Duration
	ArbitrageCleanupBatch       int
	ArbitrageCleanupMaxBatches  int
	ArbitrageDryRun             bool
	LogLevel                    string
	Development                 bool
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

type AggData struct {
	HTTPAddress               string
	RecordingDir              string
	MDSGatewayURL             string
	MDSGatewayToken           string
	AllowedOrigins            []string
	BrowserToken              string
	MaxClients                int
	MaxSubscriptions          int
	ManifestRefresh           time.Duration
	StreamInterval            time.Duration
	HistoryWorkers            int
	HistoryResolution         time.Duration
	DatabaseURL               string
	FairPriceEnabled          bool
	FairPriceDepth            int
	FairPriceLambdaPerBP      float64
	FairPriceImbalanceAlpha   float64
	FairPriceEWMATau          time.Duration
	FairPriceImpactNotional   float64
	FairPriceVenueDominance   float64
	FairPriceStreamInterval   time.Duration
	FairPriceHeartbeat        time.Duration
	FairPricePersistEnabled   bool
	FairPriceSampleInterval   time.Duration
	FairPriceRetention        time.Duration
	FairPriceCleanupInterval  time.Duration
	FairPriceDeleteBatch      int
	FairPriceDeleteMaxBatches int
	FairPriceDBTimeout        time.Duration
	Development               bool
	LogLevel                  string
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
		Ranking1hInterval:      duration("FUNDING_RANKING_1H_INTERVAL", time.Minute),
		RankingSlowInterval:    duration("FUNDING_RANKING_SLOW_INTERVAL", 10*time.Minute),
		RankingEnabled:         boolean("FUNDING_RANKING_ENABLED", true),
		RankingCacheEnabled:    boolean("FUNDING_RANKING_HISTORY_CACHE_ENABLED", true),
		RankingMinuteEnabled:   boolean("FUNDING_RANKING_MINUTE_TABLE_ENABLED", true),
		RankingHistoryMaxRows:  positiveInteger("FUNDING_RANKING_HISTORY_MAX_ROWS", 12_000_000),
		LogLevel:               env("LOG_LEVEL", "info"),
		Development:            boolean("DEVELOPMENT", false),
		EnabledExchanges: enabledExchanges(env(
			"ENABLED_EXCHANGES",
			"binance,okx,bybit,bitget,gate,hyperliquid",
		)),
	}
	if c.SyncInterval <= 0 || c.HTTPTimeout <= 0 || c.InstrumentSyncInterval <= 0 ||
		c.RankingQueryTimeout <= 0 || c.RankingPoolSize <= 0 || c.RankingHistoryMaxRows <= 0 ||
		c.Ranking1hInterval <= 0 || c.RankingSlowInterval <= 0 {
		return c, fmt.Errorf("durations must be positive")
	}
	if !identifier(c.ClickHouseDatabase) || !identifier(c.ClickHouseTable) {
		return c, fmt.Errorf("CLICKHOUSE_DATABASE and CLICKHOUSE_TABLE must be identifiers")
	}
	return c, nil
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
		DatabaseURL:         env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:         env("ACCOUNT_GRPC_ADDRESS", ":9091"),
		TokenSecret:         env("ACCOUNT_TOKEN_SECRET", ""),
		ReportInternalToken: env("REPORT_INTERNAL_TOKEN", ""),
		TraderInternalToken: env("TRADER_INTERNAL_TOKEN", ""),
		TokenTTL:            duration("ACCOUNT_TOKEN_TTL", 12*time.Hour),
		CredentialsKey:      env("ACCOUNT_CREDENTIALS_KEY", ""),
		PolymarketCLOBURL:   env("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"),
		PolymarketDataURL:   env("POLYMARKET_DATA_URL", "https://data-api.polymarket.com"),
		BinancePortfolioURL: env("BINANCE_PORTFOLIO_API_URL", "https://papi.binance.com"),
		OKXAccountURL:       env("OKX_ACCOUNT_API_URL", "https://www.okx.com"),
		BitgetAccountURL:    env("BITGET_ACCOUNT_API_URL", "https://api.bitget.com"),
		BybitAccountURL:     env("BYBIT_ACCOUNT_API_URL", "https://api.bybit.com"),
		GateAccountURL:      env("GATE_ACCOUNT_API_URL", "https://api.gateio.ws"),
		AccountSnapshotTTL:  duration("ACCOUNT_SNAPSHOT_TTL", 3*time.Second),
		HTTPTimeout:         duration("ACCOUNT_SNAPSHOT_HTTP_TIMEOUT", 8*time.Second),
		LogLevel:            env("LOG_LEVEL", "info"),
		Development:         boolean("DEVELOPMENT", false),
	}
	if c.TokenSecret == "" {
		return c, fmt.Errorf("ACCOUNT_TOKEN_SECRET is required")
	}
	if c.CredentialsKey == "" {
		return c, fmt.Errorf("ACCOUNT_CREDENTIALS_KEY is required")
	}
	if c.ReportInternalToken == "" {
		return c, fmt.Errorf("REPORT_INTERNAL_TOKEN is required")
	}
	if c.TraderInternalToken == "" {
		return c, fmt.Errorf("TRADER_INTERNAL_TOKEN is required")
	}
	if c.TokenTTL <= 0 || c.HTTPTimeout <= 0 || c.AccountSnapshotTTL <= 0 {
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
		SessionCookieName:     env("ACCOUNT_SESSION_COOKIE", "sq_session"),
		SessionCookieSecure:   boolean("ACCOUNT_COOKIE_SECURE", false),
		SessionCookieMaxAge:   duration("ACCOUNT_TOKEN_TTL", 12*time.Hour),
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
		DatabaseURL:                 env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:                 env("TRADER_GRPC_ADDRESS", ":9095"),
		AccountGRPCAddress:          env("ACCOUNT_GRPC_TARGET", "localhost:9091"),
		BinanceURL:                  env("BINANCE_PORTFOLIO_API_URL", "https://papi.binance.com"),
		OKXURL:                      env("OKX_ACCOUNT_API_URL", "https://www.okx.com"),
		BybitURL:                    env("BYBIT_ACCOUNT_API_URL", "https://api.bybit.com"),
		BitgetURL:                   env("BITGET_ACCOUNT_API_URL", "https://api.bitget.com"),
		GateURL:                     env("GATE_ACCOUNT_API_URL", "https://api.gateio.ws"),
		HTTPTimeout:                 duration("TRADER_HTTP_TIMEOUT", 12*time.Second),
		InternalToken:               strings.TrimSpace(os.Getenv("TRADER_INTERNAL_TOKEN")),
		ReconcileInterval:           duration("TRADER_RECONCILE_INTERVAL", time.Second),
		ReconcileBatchSize:          positiveInteger("TRADER_RECONCILE_BATCH_SIZE", 20),
		ReconcileWorkers:            positiveInteger("TRADER_RECONCILE_WORKERS", 3),
		TwapScheduleInterval:        duration("TRADER_TWAP_SCHEDULE_INTERVAL", 500*time.Millisecond),
		TwapScheduleLease:           duration("TRADER_TWAP_SCHEDULE_LEASE", 30*time.Second),
		TwapScheduleBatch:           positiveInteger("TRADER_TWAP_SCHEDULE_BATCH", 20),
		TwapScheduleWorkers:         positiveInteger("TRADER_TWAP_SCHEDULE_WORKERS", 4),
		TwapRetention:               duration("TRADER_TWAP_RETENTION", 7*24*time.Hour),
		TwapCleanupInterval:         duration("TRADER_TWAP_CLEANUP_INTERVAL", time.Hour),
		TwapCleanupBatch:            positiveInteger("TRADER_TWAP_CLEANUP_BATCH", 100),
		TwapCleanupMaxBatches:       positiveInteger("TRADER_TWAP_CLEANUP_MAX_BATCHES", 10),
		ArbitrageScheduleInterval:   duration("TRADER_ARBITRAGE_SCHEDULE_INTERVAL", 100*time.Millisecond),
		ArbitrageScheduleLease:      duration("TRADER_ARBITRAGE_SCHEDULE_LEASE", 30*time.Second),
		ArbitrageScheduleBatch:      positiveInteger("TRADER_ARBITRAGE_SCHEDULE_BATCH", 50),
		ArbitrageScheduleWorkers:    positiveInteger("TRADER_ARBITRAGE_SCHEDULE_WORKERS", 8),
		ArbitrageBBOStale:           duration("TRADER_ARBITRAGE_BBO_STALE", 60*time.Second),
		ArbitrageRepriceTicks:       positiveInteger("TRADER_ARBITRAGE_REPRICE_TICKS", 2),
		ArbitrageObserverInterval:   duration("TRADER_ARBITRAGE_OBSERVER_INTERVAL", 200*time.Millisecond),
		ArbitrageIOCProtectionTicks: positiveInteger("TRADER_ARBITRAGE_IOC_PROTECTION_TICKS", 3),
		ArbitrageIOCRetries:         positiveInteger("TRADER_ARBITRAGE_IOC_RETRIES", 3),
		ArbitrageOrderStreamEnabled: boolean("TRADER_ARBITRAGE_ORDER_STREAM_ENABLED", true),
		BinanceOrderWSURL:           strings.TrimSpace(os.Getenv("BINANCE_ORDER_WS_URL")),
		OKXOrderWSURL:               strings.TrimSpace(os.Getenv("OKX_ORDER_WS_URL")),
		BybitOrderWSURL:             strings.TrimSpace(os.Getenv("BYBIT_ORDER_WS_URL")),
		BitgetOrderWSURL:            strings.TrimSpace(os.Getenv("BITGET_ORDER_WS_URL")),
		GateOrderWSURL:              strings.TrimSpace(os.Getenv("GATE_ORDER_WS_URL")),
		OrderStreamReconnectInitial: duration("TRADER_ORDER_STREAM_RECONNECT_INITIAL", 250*time.Millisecond),
		OrderStreamReconnectMax:     duration("TRADER_ORDER_STREAM_RECONNECT_MAX", 30*time.Second),
		OrderStreamHeartbeat:        duration("TRADER_ORDER_STREAM_HEARTBEAT", 15*time.Second),
		OrderStreamStale:            duration("TRADER_ORDER_STREAM_STALE", 45*time.Second),
		OrderStreamListenKeyRefresh: duration("TRADER_ORDER_STREAM_LISTEN_KEY_REFRESH", 30*time.Minute),
		OrderStreamRESTAudit:        duration("TRADER_ORDER_STREAM_REST_AUDIT", 30*time.Second),
		OrderStreamSessionIdle:      duration("TRADER_ORDER_STREAM_SESSION_IDLE", 5*time.Minute),
		ArbitrageMaxActiveAccount:   positiveInteger("TRADER_ARBITRAGE_MAX_ACTIVE_PER_ACCOUNT", 4),
		ArbitrageMaxActiveVenue:     positiveInteger("TRADER_ARBITRAGE_MAX_ACTIVE_PER_VENUE", 16),
		ArbitrageRetention:          duration("TRADER_ARBITRAGE_RETENTION", 7*24*time.Hour),
		ArbitrageCleanupInterval:    duration("TRADER_ARBITRAGE_CLEANUP_INTERVAL", time.Hour),
		ArbitrageCleanupBatch:       positiveInteger("TRADER_ARBITRAGE_CLEANUP_BATCH", 100),
		ArbitrageCleanupMaxBatches:  positiveInteger("TRADER_ARBITRAGE_CLEANUP_MAX_BATCHES", 10),
		ArbitrageDryRun:             boolean("TRADER_ARBITRAGE_DRY_RUN", true),
		LogLevel:                    env("LOG_LEVEL", "info"),
		Development:                 boolean("DEVELOPMENT", false),
	}
	if c.HTTPTimeout <= 0 || c.ReconcileInterval <= 0 ||
		c.ReconcileBatchSize <= 0 || c.ReconcileWorkers <= 0 ||
		c.TwapScheduleInterval <= 0 || c.TwapScheduleLease <= 0 ||
		c.TwapScheduleBatch <= 0 || c.TwapScheduleWorkers <= 0 ||
		c.TwapRetention <= 0 || c.TwapCleanupInterval <= 0 ||
		c.TwapCleanupBatch <= 0 || c.TwapCleanupMaxBatches <= 0 ||
		c.ArbitrageScheduleInterval <= 0 || c.ArbitrageScheduleLease <= 0 ||
		c.ArbitrageScheduleBatch <= 0 || c.ArbitrageScheduleWorkers <= 0 ||
		c.ArbitrageBBOStale <= 0 || c.ArbitrageRepriceTicks <= 0 ||
		c.ArbitrageObserverInterval <= 0 || c.ArbitrageIOCProtectionTicks <= 0 ||
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
	if c.InternalToken == "" {
		return c, fmt.Errorf("TRADER_INTERNAL_TOKEN is required")
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

func AggDataFromEnv() (AggData, error) {
	c := AggData{
		HTTPAddress:               env("AGGDATA_HTTP_ADDRESS", "127.0.0.1:9093"),
		RecordingDir:              env("AGGDATA_RECORDING_DIR", "/tmp/marketdata/crypto"),
		MDSGatewayURL:             env("AGGDATA_MDS_GATEWAY_URL", "ws://127.0.0.1:9443/v1/market-data"),
		MDSGatewayToken:           strings.TrimSpace(os.Getenv("MDS_GATEWAY_TOKEN")),
		BrowserToken:              strings.TrimSpace(os.Getenv("AGGDATA_BROWSER_TOKEN")),
		MaxClients:                positiveInteger("AGGDATA_MAX_CLIENTS", 128),
		MaxSubscriptions:          positiveInteger("AGGDATA_MAX_SUBSCRIPTIONS_PER_CLIENT", 16),
		ManifestRefresh:           duration("AGGDATA_MANIFEST_REFRESH_INTERVAL", time.Second),
		StreamInterval:            duration("AGGDATA_STREAM_INTERVAL", 50*time.Millisecond),
		HistoryWorkers:            positiveInteger("AGGDATA_HISTORY_WORKERS", 1),
		HistoryResolution:         duration("AGGDATA_HISTORY_RESOLUTION", time.Minute),
		DatabaseURL:               env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		FairPriceEnabled:          boolean("AGGDATA_FAIRPRICE_ENABLED", true),
		FairPriceDepth:            positiveInteger("AGGDATA_FAIRPRICE_DEPTH", 20),
		FairPriceLambdaPerBP:      number("AGGDATA_FAIRPRICE_LAMBDA_PER_BP", 0.10),
		FairPriceImbalanceAlpha:   number("AGGDATA_FAIRPRICE_IMBALANCE_ALPHA", 1),
		FairPriceEWMATau:          duration("AGGDATA_FAIRPRICE_EWMA_TAU", 500*time.Millisecond),
		FairPriceImpactNotional:   number("AGGDATA_FAIRPRICE_IMPACT_NOTIONAL", 0),
		FairPriceVenueDominance:   number("AGGDATA_FAIRPRICE_VENUE_DOMINANCE_RATIO", 0.90),
		FairPriceStreamInterval:   duration("AGGDATA_FAIRPRICE_STREAM_INTERVAL", 5*time.Millisecond),
		FairPriceHeartbeat:        duration("AGGDATA_FAIRPRICE_HEARTBEAT", time.Second),
		FairPricePersistEnabled:   boolean("AGGDATA_FAIRPRICE_PERSIST_ENABLED", true),
		FairPriceSampleInterval:   duration("AGGDATA_FAIRPRICE_SAMPLE_INTERVAL", time.Second),
		FairPriceRetention:        duration("AGGDATA_FAIRPRICE_RETENTION", 24*time.Hour),
		FairPriceCleanupInterval:  duration("AGGDATA_FAIRPRICE_CLEANUP_INTERVAL", 5*time.Minute),
		FairPriceDeleteBatch:      positiveInteger("AGGDATA_FAIRPRICE_DELETE_BATCH", 10_000),
		FairPriceDeleteMaxBatches: positiveInteger("AGGDATA_FAIRPRICE_DELETE_MAX_BATCHES", 20),
		FairPriceDBTimeout:        duration("AGGDATA_FAIRPRICE_DB_TIMEOUT", 5*time.Second),
		Development:               boolean("DEVELOPMENT", false),
		LogLevel:                  env("LOG_LEVEL", "info"),
	}
	for _, origin := range strings.Split(env("AGGDATA_ALLOWED_ORIGINS", "http://localhost:3000"), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
				parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
				parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
				return c, fmt.Errorf("invalid AGGDATA_ALLOWED_ORIGINS entry %q", origin)
			}
			origin = strings.TrimSuffix(origin, "/")
			c.AllowedOrigins = append(c.AllowedOrigins, origin)
		}
	}
	if strings.TrimSpace(c.RecordingDir) == "" {
		return c, fmt.Errorf("AGGDATA_RECORDING_DIR is required")
	}
	gatewayURL, err := url.Parse(c.MDSGatewayURL)
	if err != nil || (gatewayURL.Scheme != "ws" && gatewayURL.Scheme != "wss") ||
		gatewayURL.Host == "" || gatewayURL.User != nil || gatewayURL.Fragment != "" {
		return c, fmt.Errorf("AGGDATA_MDS_GATEWAY_URL must use ws or wss")
	}
	if c.MDSGatewayToken == "" {
		return c, fmt.Errorf("MDS_GATEWAY_TOKEN is required")
	}
	if c.MaxClients <= 0 || c.MaxSubscriptions <= 0 || c.HistoryWorkers <= 0 ||
		c.ManifestRefresh <= 0 || c.StreamInterval < 10*time.Millisecond ||
		c.HistoryResolution <= 0 {
		return c, fmt.Errorf("aggdata limits and durations must be positive")
	}
	if c.FairPriceDepth <= 0 || c.FairPriceDepth > 50 ||
		c.FairPriceLambdaPerBP < 0 || !finiteNumber(c.FairPriceLambdaPerBP) ||
		c.FairPriceImbalanceAlpha < 0 || c.FairPriceImbalanceAlpha > 1 ||
		!finiteNumber(c.FairPriceImbalanceAlpha) ||
		c.FairPriceEWMATau <= 0 || c.FairPriceImpactNotional < 0 ||
		!finiteNumber(c.FairPriceImpactNotional) ||
		c.FairPriceVenueDominance <= 0 || c.FairPriceVenueDominance > 1 ||
		!finiteNumber(c.FairPriceVenueDominance) ||
		c.FairPriceStreamInterval < time.Millisecond || c.FairPriceHeartbeat <= 0 {
		return c, fmt.Errorf("invalid aggdata fair price algorithm or stream configuration")
	}
	if c.FairPricePersistEnabled {
		if strings.TrimSpace(c.DatabaseURL) == "" {
			return c, fmt.Errorf("DATABASE_URL is required when fair price persistence is enabled")
		}
		if c.FairPriceSampleInterval <= 0 || c.FairPriceRetention <= 0 ||
			c.FairPriceCleanupInterval <= 0 || c.FairPriceDBTimeout <= 0 ||
			c.FairPriceDeleteBatch <= 0 || c.FairPriceDeleteMaxBatches <= 0 {
			return c, fmt.Errorf("invalid aggdata fair price persistence configuration")
		}
	}
	if len(c.AllowedOrigins) == 0 {
		return c, fmt.Errorf("AGGDATA_ALLOWED_ORIGINS must contain at least one origin")
	}
	for _, key := range []string{
		"AGGDATA_MAX_CLIENTS", "AGGDATA_MAX_SUBSCRIPTIONS_PER_CLIENT",
		"AGGDATA_HISTORY_WORKERS", "AGGDATA_FAIRPRICE_DEPTH",
		"AGGDATA_FAIRPRICE_DELETE_BATCH", "AGGDATA_FAIRPRICE_DELETE_MAX_BATCHES",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed <= 0 {
				return c, fmt.Errorf("%s must be a positive integer", key)
			}
		}
	}
	for _, key := range []string{
		"AGGDATA_MANIFEST_REFRESH_INTERVAL", "AGGDATA_HISTORY_RESOLUTION",
		"AGGDATA_FAIRPRICE_EWMA_TAU", "AGGDATA_FAIRPRICE_STREAM_INTERVAL",
		"AGGDATA_FAIRPRICE_HEARTBEAT", "AGGDATA_FAIRPRICE_SAMPLE_INTERVAL",
		"AGGDATA_FAIRPRICE_RETENTION", "AGGDATA_FAIRPRICE_CLEANUP_INTERVAL",
		"AGGDATA_FAIRPRICE_DB_TIMEOUT",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				if seconds, secondsErr := strconv.Atoi(value); secondsErr == nil {
					parsed = time.Duration(seconds) * time.Second
				} else {
					return c, fmt.Errorf("%s must be a duration", key)
				}
			}
			if parsed <= 0 {
				return c, fmt.Errorf("%s must be positive", key)
			}
		}
	}
	for _, key := range []string{
		"AGGDATA_FAIRPRICE_LAMBDA_PER_BP", "AGGDATA_FAIRPRICE_IMBALANCE_ALPHA",
		"AGGDATA_FAIRPRICE_IMPACT_NOTIONAL", "AGGDATA_FAIRPRICE_VENUE_DOMINANCE_RATIO",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || !finiteNumber(parsed) {
				return c, fmt.Errorf("%s must be a finite number", key)
			}
		}
	}
	for _, key := range []string{
		"AGGDATA_FAIRPRICE_ENABLED", "AGGDATA_FAIRPRICE_PERSIST_ENABLED",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return c, fmt.Errorf("%s must be a boolean", key)
			}
		}
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

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
