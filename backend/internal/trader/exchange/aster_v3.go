package exchange

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	corex "selfquant/backend/internal/exchange"
)

func asterUsesAPIWallet(credentials Credentials) bool {
	switch strings.TrimSpace(credentials.CredentialKind) {
	case "aster_hmac":
		return false
	case "aster_api_wallet", "":
		return common.IsHexAddress(strings.TrimSpace(credentials.APIKey))
	default:
		return false
	}
}

func asterOrderPath(credentials Credentials) string {
	if asterUsesAPIWallet(credentials) {
		return "/fapi/v3/order"
	}
	return "/fapi/v1/order"
}

func asterTradesPath(credentials Credentials) string {
	if asterUsesAPIWallet(credentials) {
		return "/fapi/v3/userTrades"
	}
	return "/fapi/v1/userTrades"
}

func asterPositionsPath(credentials Credentials) string {
	if asterUsesAPIWallet(credentials) {
		return "/fapi/v3/positionRisk"
	}
	return "/fapi/v2/positionRisk"
}

func asterLeveragePath(credentials Credentials) string {
	if asterUsesAPIWallet(credentials) {
		return "/fapi/v3/leverage"
	}
	return "/fapi/v1/leverage"
}

func asterBalancePath(credentials Credentials) string {
	if asterUsesAPIWallet(credentials) {
		return "/fapi/v3/balance"
	}
	return "/fapi/v2/balance"
}

func asterWalletAuth(credentials Credentials) (string, string, *ecdsa.PrivateKey, error) {
	user := strings.TrimSpace(credentials.APIKey)
	if !common.IsHexAddress(user) {
		return "", "", nil, fmt.Errorf("%w: Aster API wallet address is required", ErrRejected)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(credentials.APISecret), "0x"))
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: invalid Aster API wallet key", ErrRejected)
	}
	return common.HexToAddress(user).Hex(), crypto.PubkeyToAddress(key.PublicKey).Hex(), key, nil
}

func (a *asterAdapter) signedV3(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	values url.Values,
) ([]byte, error) {
	user, signer, key, err := asterWalletAuth(credentials)
	if err != nil {
		return nil, err
	}
	params := map[string]string{}
	for name, items := range values {
		if len(items) == 0 || name == "signature" {
			continue
		}
		params[name] = items[0]
	}
	now := a.now().Add(a.currentTimeOffset())
	params["user"] = user
	params["signer"] = signer
	params["nonce"] = strconv.FormatInt(now.UnixMicro(), 10)
	params["timestamp"] = strconv.FormatInt(now.UnixMilli(), 10)
	params["recvWindow"] = "5000"
	message := corex.AsterParamString(params)
	signature, err := corex.SignAsterV3(key, message)
	if err != nil {
		return nil, err
	}
	headers := http.Header{"Accept": {"application/json"}}
	var body []byte
	requestPath := path
	encoded := message + "&signature=" + signature
	if method == http.MethodGet || method == http.MethodDelete {
		requestPath += "?" + encoded
	} else {
		body = []byte(encoded)
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return a.client.do(ctx, method, requestPath, headers, body, nil)
}
