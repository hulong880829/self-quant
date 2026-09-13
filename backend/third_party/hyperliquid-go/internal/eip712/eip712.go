// Package eip712 holds the low-level EIP-712 helpers used by the public
// signing surface. Symbols here are exported so the root package can call
// them, but the package itself is internal and not part of the public API.
package eip712

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/vmihailenco/msgpack/v5"
)

// SignatureResult is the {r, s, v} triple produced by every signing call.
type SignatureResult struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

// CachedDomainSeparator is the EIP-712 domain separator for the L1 phantom-agent
// scheme. It is computed once at init and reused for every L1 signature.
var CachedDomainSeparator []byte

// EIP712Types is the static type schema shared by every L1 signature.
var EIP712Types = apitypes.Types{
	"Agent": []apitypes.Type{
		{Name: "source", Type: "string"},
		{Name: "connectionId", Type: "bytes32"},
	},
	"EIP712Domain": []apitypes.Type{
		{Name: "name", Type: "string"},
		{Name: "version", Type: "string"},
		{Name: "chainId", Type: "uint256"},
		{Name: "verifyingContract", Type: "address"},
	},
}

// EIP712Domain is the L1-action EIP-712 domain.
var EIP712Domain apitypes.TypedDataDomain

func init() {
	chainID := math.HexOrDecimal256(*big.NewInt(1337))
	EIP712Domain = apitypes.TypedDataDomain{
		ChainId:           &chainID,
		Name:              "Exchange",
		Version:           "1",
		VerifyingContract: "0x0000000000000000000000000000000000000000",
	}

	td := apitypes.TypedData{
		Domain:      EIP712Domain,
		Types:       EIP712Types,
		PrimaryType: "Agent",
		Message:     map[string]any{"source": "a", "connectionId": "0x" + strings.Repeat("00", 32)},
	}
	domainSep, err := td.HashStruct("EIP712Domain", td.Domain.Map())
	if err != nil {
		panic(fmt.Sprintf("failed to compute domain separator: %v", err))
	}
	CachedDomainSeparator = domainSep
}

// ConvertStr16ToStr8 is currently a no-op: vmihailenco/msgpack v5 with
// UseCompactInts already emits str8 for strings 32 to 255 bytes, matching
// Python's eth_account msgpack output. Kept as a hook in case a future
// dependency upgrade reintroduces the str16 mismatch.
func ConvertStr16ToStr8(data []byte) []byte {
	return data
}

// HashStructLenient is HashStruct that silently drops message fields not
// declared in payloadTypes, mirroring Python eth_account's behaviour. It also
// normalises uint64 fields to *big.Int, which apitypes.HashStruct requires.
func HashStructLenient(td apitypes.TypedData, primaryType string, msg map[string]any) ([]byte, error) {
	types := td.Types[primaryType]
	filtered := make(map[string]any, len(types))
	for _, t := range types {
		v, ok := msg[t.Name]
		if !ok {
			return nil, fmt.Errorf("missing field %q for primary type %q", t.Name, primaryType)
		}
		if t.Type == "uint64" {
			n, err := toBigUint64(v, t.Name)
			if err != nil {
				return nil, err
			}
			filtered[t.Name] = n
			continue
		}
		filtered[t.Name] = v
	}
	return td.HashStruct(primaryType, filtered)
}

func toBigUint64(v any, fieldName string) (*big.Int, error) {
	switch x := v.(type) {
	case *big.Int:
		return x, nil
	case uint64:
		return new(big.Int).SetUint64(x), nil
	case int64:
		if x < 0 {
			return nil, fmt.Errorf("%s: negative int64 %d", fieldName, x)
		}
		return new(big.Int).SetUint64(uint64(x)), nil
	case int:
		if x < 0 {
			return nil, fmt.Errorf("%s: negative int %d", fieldName, x)
		}
		return new(big.Int).SetUint64(uint64(x)), nil
	case float64:
		if x < 0 || x > 1<<63 || float64(uint64(x)) != x {
			return nil, fmt.Errorf("%s: float64 %g not a valid uint64", fieldName, x)
		}
		return new(big.Int).SetUint64(uint64(x)), nil
	case string:
		n, ok := new(big.Int).SetString(x, 10)
		if !ok {
			return nil, fmt.Errorf("%s: cannot parse string %q as uint64", fieldName, x)
		}
		return n, nil
	}
	return nil, fmt.Errorf("%s: unsupported type %T for uint64", fieldName, v)
}

// AddressToBytes decodes a hex address to bytes, matching Python's address_to_bytes.
func AddressToBytes(address string) []byte {
	address = strings.TrimPrefix(address, "0x")
	b, _ := hex.DecodeString(address)
	return b
}

// ActionHash implements Python's action_hash: msgpack-encode the action then
// append nonce, vault, and optional expiresAfter, then keccak256.
func ActionHash(action any, vaultAddress string, nonce int64, expiresAfter *int64) []byte {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.SetSortMapKeys(true)
	enc.UseCompactInts(true)

	if err := enc.Encode(action); err != nil {
		panic(fmt.Sprintf("failed to marshal action: %v", err))
	}
	data := ConvertStr16ToStr8(buf.Bytes())

	if nonce < 0 {
		panic(fmt.Sprintf("nonce cannot be negative: %d", nonce))
	}
	nonceBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(nonceBytes, uint64(nonce))
	data = append(data, nonceBytes...)

	if vaultAddress == "" {
		data = append(data, 0x00)
	} else {
		data = append(data, 0x01)
		data = append(data, AddressToBytes(vaultAddress)...)
	}

	if expiresAfter != nil {
		if *expiresAfter < 0 {
			panic(fmt.Sprintf("expiresAfter cannot be negative: %d", *expiresAfter))
		}
		data = append(data, 0x00)
		expiresAfterBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(expiresAfterBytes, uint64(*expiresAfter))
		data = append(data, expiresAfterBytes...)
	}

	return crypto.Keccak256(data)
}

// ConstructPhantomAgent matches Python's construct_phantom_agent.
func ConstructPhantomAgent(hash []byte, isMainnet bool) map[string]any {
	source := "b"
	if isMainnet {
		source = "a"
	}
	return map[string]any{
		"source":       source,
		"connectionId": "0x" + hex.EncodeToString(hash),
	}
}

// L1Payload builds the EIP-712 typed-data envelope for an L1 phantom-agent action.
func L1Payload(phantomAgent map[string]any) apitypes.TypedData {
	return apitypes.TypedData{
		Domain:      EIP712Domain,
		Types:       EIP712Types,
		PrimaryType: "Agent",
		Message:     phantomAgent,
	}
}

// SignInner signs a typed-data envelope against the cached L1 domain separator.
func SignInner(privateKey *ecdsa.PrivateKey, typedData apitypes.TypedData) (SignatureResult, error) {
	typedDataHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to hash typed data: %w", err)
	}

	var rawData [66]byte
	rawData[0] = 0x19
	rawData[1] = 0x01
	copy(rawData[2:34], CachedDomainSeparator)
	copy(rawData[34:66], typedDataHash)
	msgHash := crypto.Keccak256Hash(rawData[:])

	signature, err := crypto.Sign(msgHash.Bytes(), privateKey)
	if err != nil {
		return SignatureResult{}, fmt.Errorf("failed to sign message: %w", err)
	}

	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:64])
	v := int(signature[64]) + 27

	return SignatureResult{
		R: hexutil.EncodeBig(r),
		S: hexutil.EncodeBig(s),
		V: v,
	}, nil
}
