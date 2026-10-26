package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const maxBindIdempotencyKeyLength = 128

func normalizeBindIdempotencyKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", nil
	}
	if utf8.RuneCountInString(key) > maxBindIdempotencyKeyLength {
		return "", ErrInvalidIdempotencyKey
	}
	return key, nil
}

func bindRequestHash(input CreateTradingAccountInput) string {
	payload, err := json.Marshal(struct {
		ProductName      string `json:"productName"`
		Exchange         string `json:"exchange"`
		AccountName      string `json:"accountName"`
		APIKey           string `json:"apiKey"`
		APISecret        string `json:"apiSecret"`
		Passphrase       string `json:"passphrase"`
		TradingAPIKey    string `json:"tradingApiKey"`
		TradingAPISecret string `json:"tradingApiSecret"`
		SigningAddress   string `json:"signingAddress"`
		VaultAddress     string `json:"vaultAddress"`
		AccountIndex     *int64 `json:"accountIndex"`
		APIKeyIndex      *int32 `json:"apiKeyIndex"`
	}{
		ProductName:      input.ProductName,
		Exchange:         input.Exchange,
		AccountName:      input.AccountName,
		APIKey:           input.APIKey,
		APISecret:        input.APISecret,
		Passphrase:       input.Passphrase,
		TradingAPIKey:    input.TradingAPIKey,
		TradingAPISecret: input.TradingAPISecret,
		SigningAddress:   input.SigningAddress,
		VaultAddress:     input.VaultAddress,
		AccountIndex:     input.AccountIndex,
		APIKeyIndex:      input.APIKeyIndex,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func polymarketBindRequestHash(input CreatePolymarketTradingAccountInput) string {
	payload, err := json.Marshal(struct {
		ProductName   string `json:"productName"`
		Exchange      string `json:"exchange"`
		AccountName   string `json:"accountName"`
		PrivateKey    string `json:"privateKey"`
		WalletType    string `json:"walletType"`
		FunderAddress string `json:"funderAddress"`
	}{
		ProductName:   input.ProductName,
		Exchange:      "polymarket",
		AccountName:   input.AccountName,
		PrivateKey:    input.PrivateKey,
		WalletType:    input.WalletType,
		FunderAddress: input.FunderAddress,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
