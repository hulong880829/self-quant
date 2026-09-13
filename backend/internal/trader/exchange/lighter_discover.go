package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	corex "selfquant/backend/internal/exchange"
)

func (a *lighterAdapter) ensureIndexes(ctx context.Context, credentials *Credentials) error {
	if credentials.AccountIndex != nil && credentials.APIKeyIndex != nil {
		return nil
	}
	accountIndex, apiKeyIndex, err := a.discoverIndexes(ctx, *credentials)
	if err != nil {
		return err
	}
	credentials.AccountIndex = &accountIndex
	credentials.APIKeyIndex = &apiKeyIndex
	return nil
}

func (a *lighterAdapter) discoverIndexes(ctx context.Context, credentials Credentials) (int64, int32, error) {
	want, err := corex.LighterPublicKeyHex(credentials.APISecret)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid Lighter API private key: %v", ErrRejected, err)
	}
	accounts, err := a.accountsByL1(ctx, credentials.APIKey)
	if err != nil {
		return 0, 0, err
	}
	if len(accounts) == 0 {
		return 0, 0, fmt.Errorf("%w: Lighter account not found", ErrRejected)
	}
	var matches []struct {
		account int64
		key     int32
	}
	for _, accountIndex := range accounts {
		keys, keyErr := a.listAPIKeys(ctx, accountIndex)
		if keyErr != nil {
			return 0, 0, keyErr
		}
		index, matchErr := corex.MatchLighterAPIKeyIndex(want, keys)
		if matchErr != nil {
			continue
		}
		matches = append(matches, struct {
			account int64
			key     int32
		}{accountIndex, index})
	}
	if len(matches) != 1 {
		return 0, 0, fmt.Errorf("%w: Lighter API wallet not uniquely matched", ErrRejected)
	}
	return matches[0].account, matches[0].key, nil
}

func (a *lighterAdapter) accountsByL1(ctx context.Context, address string) ([]int64, error) {
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
	_, err := a.client.do(ctx, "GET", "/api/v1/account?"+query.Encode(), nil, nil, &payload)
	if err != nil {
		return nil, err
	}
	if code, err := payload.Code.Int64(); err == nil && code != 0 && code != 200 {
		return nil, fmt.Errorf("%w: Lighter account: %s", ErrRejected, strings.TrimSpace(payload.Message))
	}
	rows := payload.Accounts
	if len(rows) == 0 && payload.Account != nil {
		rows = append(rows, *payload.Account)
	}
	result := make([]int64, 0, len(rows))
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
		result = append(result, index)
	}
	return result, nil
}

func (a *lighterAdapter) listAPIKeys(ctx context.Context, accountIndex int64) ([]corex.LighterListedKey, error) {
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
	_, err := a.client.do(ctx, "GET", "/api/v1/apikeys?"+query.Encode(), nil, nil, &payload)
	if err != nil {
		return nil, err
	}
	if code, err := payload.Code.Int64(); err == nil && code != 0 && code != 200 {
		return nil, fmt.Errorf("%w: Lighter apikeys: %s", ErrRejected, strings.TrimSpace(payload.Message))
	}
	result := make([]corex.LighterListedKey, 0, len(payload.APIKeys))
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
		result = append(result, corex.LighterListedKey{Index: int32(index), PublicKey: key.PublicKey})
	}
	return result, nil
}
