package account

import (
	"context"
	"errors"
	"fmt"
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
	Issue(username, permission string) (string, error)
	Validate(token string) (TokenClaims, error)
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
	tradeFills       tradeFillStore
	aiCredentials    aiCredentialStore
}

func NewService(accounts accountLookup, tokens tokenIssuer) *Service {
	return &Service{accounts: accounts, tokens: tokens}
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

type Session struct {
	Token      string
	Username   string
	Permission string
}

func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	account, err := s.accounts.GetByUsername(ctx, username)
	if errors.Is(err, ErrNotFound) {
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup account: %w", err)
	}
	if err := CheckPassword(account.PasswordHash, password); err != nil {
		return Session{}, err
	}
	token, err := s.tokens.Issue(account.Username, account.Permission)
	if err != nil {
		return Session{}, fmt.Errorf("issue session token: %w", err)
	}
	return Session{
		Token: token, Username: account.Username, Permission: account.Permission,
	}, nil
}

func (s *Service) ValidateSession(token string) (Session, error) {
	claims, err := s.tokens.Validate(token)
	if err != nil {
		return Session{}, err
	}
	return Session{
		Username: claims.Username, Permission: claims.Permission,
	}, nil
}
