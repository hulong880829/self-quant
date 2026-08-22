package account

import "testing"

func TestCheckPasswordAcceptsMatchingHash(t *testing.T) {
	hash, err := HashPassword("admin123")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckPassword(hash, "admin123"); err != nil {
		t.Fatal(err)
	}
	if err := CheckPassword(hash, "wrong"); err != ErrInvalidCredentials {
		t.Fatalf("err=%v", err)
	}
}
