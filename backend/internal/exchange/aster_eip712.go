package exchange

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"net/url"

	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

const (
	AsterSignName    = "AsterSignTransaction"
	AsterSignVersion = "1"
	AsterSignChainID = 1666
)

// AsterParamString sorts keys ASCII-ascending and joins URL-encoded key=value
// with &. The result (without signature) is the EIP-712 Message.msg for API
// Wallet auth, matching Aster V3 urllib.parse.urlencode() before signing.
func AsterParamString(params map[string]string) string {
	values := make(url.Values, len(params))
	for key, value := range params {
		values.Set(key, value)
	}
	return values.Encode()
}

// SignAsterV3 signs the query/body string with AsterSignTransaction / Message.msg.
func SignAsterV3(key *ecdsa.PrivateKey, message string) (string, error) {
	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Message": {
				{Name: "msg", Type: "string"},
			},
		},
		PrimaryType: "Message",
		Domain: apitypes.TypedDataDomain{
			Name:              AsterSignName,
			Version:           AsterSignVersion,
			ChainId:           ethmath.NewHexOrDecimal256(AsterSignChainID),
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Message: apitypes.TypedDataMessage{
			"msg": message,
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		return "", fmt.Errorf("aster typed data: %w", err)
	}
	signature, err := crypto.Sign(hash, key)
	if err != nil {
		return "", fmt.Errorf("aster sign: %w", err)
	}
	signature[64] += 27
	return "0x" + hex.EncodeToString(signature), nil
}
