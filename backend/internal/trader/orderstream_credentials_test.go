package trader

import "testing"

func TestToOrderStreamCredentialsPreservesDEXFieldsAndSetIndexes(t *testing.T) {
	t.Parallel()
	accountIndex, apiKeyIndex := int64(0), int32(0)
	got := toOrderStreamCredentials(Credentials{
		APIKey:         "api-key",
		APISecret:      "secret",
		Passphrase:     "passphrase",
		CredentialKind: "lighter_api",
		SigningAddress: "signer",
		VaultAddress:   "vault",
		AccountIndex:   &accountIndex,
		APIKeyIndex:    &apiKeyIndex,
	})
	if got.APIKey != "api-key" || got.Secret != "secret" ||
		got.Passphrase != "passphrase" || got.CredentialKind != "lighter_api" ||
		got.SigningAddress != "signer" || got.VaultAddress != "vault" ||
		got.AccountIndex == nil || *got.AccountIndex != 0 ||
		got.APIKeyIndex == nil || *got.APIKeyIndex != 0 {
		t.Fatalf("credentials=%+v", got)
	}
}
