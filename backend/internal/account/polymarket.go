package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
	"selfquant/backend/internal/polymarketauth"
)

var ErrInvalidPolymarketAccount = errors.New("invalid polymarket account")
var ErrPolymarketCredentialsInvalid = errors.New("polymarket credentials invalid; rebind required")

type polymarketCredentialIssuer interface {
	CreateOrDerive(context.Context, string, uint64) (polymarketauth.Credentials, string, error)
	Derive(context.Context, string, uint64) (polymarketauth.Credentials, string, error)
}

type CreatePolymarketTradingAccountInput struct {
	ProductName   string
	AccountName   string
	PrivateKey    string
	WalletType    string
	FunderAddress string
}

type PolymarketCredentials struct {
	TradingAccountID int64
	AccountName      string
	SignerAddress    string
	FunderAddress    string
	WalletType       string
	SignatureType    int32
	PrivateKey       string
	APIKey           string
	APISecret        string
	Passphrase       string
}

func (s *Service) CreatePolymarketTradingAccount(
	ctx context.Context,
	token string,
	input CreatePolymarketTradingAccountInput,
) (TradingAccountView, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingAccountView{}, err
	}
	if s.polymarket == nil || s.polymarketIssuer == nil || s.cipher == nil {
		return TradingAccountView{}, errors.New("polymarket account binding not configured")
	}
	productName := strings.TrimSpace(input.ProductName)
	if productName == "" {
		productName = "Polymarket"
	}
	accountName := strings.TrimSpace(input.AccountName)
	privateKey := strings.TrimSpace(input.PrivateKey)
	walletType := strings.ToLower(strings.TrimSpace(input.WalletType))
	funderAddress := strings.TrimSpace(input.FunderAddress)
	if accountName == "" || privateKey == "" ||
		utf8.RuneCountInString(productName) > 64 ||
		utf8.RuneCountInString(accountName) > 64 {
		return TradingAccountView{}, ErrInvalidPolymarketAccount
	}
	if walletType == "" {
		walletType = "eoa"
	}
	if walletType != "eoa" && walletType != "deposit" {
		return TradingAccountView{}, ErrInvalidPolymarketAccount
	}

	credentials, signerAddress, err := s.polymarketIssuer.CreateOrDerive(ctx, privateKey, 0)
	if err != nil {
		return TradingAccountView{}, fmt.Errorf("derive polymarket credentials: %w", err)
	}
	signatureType := signatureTypeForWallet(walletType)
	if walletType == "eoa" {
		funderAddress = signerAddress
	} else {
		if !common.IsHexAddress(funderAddress) {
			return TradingAccountView{}, ErrInvalidPolymarketAccount
		}
		funderAddress = common.HexToAddress(funderAddress).Hex()
		// The bound funder is the Polymarket Proxy Wallet controlled by the EOA signer.
	}

	encrypt := func(value string) ([]byte, error) {
		encrypted, encryptErr := s.cipher.Encrypt(value)
		if encryptErr != nil {
			return nil, encryptErr
		}
		return encrypted, nil
	}
	privateKeyEnc, err := encrypt(privateKey)
	if err != nil {
		return TradingAccountView{}, err
	}
	apiKeyEnc, err := encrypt(credentials.APIKey)
	if err != nil {
		return TradingAccountView{}, err
	}
	apiSecretEnc, err := encrypt(credentials.Secret)
	if err != nil {
		return TradingAccountView{}, err
	}
	passphraseEnc, err := encrypt(credentials.Passphrase)
	if err != nil {
		return TradingAccountView{}, err
	}

	record, stored, err := s.polymarket.Create(ctx, TradingAccountRecord{
		OwnerUsername: session.Username,
		ProductName:   productName,
		Exchange:      "polymarket",
		AccountName:   accountName,
		APIKeyEnc:     apiKeyEnc,
		APISecretEnc:  apiSecretEnc,
		PassphraseEnc: passphraseEnc,
	}, PolymarketCredentialRecord{
		SignerAddress:   signerAddress,
		FunderAddress:   funderAddress,
		WalletType:      walletType,
		SignatureType:   signatureType,
		CredentialNonce: 0,
		PrivateKeyEnc:   privateKeyEnc,
		APIKeyEnc:       apiKeyEnc,
		APISecretEnc:    apiSecretEnc,
		PassphraseEnc:   passphraseEnc,
	})
	if err != nil {
		return TradingAccountView{}, err
	}
	view, err := s.toTradingAccountView(record)
	if err != nil {
		return TradingAccountView{}, err
	}
	view.WalletAddress = stored.FunderAddress
	view.WalletType = stored.WalletType
	view.BindingStatus = stored.BindingStatus
	return view, nil
}

func signatureTypeForWallet(walletType string) int32 {
	if walletType == "deposit" {
		return 1
	}
	return 0
}

func (s *Service) GetPolymarketCredentials(
	ctx context.Context,
	token string,
	accountID int64,
) (PolymarketCredentials, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	if accountID <= 0 || s.polymarket == nil || s.cipher == nil {
		return PolymarketCredentials{}, ErrInvalidPolymarketAccount
	}
	record, err := s.polymarket.GetByOwner(ctx, session.Username, accountID)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	if record.BindingStatus == "invalid" {
		return PolymarketCredentials{}, ErrPolymarketCredentialsInvalid
	}
	decrypt := func(value []byte) (string, error) {
		plain, decryptErr := s.cipher.Decrypt(value)
		if decryptErr != nil {
			return "", decryptErr
		}
		return plain, nil
	}
	privateKey, err := decrypt(record.PrivateKeyEnc)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	apiKey, err := decrypt(record.APIKeyEnc)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	apiSecret, err := decrypt(record.APISecretEnc)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	passphrase, err := decrypt(record.PassphraseEnc)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	return PolymarketCredentials{
		TradingAccountID: record.TradingAccountID,
		AccountName:      record.AccountName,
		SignerAddress:    record.SignerAddress,
		FunderAddress:    record.FunderAddress,
		WalletType:       record.WalletType,
		SignatureType:    record.SignatureType,
		PrivateKey:       privateKey,
		APIKey:           apiKey,
		APISecret:        apiSecret,
		Passphrase:       passphrase,
	}, nil
}

func (s *Service) RefreshPolymarketCredentials(
	ctx context.Context,
	token string,
	accountID int64,
) (PolymarketCredentials, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	if accountID <= 0 || s.polymarket == nil || s.polymarketIssuer == nil || s.cipher == nil {
		return PolymarketCredentials{}, ErrInvalidPolymarketAccount
	}
	record, err := s.polymarket.GetByOwner(ctx, session.Username, accountID)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	privateKey, err := s.cipher.Decrypt(record.PrivateKeyEnc)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	issued, signerAddress, err := s.polymarketIssuer.Derive(
		ctx, privateKey, record.CredentialNonce,
	)
	if err != nil {
		return PolymarketCredentials{}, fmt.Errorf("refresh polymarket credentials: %w", err)
	}
	if !strings.EqualFold(signerAddress, record.SignerAddress) {
		return PolymarketCredentials{}, errors.New("refreshed signer does not match binding")
	}
	encrypt := func(value string) ([]byte, error) {
		return s.cipher.Encrypt(value)
	}
	apiKeyEnc, err := encrypt(issued.APIKey)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	apiSecretEnc, err := encrypt(issued.Secret)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	passphraseEnc, err := encrypt(issued.Passphrase)
	if err != nil {
		return PolymarketCredentials{}, err
	}
	if err := s.polymarket.UpdateCredentialsByOwner(
		ctx, session.Username, accountID,
		apiKeyEnc, apiSecretEnc, passphraseEnc, "active",
	); err != nil {
		return PolymarketCredentials{}, err
	}
	return PolymarketCredentials{
		TradingAccountID: record.TradingAccountID,
		AccountName:      record.AccountName,
		SignerAddress:    record.SignerAddress,
		FunderAddress:    record.FunderAddress,
		WalletType:       record.WalletType,
		SignatureType:    record.SignatureType,
		PrivateKey:       privateKey,
		APIKey:           issued.APIKey,
		APISecret:        issued.Secret,
		Passphrase:       issued.Passphrase,
	}, nil
}

func (s *Service) InvalidatePolymarketCredentials(
	ctx context.Context,
	token string,
	accountID int64,
) error {
	session, err := s.ValidateSession(token)
	if err != nil {
		return err
	}
	if s.polymarket == nil || accountID <= 0 {
		return ErrInvalidPolymarketAccount
	}
	return s.polymarket.UpdateBindingStatusByOwner(
		ctx, session.Username, accountID, "invalid",
	)
}
