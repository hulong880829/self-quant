package trader

import "selfquant/backend/internal/trader/orderstream"

func toOrderStreamCredentials(account Credentials) orderstream.Credentials {
	venue := toVenueCredentials(account)
	return orderstream.Credentials{
		APIKey:         venue.APIKey,
		Secret:         venue.APISecret,
		Passphrase:     venue.Passphrase,
		CredentialKind: venue.CredentialKind,
		SigningAddress: venue.SigningAddress,
		VaultAddress:   venue.VaultAddress,
		AccountIndex:   venue.AccountIndex,
		APIKeyIndex:    venue.APIKeyIndex,
	}
}
