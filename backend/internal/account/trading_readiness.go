package account

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"selfquant/backend/internal/exchange"
)

const (
	defaultHyperliquidInfoURL = "https://api.hyperliquid.xyz"
	defaultAsterFuturesURL    = "https://fapi.asterdex.com"
	defaultLighterAPIURL      = "https://mainnet.zklighter.elliot.ai"

	TradingStatusUnknown = "unknown"
)

func (s *Service) InspectTradingReadiness(
	ctx context.Context,
	token string,
	id int64,
) (TradingReadiness, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingReadiness{}, err
	}
	if s.trading == nil || s.cipher == nil {
		return TradingReadiness{}, fmt.Errorf("trading accounts not configured")
	}
	if id <= 0 {
		return TradingReadiness{}, ErrInvalidTradingAccount
	}
	record, err := s.trading.GetByOwner(ctx, session.Username, id)
	if err != nil {
		return TradingReadiness{}, err
	}
	return s.inspectRecord(ctx, session.Username, record)
}

func (s *Service) inspectRecord(
	ctx context.Context,
	owner string,
	record TradingAccountRecord,
) (TradingReadiness, error) {
	ready := TradingReadiness{
		TradingAccountID:   record.ID,
		Exchange:           record.Exchange,
		CredentialsPresent: len(record.APIKeyEnc) > 0 && len(record.APISecretEnc) > 0,
		TradingMode:        resolvedCredentialKind(record),
		TradingStatus:      TradingStatusChecking,
		ResolvedAccountIndex: record.AccountIndex,
	}
	if record.APIKeyIndex != nil {
		value := int32(*record.APIKeyIndex)
		ready.ResolvedAPIKeyIndex = &value
	}
	if _, ok := cexTradingExchanges[record.Exchange]; ok {
		ready.CredentialsVerified = ready.CredentialsPresent
		ready.TradingReady = ready.CredentialsPresent
		ready.TradingStatus = TradingStatusReady
		ready.TradingMode = "cex"
		return ready, nil
	}
	if _, ok := walletDEXExchanges[record.Exchange]; !ok {
		return failReadiness(ready, TradingStatusUnsupportedAuthMode, "unsupported exchange for trading readiness"), nil
	}
	credentials, err := s.decryptTradingCredentials(record)
	if err != nil {
		return failReadiness(ready, TradingStatusPrivateKeyMismatch, "stored trading credentials could not be decrypted"), nil
	}
	ready.TradingMode = credentials.CredentialKind
	ready.ResolvedAccountIndex = credentials.AccountIndex
	ready.ResolvedAPIKeyIndex = credentials.APIKeyIndex
	switch record.Exchange {
	case "hyperliquid":
		return s.inspectHyperliquid(ctx, credentials, ready)
	case "aster":
		return s.inspectAster(ctx, credentials, ready)
	case "lighter":
		return s.inspectLighter(ctx, owner, record.ID, credentials, ready)
	default:
		return failReadiness(ready, TradingStatusUnsupportedAuthMode, "unsupported DEX venue"), nil
	}
}

func failReadiness(ready TradingReadiness, code, reason string) TradingReadiness {
	ready.TradingReady = false
	ready.TradingStatus = code
	ready.TradingUnavailableCode = code
	ready.TradingUnavailableReason = reason
	return ready
}

func readyReadiness(ready TradingReadiness) TradingReadiness {
	ready.CredentialsVerified = true
	ready.TradingReady = true
	ready.TradingStatus = TradingStatusReady
	ready.TradingUnavailableCode = ""
	ready.TradingUnavailableReason = ""
	return ready
}

func (s *Service) inspectHyperliquid(
	ctx context.Context,
	credentials TradingCredentials,
	ready TradingReadiness,
) (TradingReadiness, error) {
	owner := strings.TrimSpace(credentials.APIKey)
	if owner == "" {
		return failReadiness(ready, TradingStatusAccountNotFound, "Hyperliquid owner address is missing"), nil
	}
	derived, err := addressFromPrivateKey(credentials.APISecret)
	if err != nil {
		return failReadiness(ready, TradingStatusPrivateKeyMismatch, "Hyperliquid private key is invalid"), nil
	}
	if !sameAddress(derived, owner) {
		agents, err := s.hyperliquidExtraAgents(ctx, owner)
		if err != nil {
			return mapVenueInspectError(ready, err, "Hyperliquid authorization check failed"), nil
		}
		if !hyperliquidAgentAuthorized(agents, derived) {
			return failReadiness(ready, TradingStatusWalletUnauthorized, "Hyperliquid API wallet is not authorized"), nil
		}
	}
	return readyReadiness(ready), nil
}

func (s *Service) inspectAster(
	ctx context.Context,
	credentials TradingCredentials,
	ready TradingReadiness,
) (TradingReadiness, error) {
	switch credentials.CredentialKind {
	case CredentialKindAsterHMAC:
		if err := s.asterHMACAccount(ctx, credentials); err != nil {
			return mapVenueInspectError(ready, err, "Aster HMAC account check failed"), nil
		}
		return readyReadiness(ready), nil
	case CredentialKindAsterAPIWallet:
		if err := s.asterV3Account(ctx, credentials); err != nil {
			return mapVenueInspectError(ready, err, "Aster API wallet account check failed"), nil
		}
		ready.CredentialsVerified = true
		if err := assertAsterWriteSignable(credentials); err != nil {
			return failReadiness(ready, TradingStatusUnsupportedAuthMode, err.Error()), nil
		}
		return readyReadiness(ready), nil
	default:
		return failReadiness(ready, TradingStatusUnsupportedAuthMode, "unsupported Aster credential kind"), nil
	}
}

func (s *Service) inspectLighter(
	ctx context.Context,
	owner string,
	id int64,
	credentials TradingCredentials,
	ready TradingReadiness,
) (TradingReadiness, error) {
	accountIndex := credentials.AccountIndex
	apiKeyIndex := credentials.APIKeyIndex
	if accountIndex == nil || apiKeyIndex == nil {
		discoveredAccount, discoveredKey, err := s.discoverLighterIndexes(ctx, credentials)
		if err != nil {
			return mapVenueInspectError(ready, err, "Lighter API wallet discovery failed"), nil
		}
		accountIndex = &discoveredAccount
		apiKeyIndex = &discoveredKey
		ready.ResolvedAccountIndex = accountIndex
		ready.ResolvedAPIKeyIndex = apiKeyIndex
		if persistErr := s.persistLighterIndexes(ctx, owner, id, discoveredAccount, discoveredKey); persistErr != nil {
			return failReadiness(ready, TradingStatusVenueUnavailable, "Lighter indexes could not be saved"), nil
		}
	}
	if err := s.verifyLighterAuth(ctx, credentials, *accountIndex, *apiKeyIndex); err != nil {
		return mapVenueInspectError(ready, err, "Lighter authorization check failed"), nil
	}
	ready.ResolvedAccountIndex = accountIndex
	ready.ResolvedAPIKeyIndex = apiKeyIndex
	return readyReadiness(ready), nil
}

func (s *Service) persistLighterIndexes(ctx context.Context, owner string, id, accountIndex int64, apiKeyIndex int32) error {
	if s.trading == nil {
		return fmt.Errorf("trading accounts not configured")
	}
	index16 := int16(apiKeyIndex)
	return s.trading.UpdateIndexes(ctx, owner, id, &accountIndex, &index16)
}

func mapVenueInspectError(ready TradingReadiness, err error, fallback string) TradingReadiness {
	if err == nil {
		return ready
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return failReadiness(ready, TradingStatusUnknown, "venue request timed out")
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "api wallet not found") || strings.Contains(message, "not uniquely matched"):
		return failReadiness(ready, TradingStatusAPIWalletNotFound, err.Error())
	case strings.Contains(message, "account not found") || strings.Contains(message, "no accounts"):
		return failReadiness(ready, TradingStatusAccountNotFound, err.Error())
	case strings.Contains(message, "unauthorized") || strings.Contains(message, "not authorized"):
		return failReadiness(ready, TradingStatusWalletUnauthorized, err.Error())
	case strings.Contains(message, "private key"):
		return failReadiness(ready, TradingStatusPrivateKeyMismatch, err.Error())
	default:
		return failReadiness(ready, TradingStatusVenueUnavailable, fallback)
	}
}

func (s *Service) readinessClient() *http.Client {
	if s.readinessHTTP != nil {
		return s.readinessHTTP
	}
	return &http.Client{Timeout: 8 * time.Second}
}

func (s *Service) readinessURL(exchange, fallback string) string {
	if s.readinessURLs != nil {
		if value := strings.TrimSpace(s.readinessURLs[exchange]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return fallback
}

func (s *Service) hyperliquidExtraAgents(ctx context.Context, user string) ([]hyperliquidAgent, error) {
	var agents []hyperliquidAgent
	if err := s.postJSON(ctx, s.readinessURL("hyperliquid", defaultHyperliquidInfoURL)+"/info", map[string]any{
		"type": "extraAgents", "user": user,
	}, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

type hyperliquidAgent struct {
	Address    string `json:"address"`
	Name       string `json:"name"`
	ValidUntil int64  `json:"validUntil"`
}

func hyperliquidAgentAuthorized(agents []hyperliquidAgent, address string) bool {
	now := time.Now().UnixMilli()
	for _, agent := range agents {
		if !sameAddress(agent.Address, address) {
			continue
		}
		if agent.ValidUntil > 0 && agent.ValidUntil < now {
			continue
		}
		return true
	}
	return false
}

func (s *Service) asterV3Account(ctx context.Context, credentials TradingCredentials) error {
	user, signer, key, err := asterInspectAuth(credentials)
	if err != nil {
		return err
	}
	params := asterV3BaseParams(user, signer, time.Now())
	var payload map[string]any
	return s.asterSignedGet(ctx, "/fapi/v3/account", params, key, &payload)
}

func (s *Service) asterHMACAccount(ctx context.Context, credentials TradingCredentials) error {
	values := url.Values{}
	values.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	values.Set("recvWindow", "5000")
	values.Set("signature", hmacHex256(credentials.APISecret, values.Encode()))
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		s.readinessURL("aster", defaultAsterFuturesURL)+"/fapi/v2/account?"+values.Encode(),
		nil,
	)
	if err != nil {
		return err
	}
	req.Header.Set("X-MBX-APIKEY", credentials.APIKey)
	resp, err := s.readinessClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("aster hmac account status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func assertAsterWriteSignable(credentials TradingCredentials) error {
	user, signer, key, err := asterInspectAuth(credentials)
	if err != nil {
		return err
	}
	params := asterV3BaseParams(user, signer, time.Unix(1_700_000_000, 0).UTC())
	params["symbol"] = "BTCUSDT"
	params["side"] = "BUY"
	params["type"] = "LIMIT"
	params["quantity"] = "0.001"
	params["price"] = "1"
	params["timeInForce"] = "GTC"
	message := exchange.AsterParamString(params)
	if _, err := exchange.SignAsterV3(key, message); err != nil {
		return fmt.Errorf("Aster write signing is not available")
	}
	return nil
}

func asterInspectAuth(credentials TradingCredentials) (string, string, *ecdsa.PrivateKey, error) {
	user := strings.TrimSpace(credentials.APIKey)
	if !common.IsHexAddress(user) {
		return "", "", nil, fmt.Errorf("aster account requires a wallet address")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(credentials.APISecret), "0x"))
	if err != nil {
		return "", "", nil, fmt.Errorf("aster private key is invalid")
	}
	return common.HexToAddress(user).Hex(), crypto.PubkeyToAddress(key.PublicKey).Hex(), key, nil
}

func asterV3BaseParams(user, signer string, now time.Time) map[string]string {
	return map[string]string{
		"user":       user,
		"signer":     signer,
		"nonce":      strconv.FormatInt(now.UnixMicro(), 10),
		"timestamp":  strconv.FormatInt(now.UnixMilli(), 10),
		"recvWindow": "5000",
	}
}

func (s *Service) asterSignedGet(
	ctx context.Context,
	path string,
	params map[string]string,
	key *ecdsa.PrivateKey,
	target any,
) error {
	message := exchange.AsterParamString(params)
	signature, err := exchange.SignAsterV3(key, message)
	if err != nil {
		return err
	}
	rawURL := s.readinessURL("aster", defaultAsterFuturesURL) + path + "?" + message + "&signature=" + url.QueryEscape(signature)
	return s.getJSON(ctx, rawURL, target)
}

func (s *Service) discoverLighterIndexes(
	ctx context.Context,
	credentials TradingCredentials,
) (int64, int32, error) {
	want, err := exchange.LighterPublicKeyHex(credentials.APISecret)
	if err != nil {
		return 0, 0, fmt.Errorf("private key mismatch: %w", err)
	}
	accounts, err := s.lighterAccountsByL1(ctx, credentials.APIKey)
	if err != nil {
		return 0, 0, err
	}
	if len(accounts) == 0 {
		return 0, 0, fmt.Errorf("account not found")
	}
	var matches []lighterIndexMatch
	for _, item := range accounts {
		keys, keyErr := s.lighterAPIKeys(ctx, item.Index)
		if keyErr != nil {
			return 0, 0, keyErr
		}
		index, matchErr := exchange.MatchLighterAPIKeyIndex(want, keys)
		if matchErr != nil {
			continue
		}
		matches = append(matches, lighterIndexMatch{AccountIndex: item.Index, APIKeyIndex: index})
	}
	if len(matches) != 1 {
		return 0, 0, fmt.Errorf("api wallet not found")
	}
	return matches[0].AccountIndex, matches[0].APIKeyIndex, nil
}

func (s *Service) verifyLighterAuth(
	ctx context.Context,
	credentials TradingCredentials,
	accountIndex int64,
	apiKeyIndex int32,
) error {
	want, err := exchange.LighterPublicKeyHex(credentials.APISecret)
	if err != nil {
		return fmt.Errorf("private key mismatch: %w", err)
	}
	keys, err := s.lighterAPIKeys(ctx, accountIndex)
	if err != nil {
		return err
	}
	matched, err := exchange.MatchLighterAPIKeyIndex(want, keys)
	if err != nil || matched != apiKeyIndex {
		return fmt.Errorf("api wallet not found")
	}
	return nil
}

type lighterIndexMatch struct {
	AccountIndex int64
	APIKeyIndex  int32
}

type lighterAccountRow struct {
	Index int64
}

func (s *Service) lighterAccountsByL1(ctx context.Context, address string) ([]lighterAccountRow, error) {
	query := url.Values{"by": {"l1_address"}, "value": {strings.TrimSpace(address)}}
	var payload struct {
		Code     json.Number `json:"code"`
		Message  string      `json:"message"`
		Accounts []struct {
			AccountIndex json.Number `json:"account_index"`
			Index        json.Number `json:"index"`
		} `json:"accounts"`
		Account *struct {
			AccountIndex json.Number `json:"account_index"`
			Index        json.Number `json:"index"`
		} `json:"account"`
	}
	if err := s.getJSON(ctx, s.readinessURL("lighter", defaultLighterAPIURL)+"/api/v1/account?"+query.Encode(), &payload); err != nil {
		return nil, err
	}
	if code, err := payload.Code.Int64(); err == nil && code != 0 && code != 200 {
		return nil, fmt.Errorf("lighter account: %s", strings.TrimSpace(payload.Message))
	}
	rows := payload.Accounts
	if len(rows) == 0 && payload.Account != nil {
		rows = append(rows, *payload.Account)
	}
	result := make([]lighterAccountRow, 0, len(rows))
	for _, row := range rows {
		raw := strings.TrimSpace(row.AccountIndex.String())
		if raw == "" {
			raw = strings.TrimSpace(row.Index.String())
		}
		if raw == "" {
			continue
		}
		index, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, lighterAccountRow{Index: index})
	}
	return result, nil
}

func (s *Service) lighterAPIKeys(ctx context.Context, accountIndex int64) ([]exchange.LighterListedKey, error) {
	query := url.Values{"account_index": {strconv.FormatInt(accountIndex, 10)}}
	var payload struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		APIKeys []struct {
			APIKeyIndex json.Number `json:"api_key_index"`
			Index       json.Number `json:"index"`
			PublicKey   string      `json:"public_key"`
		} `json:"api_keys"`
	}
	if err := s.getJSON(ctx, s.readinessURL("lighter", defaultLighterAPIURL)+"/api/v1/apikeys?"+query.Encode(), &payload); err != nil {
		return nil, err
	}
	if code, err := payload.Code.Int64(); err == nil && code != 0 && code != 200 {
		return nil, fmt.Errorf("lighter apikeys: %s", strings.TrimSpace(payload.Message))
	}
	result := make([]exchange.LighterListedKey, 0, len(payload.APIKeys))
	for _, key := range payload.APIKeys {
		raw := strings.TrimSpace(key.APIKeyIndex.String())
		if raw == "" {
			raw = strings.TrimSpace(key.Index.String())
		}
		if raw == "" {
			continue
		}
		index, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return nil, err
		}
		result = append(result, exchange.LighterListedKey{Index: int32(index), PublicKey: key.PublicKey})
	}
	return result, nil
}

func (s *Service) postJSON(ctx context.Context, rawURL string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return s.doJSON(req, target)
}

func (s *Service) getJSON(ctx context.Context, rawURL string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	return s.doJSON(req, target)
}

func (s *Service) doJSON(req *http.Request, target any) error {
	resp, err := s.readinessClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(body, target)
}

func addressFromPrivateKey(secret string) (string, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(secret), "0x"))
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(key.PublicKey).Hex(), nil
}

func sameAddress(left, right string) bool {
	if !common.IsHexAddress(left) || !common.IsHexAddress(right) {
		return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
	}
	return common.HexToAddress(left) == common.HexToAddress(right)
}

func hmacHex256(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
