package signing

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/Simon-Busch/hyperliquid-go/internal/eip712"
)

// SignatureResult is the {r, s, v} triple produced by every signing call.
type SignatureResult = eip712.SignatureResult

// userSignedChainIDHex is the wallet-side chainId Hyperliquid prescribes for
// user-signed actions ("0x66eee" = 421614, Arbitrum Sepolia). The same value
// is used on mainnet and testnet — hyperliquidChain distinguishes them.
const userSignedChainIDHex = "0x66eee"

var userSignedChainID = func() *big.Int {
	v, _ := new(big.Int).SetString(strings.TrimPrefix(userSignedChainIDHex, "0x"), 16)
	return v
}()

// SignUserSignedAction signs an EIP-712 user-signed action using the
// "HyperliquidSignTransaction" domain (chainId 421614). Unlike L1 actions,
// these are not msgpack-hashed; the action map is hashed directly per its
// EIP-712 typed-data schema. Caller passes the per-action payloadTypes and
// primary type name (e.g. "HyperliquidTransaction:UsdClassTransfer").
//
// Mutates action by adding hyperliquidChain + signatureChainId so the JSON
// sent to /exchange matches what was signed (Python SDK does the same).
func SignUserSignedAction(
	privateKey *ecdsa.PrivateKey,
	action map[string]any,
	payloadTypes []apitypes.Type,
	primaryType string,
	isMainnet bool,
) (SignatureResult, error) {
	if isMainnet {
		action["hyperliquidChain"] = "Mainnet"
	} else {
		action["hyperliquidChain"] = "Testnet"
	}
	action["signatureChainId"] = userSignedChainIDHex

	chainID := math.HexOrDecimal256(*userSignedChainID)
	td := apitypes.TypedData{
		Domain: apitypes.TypedDataDomain{
			ChainId:           &chainID,
			Name:              "HyperliquidSignTransaction",
			Version:           "1",
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Types: apitypes.Types{
			primaryType: payloadTypes,
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
		},
		PrimaryType: primaryType,
		Message:     action,
	}

	domainSep, err := td.HashStruct("EIP712Domain", td.Domain.Map())
	if err != nil {
		return SignatureResult{}, fmt.Errorf("hash domain: %w", err)
	}
	msgHash, err := eip712.HashStructLenient(td, primaryType, action)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("hash message: %w", err)
	}

	var raw [66]byte
	raw[0] = 0x19
	raw[1] = 0x01
	copy(raw[2:34], domainSep)
	copy(raw[34:66], msgHash)
	digest := crypto.Keccak256Hash(raw[:])

	sig, err := crypto.Sign(digest.Bytes(), privateKey)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("sign: %w", err)
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:64])
	return SignatureResult{
		R: hexutil.EncodeBig(r),
		S: hexutil.EncodeBig(s),
		V: int(sig[64]) + 27,
	}, nil
}

// SignL1Action signs an L1 action via the phantom-agent EIP-712 scheme:
// msgpack-hash the action with nonce/vault/expiresAfter, wrap in an Agent
// typed-data envelope, then sign against the cached domain separator.
func SignL1Action(
	privateKey *ecdsa.PrivateKey,
	action any,
	vaultAddress string,
	timestamp int64,
	expiresAfter *int64,
	isMainnet bool,
) (SignatureResult, error) {
	hash := eip712.ActionHash(action, vaultAddress, timestamp, expiresAfter)
	phantomAgent := eip712.ConstructPhantomAgent(hash, isMainnet)
	typedData := eip712.L1Payload(phantomAgent)
	return eip712.SignInner(privateKey, typedData)
}

// User-signed action payload schemas, copied from the official Python SDK
// (hyperliquid/utils/signing.py). The order of fields must match Python.

var (
	UsdSendSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "destination", Type: "string"},
		{Name: "amount", Type: "string"},
		{Name: "time", Type: "uint64"},
	}
	SpotTransferSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "destination", Type: "string"},
		{Name: "token", Type: "string"},
		{Name: "amount", Type: "string"},
		{Name: "time", Type: "uint64"},
	}
	WithdrawSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "destination", Type: "string"},
		{Name: "amount", Type: "string"},
		{Name: "time", Type: "uint64"},
	}
	UsdClassTransferSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "amount", Type: "string"},
		{Name: "toPerp", Type: "bool"},
		{Name: "nonce", Type: "uint64"},
	}
	SendAssetSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "destination", Type: "string"},
		{Name: "sourceDex", Type: "string"},
		{Name: "destinationDex", Type: "string"},
		{Name: "token", Type: "string"},
		{Name: "amount", Type: "string"},
		{Name: "fromSubAccount", Type: "string"},
		{Name: "nonce", Type: "uint64"},
	}
	TokenDelegateSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "validator", Type: "address"},
		{Name: "wei", Type: "uint64"},
		{Name: "isUndelegate", Type: "bool"},
		{Name: "nonce", Type: "uint64"},
	}
	ConvertToMultiSigUserSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "signers", Type: "string"},
		{Name: "nonce", Type: "uint64"},
	}
	ApproveAgentSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "agentAddress", Type: "address"},
		{Name: "agentName", Type: "string"},
		{Name: "nonce", Type: "uint64"},
	}
	ApproveBuilderFeeSignTypes = []apitypes.Type{
		{Name: "hyperliquidChain", Type: "string"},
		{Name: "maxFeeRate", Type: "string"},
		{Name: "builder", Type: "address"},
		{Name: "nonce", Type: "uint64"},
	}
)

// FloatToUsdInt converts a float USD amount to the integer representation
// expected by Hyperliquid (six decimals for USDC).
func FloatToUsdInt(value float64) int {
	return int(value * 1e6)
}

// GetTimestampMs returns the current Unix time in milliseconds.
func GetTimestampMs() int64 {
	return time.Now().UnixMilli()
}
