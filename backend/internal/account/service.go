package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/polymarket"
)

type accountLookup interface {
	GetByUsername(context.Context, string) (Account, error)
}

type tokenIssuer interface {
	Issue(username, permission string, authVersion int64) (string, error)
	Validate(token string) (TokenClaims, error)
}

type cachedAccountStatus struct {
	account Account
	expires time.Time
}

type Service struct {
	accounts         accountLookup
	tokens           tokenIssuer
	trading          tradingStore
	cipher           credentialProtector
	polymarket       polymarketCredentialStore
	polymarketIssuer polymarketCredentialIssuer
	portfolios       *portfolio.Registry
	polyData         *polymarket.DataClient
	polyCLOB         *polymarket.CLOBClient
	instruments      instrumentCatalog
	snapshotTTL      time.Duration
	snapshotMu       sync.RWMutex
	snapshotCache    map[string]snapshotCacheEntry
	snapshotGroup    singleflight.Group
	internalToken    string
	traderToken      string
	gatewayToken     string
	adminToken       string
	tradeFills       tradeFillStore
	aiCredentials    aiCredentialStore
	readinessHTTP    *http.Client
	readinessURLs    map[string]string
	fees             *FeeSync
	statusCacheTTL   time.Duration
	statusCacheMu    sync.RWMutex
	statusCache      map[string]cachedAccountStatus
}

func NewService(accounts accountLookup, tokens tokenIssuer) *Service {
	return &Service{
		accounts:       accounts,
		tokens:         tokens,
		statusCacheTTL: 30 * time.Second,
		statusCache:    make(map[string]cachedAccountStatus),
	}
}

func (s *Service) WithStatusCache(ttl time.Duration) *Service {
	if ttl > 0 {
		s.statusCacheTTL = ttl
	}
	return s
}

func (s *Service) WithInternalGateway(token string) *Service {
	s.gatewayToken = token
	return s
}

func (s *Service) WithInternalAdmin(token string) *Service {
	s.adminToken = token
	return s
}

func (s *Service) WithTrading(trading tradingStore, cipher credentialProtector) *Service {
	s.trading = trading
	s.cipher = cipher
	return s
}

func (s *Service) WithPolymarket(
	store polymarketCredentialStore,
	issuer polymarketCredentialIssuer,
) *Service {
	s.polymarket = store
	s.polymarketIssuer = issuer
	return s
}

func (s *Service) WithInstrumentCatalog(catalog instrumentCatalog) *Service {
	s.instruments = catalog
	return s
}

func (s *Service) WithInternalReports(token string, fills tradeFillStore) *Service {
	s.internalToken = token
	s.tradeFills = fills
	return s
}

func (s *Service) WithInternalTrader(token string) *Service {
	s.traderToken = token
	return s
}

func (s *Service) WithReadiness(client *http.Client, urls map[string]string) *Service {
	s.readinessHTTP = client
	s.readinessURLs = urls
	return s
}

func (s *Service) WithFeeSync(fees *FeeSync) *Service {
	s.fees = fees
	return s
}

type Session struct {
	Token      string
	Username   string
	Permission string
}

func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	account, err := s.lookupAccountStatus(ctx, username)
	if errors.Is(err, ErrNotFound) {
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup account: %w", err)
	}
	if !accountUsable(account) {
		return Session{}, ErrInvalidCredentials
	}
	if err := CheckPassword(account.PasswordHash, password); err != nil {
		return Session{}, err
	}
	token, err := s.tokens.Issue(account.Username, account.Permission, account.AuthVersion)
	if err != nil {
		return Session{}, fmt.Errorf("issue session token: %w", err)
	}
	return Session{
		Token: token, Username: account.Username, Permission: account.Permission,
	}, nil
}

func (s *Service) ValidateSession(ctx context.Context, token string) (Session, error) {
	claims, err := s.tokens.Validate(token)
	if err != nil {
		return Session{}, err
	}
	account, err := s.lookupAccountStatus(ctx, claims.Username)
	if err != nil {
		return Session{}, ErrInvalidToken
	}
	if !accountUsable(account) {
		return Session{}, ErrInvalidToken
	}
	if NormalizedAuthVersion(claims.AuthVersion) != account.AuthVersion {
		return Session{}, ErrInvalidToken
	}
	return Session{
		Username: account.Username, Permission: account.Permission,
	}, nil
}

func accountUsable(account Account) bool {
	status := account.Status
	if status == "" {
		status = AccountStatusActive
	}
	return status == AccountStatusActive && account.DeletedAt == nil
}

func (s *Service) lookupAccountStatus(ctx context.Context, username string) (Account, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return Account{}, ErrNotFound
	}
	now := time.Now()
	s.statusCacheMu.RLock()
	if entry, ok := s.statusCache[username]; ok && now.Before(entry.expires) {
		account := entry.account
		s.statusCacheMu.RUnlock()
		return account, nil
	}
	s.statusCacheMu.RUnlock()

	account, err := s.accounts.GetByUsername(ctx, username)
	if err != nil {
		return Account{}, err
	}
	account.AuthVersion = NormalizedAuthVersion(account.AuthVersion)
	if account.Status == "" {
		account.Status = AccountStatusActive
	}
	ttl := s.statusCacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	s.statusCacheMu.Lock()
	if s.statusCache == nil {
		s.statusCache = make(map[string]cachedAccountStatus)
	}
	s.statusCache[username] = cachedAccountStatus{account: account, expires: now.Add(ttl)}
	s.statusCacheMu.Unlock()
	return account, nil
}

func (s *Service) invalidateAccountStatus(username string) {
	username = strings.TrimSpace(username)
	if username == "" {
		return
	}
	s.statusCacheMu.Lock()
	delete(s.statusCache, username)
	s.statusCacheMu.Unlock()
}
