package account

import (
	"bytes"
	"testing"
)

func TestCredentialCipherRoundTrip(t *testing.T) {
	cipher, err := NewCredentialCipher("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("super-secret")) {
		t.Fatalf("plaintext leaked into ciphertext")
	}
	plain, err := cipher.Decrypt(encrypted)
	if err != nil || plain != "super-secret" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}

func TestCredentialCipherRejectsTamperedPayload(t *testing.T) {
	cipher, err := NewCredentialCipher("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("value")
	if err != nil {
		t.Fatal(err)
	}
	encrypted[len(encrypted)-1] ^= 0x01
	if _, err := cipher.Decrypt(encrypted); err != ErrInvalidCiphertext {
		t.Fatalf("err=%v", err)
	}
}

func TestCredentialCipherAcceptsHexKey(t *testing.T) {
	key := "6380555196bd019ca3a367fa5c7c124f06a3abc8738eba32e6131e425aa75cd5"
	cipher, err := NewCredentialCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("ok")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.Decrypt(encrypted)
	if err != nil || plain != "ok" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}

func TestMaskAPIKey(t *testing.T) {
	if got := MaskAPIKey("abcdefghijklmnop"); got != "abcd********mnop" {
		t.Fatalf("got=%q", got)
	}
	if got := MaskAPIKey("short"); got != "*****" {
		t.Fatalf("got=%q", got)
	}
}
