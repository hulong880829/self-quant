package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidTradingAccount = errors.New("invalid trading account")
	ErrUnsupportedExchange   = errors.New("unsupported exchange")
	ErrPassphraseRequired    = errors.New("passphrase is required")
)

var cexTradingExchanges = map[string]struct{}{
	"binance": {}, "okx": {}, "bybit": {}, "bitget": {}, "gate": {},
}

var passphraseRequiredExchanges = map[string]struct{}{
	"okx": {}, "bitget": {},
}

type TradingCredentials struct {
	TradingAccountID int64
	ProductName      string
	Exchange         string
	AccountName      string
	APIKey           string
	APISecret        string
	Passphrase       string
}

var supportedExchanges = map[string]struct{}{
	"binance": {}, "okx": {}, "bybit": {}, "bitget": {}, "gate": {}, "hyperliquid": {},
	"polymarket": {},
}

type tradingStore interface {
	ListByOwner(context.Context, string) ([]TradingAccountRecord, error)
	GetByOwner(context.Context, string, int64) (TradingAccountRecord, error)
	ListByOwnerProduct(context.Context, string, string) ([]TradingAccountRecord, error)
	Create(context.Context, TradingAccountRecord) (TradingAccountRecord, error)
	DeleteByOwner(context.Context, string, int64) error
}

type credentialProtector interface {
	Encrypt(string) ([]byte, error)
	Decrypt([]byte) (string, error)
}

type TradingAccountView struct {
	ID            int64
	ProductName   string
	Exchange      string
	AccountName   string
	APIKeyMasked  string
	HasPassphrase bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	WalletAddress string
	WalletType    string
	BindingStatus string
}

type CreateTradingAccountInput struct {
	ProductName string
	Exchange    string
	AccountName string
	APIKey      string
	APISecret   string
	Passphrase  string
}

func (s *Service) ListTradingAccounts(ctx context.Context, token string) ([]TradingAccountView, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return nil, err
	}
	if s.trading == nil || s.cipher == nil {
		return nil, fmt.Errorf("trading accounts not configured")
	}
	records, err := s.trading.ListByOwner(ctx, session.Username)
	if err != nil {
		return nil, err
	}
	metadata := make(map[int64]PolymarketCredentialRecord)
	if s.polymarket != nil {
		items, metadataErr := s.polymarket.ListMetadataByOwner(ctx, session.Username)
		if metadataErr != nil {
			return nil, metadataErr
		}
		for _, item := range items {
			metadata[item.TradingAccountID] = item
		}
	}
	views := make([]TradingAccountView, 0, len(records))
	for _, record := range records {
		view, err := s.toTradingAccountView(record)
		if err != nil {
			return nil, err
		}
		if item, ok := metadata[record.ID]; ok {
			view.WalletAddress = item.FunderAddress
			view.WalletType = item.WalletType
			view.BindingStatus = item.BindingStatus
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Service) CreateTradingAccount(
	ctx context.Context,
	token string,
	input CreateTradingAccountInput,
) (TradingAccountView, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingAccountView{}, err
	}
	if s.trading == nil || s.cipher == nil {
		return TradingAccountView{}, fmt.Errorf("trading accounts not configured")
	}
	normalized, err := normalizeCreateInput(input)
	if err != nil {
		return TradingAccountView{}, err
	}
	apiKeyEnc, err := s.cipher.Encrypt(normalized.APIKey)
	if err != nil {
		return TradingAccountView{}, fmt.Errorf("encrypt api key: %w", err)
	}
	apiSecretEnc, err := s.cipher.Encrypt(normalized.APISecret)
	if err != nil {
		return TradingAccountView{}, fmt.Errorf("encrypt api secret: %w", err)
	}
	var passphraseEnc []byte
	if normalized.Passphrase != "" {
		passphraseEnc, err = s.cipher.Encrypt(normalized.Passphrase)
		if err != nil {
			return TradingAccountView{}, fmt.Errorf("encrypt passphrase: %w", err)
		}
	}
	created, err := s.trading.Create(ctx, TradingAccountRecord{
		OwnerUsername: session.Username,
		ProductName:   normalized.ProductName,
		Exchange:      normalized.Exchange,
		AccountName:   normalized.AccountName,
		APIKeyEnc:     apiKeyEnc,
		APISecretEnc:  apiSecretEnc,
		PassphraseEnc: passphraseEnc,
	})
	if err != nil {
		return TradingAccountView{}, err
	}
	return s.toTradingAccountView(created)
}

func (s *Service) GetTradingCredentials(
	ctx context.Context,
	token string,
	id int64,
) (TradingCredentials, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingCredentials{}, err
	}
	if s.trading == nil || s.cipher == nil {
		return TradingCredentials{}, fmt.Errorf("trading accounts not configured")
	}
	if id <= 0 {
		return TradingCredentials{}, ErrInvalidTradingAccount
	}
	record, err := s.trading.GetByOwner(ctx, session.Username, id)
	if err != nil {
		return TradingCredentials{}, err
	}
	return s.decryptTradingCredentials(record)
}

func (s *Service) GetTradingCredentialsInternal(
	ctx context.Context,
	serviceToken string,
	owner string,
	id int64,
) (TradingCredentials, error) {
	expected := []byte(strings.TrimSpace(s.traderToken))
	provided := []byte(strings.TrimSpace(serviceToken))
	if len(expected) == 0 || len(expected) != len(provided) ||
		subtle.ConstantTimeCompare(expected, provided) != 1 {
		return TradingCredentials{}, ErrInvalidServiceToken
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || id <= 0 || s.trading == nil || s.cipher == nil {
		return TradingCredentials{}, ErrInvalidTradingAccount
	}
	record, err := s.trading.GetByOwner(ctx, owner, id)
	if err != nil {
		return TradingCredentials{}, err
	}
	return s.decryptTradingCredentials(record)
}

func (s *Service) decryptTradingCredentials(record TradingAccountRecord) (TradingCredentials, error) {
	if _, ok := cexTradingExchanges[record.Exchange]; !ok {
		return TradingCredentials{}, ErrUnsupportedExchange
	}
	apiKey, err := s.cipher.Decrypt(record.APIKeyEnc)
	if err != nil {
		return TradingCredentials{}, fmt.Errorf("decrypt api key: %w", err)
	}
	apiSecret, err := s.cipher.Decrypt(record.APISecretEnc)
	if err != nil {
		return TradingCredentials{}, fmt.Errorf("decrypt api secret: %w", err)
	}
	passphrase := ""
	if len(record.PassphraseEnc) > 0 {
		passphrase, err = s.cipher.Decrypt(record.PassphraseEnc)
		if err != nil {
			return TradingCredentials{}, fmt.Errorf("decrypt passphrase: %w", err)
		}
	}
	if _, required := passphraseRequiredExchanges[record.Exchange]; required && passphrase == "" {
		return TradingCredentials{}, ErrPassphraseRequired
	}
	return TradingCredentials{
		TradingAccountID: record.ID,
		ProductName:      record.ProductName,
		Exchange:         record.Exchange,
		AccountName:      record.AccountName,
		APIKey:           apiKey,
		APISecret:        apiSecret,
		Passphrase:       passphrase,
	}, nil
}

func (s *Service) DeleteTradingAccount(ctx context.Context, token string, id int64) error {
	session, err := s.ValidateSession(token)
	if err != nil {
		return err
	}
	if s.trading == nil {
		return fmt.Errorf("trading accounts not configured")
	}
	if id <= 0 {
		return ErrInvalidTradingAccount
	}
	return s.trading.DeleteByOwner(ctx, session.Username, id)
}

func (s *Service) toTradingAccountView(record TradingAccountRecord) (TradingAccountView, error) {
	apiKey, err := s.cipher.Decrypt(record.APIKeyEnc)
	if err != nil {
		return TradingAccountView{}, fmt.Errorf("decrypt api key: %w", err)
	}
	return TradingAccountView{
		ID:            record.ID,
		ProductName:   record.ProductName,
		Exchange:      record.Exchange,
		AccountName:   record.AccountName,
		APIKeyMasked:  MaskAPIKey(apiKey),
		HasPassphrase: len(record.PassphraseEnc) > 0,
		CreatedAt:     record.CreatedAt.UTC(),
		UpdatedAt:     record.UpdatedAt.UTC(),
	}, nil
}

func normalizeCreateInput(input CreateTradingAccountInput) (CreateTradingAccountInput, error) {
	productName := strings.TrimSpace(input.ProductName)
	accountName := strings.TrimSpace(input.AccountName)
	exchange := strings.ToLower(strings.TrimSpace(input.Exchange))
	apiKey := strings.TrimSpace(input.APIKey)
	apiSecret := strings.TrimSpace(input.APISecret)
	passphrase := strings.TrimSpace(input.Passphrase)

	if productName == "" || accountName == "" || apiKey == "" || apiSecret == "" {
		return CreateTradingAccountInput{}, ErrInvalidTradingAccount
	}
	if utf8.RuneCountInString(productName) > 64 || utf8.RuneCountInString(accountName) > 64 {
		return CreateTradingAccountInput{}, ErrInvalidTradingAccount
	}
	if utf8.RuneCountInString(apiKey) > 256 || utf8.RuneCountInString(apiSecret) > 256 ||
		utf8.RuneCountInString(passphrase) > 256 {
		return CreateTradingAccountInput{}, ErrInvalidTradingAccount
	}
	if _, ok := supportedExchanges[exchange]; !ok {
		return CreateTradingAccountInput{}, ErrUnsupportedExchange
	}
	if _, required := passphraseRequiredExchanges[exchange]; required && passphrase == "" {
		return CreateTradingAccountInput{}, ErrPassphraseRequired
	}
	return CreateTradingAccountInput{
		ProductName: productName,
		Exchange:    exchange,
		AccountName: accountName,
		APIKey:      apiKey,
		APISecret:   apiSecret,
		Passphrase:  passphrase,
	}, nil
}
