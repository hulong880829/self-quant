package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"selfquant/backend/internal/account/portfolio"
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

const (
	TradingStatusChecking              = "checking"
	TradingStatusReady                 = "ready"
	TradingStatusWalletUnauthorized    = "wallet_unauthorized"
	TradingStatusPrivateKeyMismatch    = "private_key_mismatch"
	TradingStatusAccountNotFound       = "account_not_found"
	TradingStatusAPIWalletNotFound     = "api_wallet_not_found"
	TradingStatusInstrumentUnavailable = "instrument_unavailable"
	TradingStatusVenueUnavailable      = "venue_unavailable"
	TradingStatusUnsupportedAuthMode   = "unsupported_auth_mode"

	CredentialKindAsterHMAC         = "aster_hmac"
	CredentialKindAsterAPIWallet    = "aster_api_wallet"
	CredentialKindHyperliquidAgent  = "hyperliquid_agent"
	CredentialKindHyperliquidWallet = "hyperliquid_api_wallet"
	CredentialKindLighterAPI        = "lighter_api"
	CredentialKindLighterAPIWallet  = "lighter_api_wallet"
)

type TradingCredentials struct {
	TradingAccountID int64
	ProductName      string
	Exchange         string
	AccountName      string
	APIKey           string
	APISecret        string
	Passphrase       string
	CredentialKind   string
	SigningAddress   string
	VaultAddress     string
	AccountIndex     *int64
	APIKeyIndex      *int32
}

type TradingAccountMeta struct {
	ID             int64
	ProductName    string
	Exchange       string
	AccountName    string
	CredentialKind string
	AccountIndex   *int64
	APIKeyIndex    *int32
}

type TradingReadiness struct {
	TradingAccountID         int64
	Exchange                 string
	CredentialsPresent       bool
	CredentialsVerified      bool
	TradingMode              string
	TradingReady             bool
	TradingStatus            string
	TradingUnavailableCode   string
	TradingUnavailableReason string
	ResolvedAccountIndex     *int64
	ResolvedAPIKeyIndex      *int32
}

var supportedExchanges = map[string]struct{}{
	"binance": {}, "okx": {}, "bybit": {}, "bitget": {}, "gate": {},
	"hyperliquid": {}, "aster": {}, "lighter": {},
	"polymarket": {},
}

var walletDEXExchanges = map[string]struct{}{
	"hyperliquid": {}, "aster": {}, "lighter": {},
}

type tradingStore interface {
	ListByOwner(context.Context, string) ([]TradingAccountRecord, error)
	GetByOwner(context.Context, string, int64) (TradingAccountRecord, error)
	ListByOwnerProduct(context.Context, string, string) ([]TradingAccountRecord, error)
	Create(context.Context, TradingAccountRecord) (TradingAccountRecord, error)
	UpdateIndexes(context.Context, string, int64, *int64, *int16) error
	DeleteByOwner(context.Context, string, int64) error
}

type credentialProtector interface {
	Encrypt(string) ([]byte, error)
	Decrypt([]byte) (string, error)
}

type TradingAccountView struct {
	ID                       int64
	ProductName              string
	Exchange                 string
	AccountName              string
	APIKeyMasked             string
	HasPassphrase            bool
	CreatedAt                time.Time
	UpdatedAt                time.Time
	WalletAddress            string
	WalletType               string
	BindingStatus            string
	CredentialsPresent       bool
	CredentialsVerified      bool
	TradingMode              string
	TradingReady             bool
	TradingStatus            string
	TradingUnavailableCode   string
	TradingUnavailableReason string
	ResolvedAccountIndex     *int64
	ResolvedAPIKeyIndex      *int32
	Fees                     TradingAccountFees
}

type CreateTradingAccountInput struct {
	ProductName      string
	Exchange         string
	AccountName      string
	APIKey           string
	APISecret        string
	Passphrase       string
	TradingAPIKey    string
	TradingAPISecret string
	SigningAddress   string
	VaultAddress     string
	AccountIndex     *int64
	APIKeyIndex      *int32
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
		view := s.toTradingAccountView(record)
		if item, ok := metadata[record.ID]; ok {
			view.WalletAddress = item.FunderAddress
			view.WalletType = item.WalletType
			view.BindingStatus = item.BindingStatus
		}
		s.attachFeeView(ctx, &view)
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
	tradingAPIKeyEnc, err := encryptOptional(s.cipher, normalized.TradingAPIKey)
	if err != nil {
		return TradingAccountView{}, fmt.Errorf("encrypt trading api key: %w", err)
	}
	tradingAPISecretEnc, err := encryptOptional(s.cipher, normalized.TradingAPISecret)
	if err != nil {
		return TradingAccountView{}, fmt.Errorf("encrypt trading api secret: %w", err)
	}
	var accountIndex *int64
	var apiKeyIndex *int16
	if normalized.AccountIndex != nil {
		value := *normalized.AccountIndex
		accountIndex = &value
	}
	if normalized.APIKeyIndex != nil {
		value := int16(*normalized.APIKeyIndex)
		apiKeyIndex = &value
	}
	created, err := s.trading.Create(ctx, TradingAccountRecord{
		OwnerUsername:       session.Username,
		ProductName:         normalized.ProductName,
		Exchange:            normalized.Exchange,
		AccountName:         normalized.AccountName,
		APIKeyEnc:           apiKeyEnc,
		APISecretEnc:        apiSecretEnc,
		PassphraseEnc:       passphraseEnc,
		CredentialKind:      credentialKindForCreate(normalized),
		TradingAPIKeyEnc:    tradingAPIKeyEnc,
		TradingAPISecretEnc: tradingAPISecretEnc,
		SigningAddress:      normalized.SigningAddress,
		VaultAddress:        normalized.VaultAddress,
		AccountIndex:        accountIndex,
		APIKeyIndex:         apiKeyIndex,
	})
	if err != nil {
		return TradingAccountView{}, err
	}
	return s.afterTradingAccountBound(ctx, created), nil
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

func (s *Service) GetTradingAccountMeta(
	ctx context.Context,
	token string,
	id int64,
) (TradingAccountMeta, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingAccountMeta{}, err
	}
	return s.tradingAccountMeta(ctx, session.Username, id)
}

func (s *Service) tradingAccountMeta(
	ctx context.Context,
	owner string,
	id int64,
) (TradingAccountMeta, error) {
	if s.trading == nil {
		return TradingAccountMeta{}, fmt.Errorf("trading accounts not configured")
	}
	if id <= 0 {
		return TradingAccountMeta{}, ErrInvalidTradingAccount
	}
	record, err := s.trading.GetByOwner(ctx, owner, id)
	if err != nil {
		return TradingAccountMeta{}, err
	}
	_, cex := cexTradingExchanges[record.Exchange]
	_, walletDEX := walletDEXExchanges[record.Exchange]
	if !cex && !walletDEX {
		return TradingAccountMeta{}, ErrUnsupportedExchange
	}
	meta := TradingAccountMeta{
		ID:             record.ID,
		ProductName:    record.ProductName,
		Exchange:       record.Exchange,
		AccountName:    record.AccountName,
		CredentialKind: resolvedCredentialKind(record),
		AccountIndex:   record.AccountIndex,
	}
	if record.APIKeyIndex != nil {
		value := int32(*record.APIKeyIndex)
		meta.APIKeyIndex = &value
	}
	return meta, nil
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
	_, cex := cexTradingExchanges[record.Exchange]
	_, walletDEX := walletDEXExchanges[record.Exchange]
	if !cex && !walletDEX {
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
	kind := resolvedCredentialKind(record)
	signingAddress := strings.TrimSpace(record.SigningAddress)
	vaultAddress := strings.TrimSpace(record.VaultAddress)
	if walletDEX {
		switch kind {
		case CredentialKindAsterAPIWallet, CredentialKindHyperliquidWallet, CredentialKindLighterAPIWallet:
			if err := validateAPIWalletCredentials(record.Exchange, apiKey, apiSecret); err != nil {
				return TradingCredentials{}, err
			}
			if signingAddress == "" {
				signingAddress = apiKey
			}
		case CredentialKindAsterHMAC, CredentialKindHyperliquidAgent, CredentialKindLighterAPI:
			tradingAPIKey, decryptErr := decryptOptional(s.cipher, record.TradingAPIKeyEnc)
			if decryptErr != nil {
				return TradingCredentials{}, fmt.Errorf("decrypt trading api key: %w", decryptErr)
			}
			tradingAPISecret, decryptErr := decryptOptional(s.cipher, record.TradingAPISecretEnc)
			if decryptErr != nil {
				return TradingCredentials{}, fmt.Errorf("decrypt trading api secret: %w", decryptErr)
			}
			if err := validateExtensionCredentialGroup(kind, tradingAPIKey, tradingAPISecret, signingAddress, record.AccountIndex, record.APIKeyIndex); err != nil {
				return TradingCredentials{}, err
			}
			switch kind {
			case CredentialKindAsterHMAC:
				apiKey, apiSecret = tradingAPIKey, tradingAPISecret
			default:
				apiSecret = tradingAPISecret
			}
		default:
			return TradingCredentials{}, ErrInvalidTradingAccount
		}
	}
	var accountIndex *int64
	var apiKeyIndex *int32
	if record.AccountIndex != nil {
		value := *record.AccountIndex
		accountIndex = &value
	}
	if record.APIKeyIndex != nil {
		value := int32(*record.APIKeyIndex)
		apiKeyIndex = &value
	}
	return TradingCredentials{
		TradingAccountID: record.ID,
		ProductName:      record.ProductName,
		Exchange:         record.Exchange,
		AccountName:      record.AccountName,
		APIKey:           apiKey,
		APISecret:        apiSecret,
		Passphrase:       passphrase,
		CredentialKind:   kind,
		SigningAddress:   signingAddress,
		VaultAddress:     vaultAddress,
		AccountIndex:     accountIndex,
		APIKeyIndex:      apiKeyIndex,
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
	if err := s.trading.DeleteByOwner(ctx, session.Username, id); err != nil {
		return err
	}
	if s.fees != nil {
		s.fees.Drop(id)
	}
	return nil
}

func (s *Service) toTradingAccountView(record TradingAccountRecord) TradingAccountView {
	view := TradingAccountView{
		ID:            record.ID,
		ProductName:   record.ProductName,
		Exchange:      record.Exchange,
		AccountName:   record.AccountName,
		HasPassphrase: len(record.PassphraseEnc) > 0,
		CreatedAt:     record.CreatedAt.UTC(),
		UpdatedAt:     record.UpdatedAt.UTC(),
	}
	if _, ok := walletDEXExchanges[record.Exchange]; ok {
		if apiKey, err := s.cipher.Decrypt(record.APIKeyEnc); err == nil {
			view.WalletAddress = apiKey
		}
		view.CredentialsPresent = len(record.APIKeyEnc) > 0 && len(record.APISecretEnc) > 0
		view.TradingMode = resolvedCredentialKind(record)
		view.TradingStatus = TradingStatusChecking
		view.ResolvedAccountIndex = record.AccountIndex
		if record.APIKeyIndex != nil {
			value := int32(*record.APIKeyIndex)
			view.ResolvedAPIKeyIndex = &value
		}
	} else if _, ok := cexTradingExchanges[record.Exchange]; ok {
		view.CredentialsPresent = true
		view.CredentialsVerified = true
		view.TradingReady = true
		view.TradingStatus = TradingStatusReady
		view.TradingMode = "cex"
	}
	return view
}

func (s *Service) afterTradingAccountBound(
	ctx context.Context,
	record TradingAccountRecord,
) TradingAccountView {
	view := s.toTradingAccountView(record)
	if s.fees == nil {
		return view
	}
	if strings.EqualFold(record.Exchange, "polymarket") {
		_ = s.fees.MarkUnsupported(ctx, record)
		s.attachFeeView(ctx, &view)
		return view
	}
	_ = s.fees.MarkPending(ctx, record)
	done := s.fees.Enqueue(record.ID, true)
	timer := time.NewTimer(feeBindWaitTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	s.attachFeeView(ctx, &view)
	return view
}

func (s *Service) attachFeeView(ctx context.Context, view *TradingAccountView) {
	if view == nil {
		return
	}
	if s.fees == nil {
		if strings.EqualFold(view.Exchange, "polymarket") {
			view.Fees = TradingAccountFees{
				Spot:               MarketFeeView{Status: portfolio.FeeStatusUnsupported},
				Contract:           MarketFeeView{Status: portfolio.FeeStatusUnsupported},
				SyncStatus:         FeeSyncOK,
				UnsupportedMarkets: []string{portfolio.FeeMarketSpot, portfolio.FeeMarketContract},
			}
		}
		return
	}
	fees, err := s.fees.Load(ctx, view.ID)
	if err != nil {
		if cached, ok := s.fees.Fees(view.ID); ok {
			view.Fees = cached
		}
		return
	}
	view.Fees = fees
}

func (s *Service) GetTradingAccountFeeRates(
	ctx context.Context,
	token string,
	id int64,
) (TradingAccountFees, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingAccountFees{}, err
	}
	if s.trading == nil || id <= 0 {
		return TradingAccountFees{}, ErrInvalidTradingAccount
	}
	if _, err := s.trading.GetByOwner(ctx, session.Username, id); err != nil {
		return TradingAccountFees{}, err
	}
	if s.fees == nil {
		return TradingAccountFees{}, ErrTradingAccountNotFound
	}
	return s.fees.Load(ctx, id)
}

func (s *Service) SyncTradingAccountFeeRates(
	ctx context.Context,
	token string,
	id int64,
) (TradingAccountFees, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingAccountFees{}, err
	}
	if s.trading == nil || s.fees == nil || id <= 0 {
		return TradingAccountFees{}, ErrInvalidTradingAccount
	}
	if _, err := s.trading.GetByOwner(ctx, session.Username, id); err != nil {
		return TradingAccountFees{}, err
	}
	done := s.fees.Enqueue(id, true)
	select {
	case <-done:
	case <-ctx.Done():
		return TradingAccountFees{}, ctx.Err()
	}
	return s.fees.Load(ctx, id)
}

func (s *Service) TradingCredentialsFromRecord(record TradingAccountRecord) (TradingCredentials, error) {
	return s.decryptTradingCredentials(record)
}

func normalizeCreateInput(input CreateTradingAccountInput) (CreateTradingAccountInput, error) {
	productName := strings.TrimSpace(input.ProductName)
	accountName := strings.TrimSpace(input.AccountName)
	exchange := strings.ToLower(strings.TrimSpace(input.Exchange))
	apiKey := strings.TrimSpace(input.APIKey)
	apiSecret := strings.TrimSpace(input.APISecret)
	passphrase := strings.TrimSpace(input.Passphrase)
	tradingAPIKey := strings.TrimSpace(input.TradingAPIKey)
	tradingAPISecret := strings.TrimSpace(input.TradingAPISecret)
	signingAddress := strings.TrimSpace(input.SigningAddress)
	vaultAddress := strings.TrimSpace(input.VaultAddress)

	if productName == "" || accountName == "" || apiKey == "" || apiSecret == "" {
		return CreateTradingAccountInput{}, ErrInvalidTradingAccount
	}
	if utf8.RuneCountInString(productName) > 64 || utf8.RuneCountInString(accountName) > 64 {
		return CreateTradingAccountInput{}, ErrInvalidTradingAccount
	}
	if utf8.RuneCountInString(apiKey) > 256 || utf8.RuneCountInString(apiSecret) > 256 ||
		utf8.RuneCountInString(passphrase) > 256 ||
		utf8.RuneCountInString(tradingAPIKey) > 256 ||
		utf8.RuneCountInString(tradingAPISecret) > 256 {
		return CreateTradingAccountInput{}, ErrInvalidTradingAccount
	}
	if _, ok := supportedExchanges[exchange]; !ok {
		return CreateTradingAccountInput{}, ErrUnsupportedExchange
	}
	if _, required := passphraseRequiredExchanges[exchange]; required && passphrase == "" {
		return CreateTradingAccountInput{}, ErrPassphraseRequired
	}
	if _, ok := walletDEXExchanges[exchange]; ok {
		apiKey, err := normalizeWalletAddress(apiKey)
		if err != nil {
			return CreateTradingAccountInput{}, err
		}
		if exchange == "lighter" {
			apiSecret, err = normalizeLighterPrivateKey(apiSecret)
		} else {
			apiSecret, err = normalizeWalletPrivateKey(apiSecret)
		}
		if err != nil {
			return CreateTradingAccountInput{}, err
		}
		if vaultAddress != "" {
			vaultAddress, err = normalizeWalletAddress(vaultAddress)
			if err != nil {
				return CreateTradingAccountInput{}, err
			}
		}
		normalized := CreateTradingAccountInput{
			ProductName:   productName,
			Exchange:      exchange,
			AccountName:   accountName,
			APIKey:        apiKey,
			APISecret:     apiSecret,
			TradingAPIKey: tradingAPIKey, TradingAPISecret: tradingAPISecret,
			SigningAddress: signingAddress, VaultAddress: vaultAddress,
			AccountIndex: input.AccountIndex, APIKeyIndex: input.APIKeyIndex,
		}
		if err := normalizeVenueTradingInput(&normalized); err != nil {
			return CreateTradingAccountInput{}, err
		}
		return normalized, nil
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

func normalizeVenueTradingInput(input *CreateTradingAccountInput) error {
	if input.VaultAddress != "" {
		normalized, err := normalizeWalletAddress(input.VaultAddress)
		if err != nil {
			return err
		}
		input.VaultAddress = normalized
	}
	hasExtension := input.TradingAPIKey != "" || input.TradingAPISecret != "" ||
		input.SigningAddress != "" || input.AccountIndex != nil || input.APIKeyIndex != nil
	if !hasExtension {
		return nil
	}
	switch input.Exchange {
	case "aster":
		if input.TradingAPIKey == "" || input.TradingAPISecret == "" {
			return ErrInvalidTradingAccount
		}
	case "hyperliquid":
		if input.TradingAPISecret == "" || input.SigningAddress == "" {
			return ErrInvalidTradingAccount
		}
		var err error
		input.TradingAPISecret, err = normalizeWalletPrivateKey(input.TradingAPISecret)
		if err != nil {
			return err
		}
		input.SigningAddress, err = normalizeWalletAddress(input.SigningAddress)
		if err != nil {
			return err
		}
	case "lighter":
		if input.TradingAPISecret == "" || input.AccountIndex == nil || input.APIKeyIndex == nil ||
			*input.AccountIndex < 0 || *input.APIKeyIndex < 0 || *input.APIKeyIndex > 255 {
			return ErrInvalidTradingAccount
		}
		secret, err := normalizeLighterPrivateKey(input.TradingAPISecret)
		if err != nil {
			return err
		}
		input.TradingAPISecret = secret
	default:
		return ErrUnsupportedExchange
	}
	return nil
}

func defaultAPIWalletKind(exchange string) string {
	switch exchange {
	case "aster":
		return CredentialKindAsterAPIWallet
	case "hyperliquid":
		return CredentialKindHyperliquidWallet
	case "lighter":
		return CredentialKindLighterAPIWallet
	default:
		return ""
	}
}

func credentialKindForCreate(input CreateTradingAccountInput) string {
	if _, ok := walletDEXExchanges[input.Exchange]; !ok {
		return ""
	}
	hasExtension := input.TradingAPIKey != "" || input.TradingAPISecret != "" ||
		input.SigningAddress != "" || input.AccountIndex != nil || input.APIKeyIndex != nil
	if !hasExtension {
		return defaultAPIWalletKind(input.Exchange)
	}
	switch input.Exchange {
	case "aster":
		return CredentialKindAsterHMAC
	case "hyperliquid":
		return CredentialKindHyperliquidAgent
	case "lighter":
		return CredentialKindLighterAPI
	default:
		return defaultAPIWalletKind(input.Exchange)
	}
}

func resolvedCredentialKind(record TradingAccountRecord) string {
	kind := strings.TrimSpace(record.CredentialKind)
	if kind != "" {
		return kind
	}
	return defaultAPIWalletKind(record.Exchange)
}

func validateAPIWalletCredentials(exchange, apiKey, apiSecret string) error {
	if apiKey == "" || apiSecret == "" {
		return ErrInvalidTradingAccount
	}
	if _, err := normalizeWalletAddress(apiKey); err != nil {
		return err
	}
	if exchange == "lighter" {
		if _, err := normalizeLighterPrivateKey(apiSecret); err != nil {
			return err
		}
		return nil
	}
	if _, err := normalizeWalletPrivateKey(apiSecret); err != nil {
		return err
	}
	return nil
}

func validateExtensionCredentialGroup(
	kind, apiKey, apiSecret, signingAddress string,
	accountIndex *int64,
	apiKeyIndex *int16,
) error {
	switch kind {
	case CredentialKindAsterHMAC:
		if apiKey == "" || apiSecret == "" {
			return ErrInvalidTradingAccount
		}
	case CredentialKindHyperliquidAgent:
		if apiSecret == "" || signingAddress == "" {
			return ErrInvalidTradingAccount
		}
	case CredentialKindLighterAPI:
		if apiSecret == "" || accountIndex == nil || apiKeyIndex == nil {
			return ErrInvalidTradingAccount
		}
	default:
		return ErrInvalidTradingAccount
	}
	return nil
}

func encryptOptional(cipher credentialProtector, value string) ([]byte, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	return cipher.Encrypt(value)
}

func decryptOptional(cipher credentialProtector, value []byte) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	return cipher.Decrypt(value)
}

func normalizeWalletAddress(value string) (string, error) {
	if !common.IsHexAddress(value) {
		return "", ErrInvalidTradingAccount
	}
	return common.HexToAddress(value).Hex(), nil
}

func normalizeWalletPrivateKey(value string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	if len(trimmed) != 64 {
		return "", ErrInvalidTradingAccount
	}
	if _, err := crypto.HexToECDSA(trimmed); err != nil {
		return "", ErrInvalidTradingAccount
	}
	return "0x" + strings.ToLower(trimmed), nil
}

func normalizeLighterPrivateKey(value string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	switch len(trimmed) {
	case 64:
		return normalizeWalletPrivateKey(value)
	case 80:
		for _, r := range trimmed {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return "", ErrInvalidTradingAccount
			}
		}
		return "0x" + strings.ToLower(trimmed), nil
	default:
		return "", ErrInvalidTradingAccount
	}
}
