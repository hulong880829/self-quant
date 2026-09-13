package exchange

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/elliottech/lighter-go/signer"
)

type LighterListedKey struct {
	Index     int32
	PublicKey string
}

func NormalizeLighterPrivateKey(value string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	switch len(trimmed) {
	case 64, 80:
		if _, err := hex.DecodeString(trimmed); err != nil {
			return "", fmt.Errorf("invalid Lighter private key")
		}
		return "0x" + strings.ToLower(trimmed), nil
	default:
		return "", fmt.Errorf("invalid Lighter private key length")
	}
}

func LighterKeyManager(secret string) (signer.KeyManager, error) {
	normalized, err := NormalizeLighterPrivateKey(secret)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimPrefix(normalized, "0x")
	bytes, err := hex.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	if len(bytes) == 32 {
		return signer.NewSeedKeyManager(raw)
	}
	return signer.NewKeyManager(bytes)
}

func LighterPublicKeyHex(secret string) (string, error) {
	manager, err := LighterKeyManager(secret)
	if err != nil {
		return "", err
	}
	pub := manager.PubKeyBytes()
	return "0x" + hex.EncodeToString(pub[:]), nil
}

func LighterTxPrivateKey(secret string) (string, error) {
	manager, err := LighterKeyManager(secret)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(manager.PrvKeyBytes()), nil
}

func MatchLighterAPIKeyIndex(want string, keys []LighterListedKey) (int32, error) {
	normalizedWant := normalizeLighterPubKey(want)
	if normalizedWant == "" {
		return 0, fmt.Errorf("empty Lighter public key")
	}
	var matched []int32
	seen := map[int32]struct{}{}
	for _, key := range keys {
		if normalizeLighterPubKey(key.PublicKey) != normalizedWant {
			continue
		}
		if _, ok := seen[key.Index]; ok {
			continue
		}
		seen[key.Index] = struct{}{}
		matched = append(matched, key.Index)
	}
	if len(matched) != 1 {
		return 0, fmt.Errorf("lighter api wallet not uniquely matched")
	}
	return matched[0], nil
}

func normalizeLighterPubKey(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "0x"))
}
