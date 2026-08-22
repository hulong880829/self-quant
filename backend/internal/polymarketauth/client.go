package polymarketauth

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

const authMessage = "This message attests that I control the given wallet"

type Credentials struct {
	APIKey     string `json:"apiKey"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

type Issuer interface {
	CreateOrDerive(context.Context, string, uint64) (Credentials, string, error)
	Derive(context.Context, string, uint64) (Credentials, string, error)
}

type AuthError struct {
	StatusCode int
	Message    string
}

func (e *AuthError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("polymarket auth returned %d", e.StatusCode)
	}
	return fmt.Sprintf("polymarket auth returned %d: %s", e.StatusCode, e.Message)
}

type Client struct {
	baseURL string
	http    *http.Client
	now     func() time.Time
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
		now:     time.Now,
	}
}

func ParsePrivateKey(value string) (*ecdsa.PrivateKey, string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	if len(trimmed) != 64 {
		return nil, "", errors.New("private key must contain 32 bytes")
	}
	key, err := crypto.HexToECDSA(trimmed)
	if err != nil {
		return nil, "", errors.New("invalid private key")
	}
	address := crypto.PubkeyToAddress(key.PublicKey).Hex()
	return key, address, nil
}

func (c *Client) CreateOrDerive(
	ctx context.Context,
	privateKey string,
	nonce uint64,
) (Credentials, string, error) {
	key, address, err := ParsePrivateKey(privateKey)
	if err != nil {
		return Credentials{}, "", err
	}
	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	signature, err := signAuth(key, address, timestamp, nonce)
	if err != nil {
		return Credentials{}, "", err
	}
	headers := map[string]string{
		"POLY_ADDRESS":   address,
		"POLY_SIGNATURE": signature,
		"POLY_TIMESTAMP": timestamp,
		"POLY_NONCE":     strconv.FormatUint(nonce, 10),
	}
	credentials, statusCode, err := c.request(ctx, http.MethodGet, "/auth/derive-api-key", headers)
	if err == nil {
		return credentials, address, nil
	}
	var authErr *AuthError
	explicitlyMissing := errors.As(err, &authErr) &&
		authErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(authErr.Message), "could not derive api key")
	if statusCode != http.StatusNotFound && !explicitlyMissing {
		return Credentials{}, "", err
	}
	credentials, _, err = c.request(ctx, http.MethodPost, "/auth/api-key", headers)
	if err != nil {
		return Credentials{}, "", err
	}
	return credentials, address, nil
}

func (c *Client) Derive(
	ctx context.Context,
	privateKey string,
	nonce uint64,
) (Credentials, string, error) {
	key, address, err := ParsePrivateKey(privateKey)
	if err != nil {
		return Credentials{}, "", err
	}
	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	signature, err := signAuth(key, address, timestamp, nonce)
	if err != nil {
		return Credentials{}, "", err
	}
	credentials, _, err := c.request(ctx, http.MethodGet, "/auth/derive-api-key", map[string]string{
		"POLY_ADDRESS": address, "POLY_SIGNATURE": signature,
		"POLY_TIMESTAMP": timestamp, "POLY_NONCE": strconv.FormatUint(nonce, 10),
	})
	if err != nil {
		return Credentials{}, "", err
	}
	return credentials, address, nil
}

func signAuth(
	key *ecdsa.PrivateKey,
	address string,
	timestamp string,
	nonce uint64,
) (string, error) {
	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
			},
			"ClobAuth": {
				{Name: "address", Type: "address"},
				{Name: "timestamp", Type: "string"},
				{Name: "nonce", Type: "uint256"},
				{Name: "message", Type: "string"},
			},
		},
		PrimaryType: "ClobAuth",
		Domain: apitypes.TypedDataDomain{
			Name:    "ClobAuthDomain",
			Version: "1",
			ChainId: ethmath.NewHexOrDecimal256(137),
		},
		Message: apitypes.TypedDataMessage{
			"address":   common.HexToAddress(address).Hex(),
			"timestamp": timestamp,
			"nonce":     fmt.Sprintf("%d", nonce),
			"message":   authMessage,
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		return "", fmt.Errorf("hash auth payload: %w", err)
	}
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		return "", fmt.Errorf("sign auth payload: %w", err)
	}
	signature[64] += 27
	return "0x" + hex.EncodeToString(signature), nil
}

func (c *Client) request(
	ctx context.Context,
	method string,
	path string,
	headers map[string]string,
) (Credentials, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return Credentials{}, 0, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Credentials{}, 0, fmt.Errorf("polymarket auth request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Credentials{}, response.StatusCode, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &payload)
		message := strings.TrimSpace(payload.Error)
		if message == "" {
			message = strings.TrimSpace(string(body))
		}
		return Credentials{}, response.StatusCode, &AuthError{
			StatusCode: response.StatusCode, Message: message,
		}
	}
	var credentials Credentials
	if err := json.Unmarshal(body, &credentials); err != nil {
		return Credentials{}, response.StatusCode, fmt.Errorf("decode polymarket credentials: %w", err)
	}
	if strings.TrimSpace(credentials.APIKey) == "" ||
		strings.TrimSpace(credentials.Secret) == "" ||
		strings.TrimSpace(credentials.Passphrase) == "" {
		return Credentials{}, response.StatusCode, errors.New("polymarket returned incomplete credentials")
	}
	return credentials, response.StatusCode, nil
}
