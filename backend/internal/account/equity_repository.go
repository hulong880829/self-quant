package account

import (
	"context"
	"fmt"
	"time"
)

func (r *TradingRepository) ListAll(ctx context.Context) ([]TradingAccountRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_username, product_name, exchange, account_name,
		       api_key_enc, api_secret_enc, passphrase_enc,
		       COALESCE(credential_kind,''), trading_api_key_enc, trading_api_secret_enc,
		       COALESCE(signing_address,''), COALESCE(vault_address,''),
		       account_index, api_key_index, created_at, updated_at
		FROM trading_accounts
		ORDER BY exchange ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all trading accounts: %w", err)
	}
	defer rows.Close()
	result := make([]TradingAccountRecord, 0)
	for rows.Next() {
		var record TradingAccountRecord
		if err := rows.Scan(
			&record.ID, &record.OwnerUsername, &record.ProductName, &record.Exchange,
			&record.AccountName, &record.APIKeyEnc, &record.APISecretEnc, &record.PassphraseEnc,
			&record.CredentialKind, &record.TradingAPIKeyEnc, &record.TradingAPISecretEnc,
			&record.SigningAddress, &record.VaultAddress, &record.AccountIndex,
			&record.APIKeyIndex, &record.CreatedAt, &record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan trading account: %w", err)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *TradingRepository) UpsertEquitySnapshot(ctx context.Context, snapshot EquitySnapshotRecord) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO trading_account_equity_snapshots (
			trading_account_id, venue_identity, equity_usd, status, observed_at,
			last_success_at, error_code, error_message, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (trading_account_id) DO UPDATE SET
			venue_identity=EXCLUDED.venue_identity,
			equity_usd=EXCLUDED.equity_usd,
			status=EXCLUDED.status,
			observed_at=EXCLUDED.observed_at,
			last_success_at=COALESCE(EXCLUDED.last_success_at, trading_account_equity_snapshots.last_success_at),
			error_code=EXCLUDED.error_code,
			error_message=EXCLUDED.error_message,
			updated_at=now()`,
		snapshot.TradingAccountID, snapshot.VenueIdentity, snapshot.EquityUSD, snapshot.Status,
		snapshot.ObservedAt, snapshot.LastSuccessAt, snapshot.ErrorCode, snapshot.ErrorMessage,
	)
	if err != nil {
		return fmt.Errorf("upsert equity snapshot: %w", err)
	}
	return nil
}

type EquitySnapshotView struct {
	OwnerUsername string
	BrokerID      string
	VenueIdentity string
	EquityUSD     string
	Status        string
	ObservedAt    time.Time
}
