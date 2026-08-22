package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PolymarketCredentialRecord struct {
	TradingAccountID int64
	OwnerUsername    string
	AccountName      string
	SignerAddress    string
	FunderAddress    string
	WalletType       string
	SignatureType    int32
	CredentialNonce  uint64
	PrivateKeyEnc    []byte
	APIKeyEnc        []byte
	APISecretEnc     []byte
	PassphraseEnc    []byte
	BindingStatus    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type polymarketCredentialStore interface {
	Create(
		context.Context,
		TradingAccountRecord,
		PolymarketCredentialRecord,
	) (TradingAccountRecord, PolymarketCredentialRecord, error)
	ListMetadataByOwner(context.Context, string) ([]PolymarketCredentialRecord, error)
	GetByOwner(context.Context, string, int64) (PolymarketCredentialRecord, error)
	UpdateCredentialsByOwner(context.Context, string, int64, []byte, []byte, []byte, string) error
	UpdateBindingStatusByOwner(context.Context, string, int64, string) error
}

type PolymarketCredentialRepository struct {
	pool *pgxpool.Pool
}

func NewPolymarketCredentialRepository(pool *pgxpool.Pool) *PolymarketCredentialRepository {
	return &PolymarketCredentialRepository{pool: pool}
}

func (r *PolymarketCredentialRepository) Create(
	ctx context.Context,
	account TradingAccountRecord,
	credentials PolymarketCredentialRecord,
) (TradingAccountRecord, PolymarketCredentialRecord, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TradingAccountRecord{}, PolymarketCredentialRecord{}, err
	}
	defer tx.Rollback(ctx)

	var created TradingAccountRecord
	err = tx.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name,
			api_key_enc, api_secret_enc, passphrase_enc
		) VALUES ($1,$2,'polymarket',$3,$4,$5,$6)
		RETURNING id, owner_username, product_name, exchange, account_name,
		          api_key_enc, api_secret_enc, passphrase_enc, created_at, updated_at`,
		account.OwnerUsername,
		account.ProductName,
		account.AccountName,
		account.APIKeyEnc,
		account.APISecretEnc,
		account.PassphraseEnc,
	).Scan(
		&created.ID,
		&created.OwnerUsername,
		&created.ProductName,
		&created.Exchange,
		&created.AccountName,
		&created.APIKeyEnc,
		&created.APISecretEnc,
		&created.PassphraseEnc,
		&created.CreatedAt,
		&created.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return TradingAccountRecord{}, PolymarketCredentialRecord{}, ErrDuplicateTradingAccount
		}
		return TradingAccountRecord{}, PolymarketCredentialRecord{}, fmt.Errorf("create polymarket account: %w", err)
	}

	var stored PolymarketCredentialRecord
	err = tx.QueryRow(ctx, `
		INSERT INTO polymarket_account_credentials (
			trading_account_id, signer_address, funder_address, wallet_type,
			signature_type, credential_nonce, private_key_enc, api_key_enc,
			api_secret_enc, passphrase_enc, binding_status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active')
		RETURNING trading_account_id, signer_address, funder_address, wallet_type,
		          signature_type, credential_nonce, private_key_enc, api_key_enc,
		          api_secret_enc, passphrase_enc, binding_status, created_at, updated_at`,
		created.ID,
		credentials.SignerAddress,
		credentials.FunderAddress,
		credentials.WalletType,
		credentials.SignatureType,
		credentials.CredentialNonce,
		credentials.PrivateKeyEnc,
		credentials.APIKeyEnc,
		credentials.APISecretEnc,
		credentials.PassphraseEnc,
	).Scan(
		&stored.TradingAccountID,
		&stored.SignerAddress,
		&stored.FunderAddress,
		&stored.WalletType,
		&stored.SignatureType,
		&stored.CredentialNonce,
		&stored.PrivateKeyEnc,
		&stored.APIKeyEnc,
		&stored.APISecretEnc,
		&stored.PassphraseEnc,
		&stored.BindingStatus,
		&stored.CreatedAt,
		&stored.UpdatedAt,
	)
	if err != nil {
		return TradingAccountRecord{}, PolymarketCredentialRecord{}, fmt.Errorf("store polymarket credentials: %w", err)
	}
	stored.OwnerUsername = created.OwnerUsername
	stored.AccountName = created.AccountName
	if err := tx.Commit(ctx); err != nil {
		return TradingAccountRecord{}, PolymarketCredentialRecord{}, err
	}
	return created, stored, nil
}

func (r *PolymarketCredentialRepository) ListMetadataByOwner(
	ctx context.Context,
	owner string,
) ([]PolymarketCredentialRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.trading_account_id, t.owner_username, t.account_name,
		       p.signer_address, p.funder_address, p.wallet_type, p.signature_type,
		       p.credential_nonce, p.binding_status, p.created_at, p.updated_at
		FROM polymarket_account_credentials p
		JOIN trading_accounts t ON t.id=p.trading_account_id
		WHERE t.owner_username=$1`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PolymarketCredentialRecord, 0)
	for rows.Next() {
		var item PolymarketCredentialRecord
		if err := rows.Scan(
			&item.TradingAccountID,
			&item.OwnerUsername,
			&item.AccountName,
			&item.SignerAddress,
			&item.FunderAddress,
			&item.WalletType,
			&item.SignatureType,
			&item.CredentialNonce,
			&item.BindingStatus,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *PolymarketCredentialRepository) GetByOwner(
	ctx context.Context,
	owner string,
	accountID int64,
) (PolymarketCredentialRecord, error) {
	var item PolymarketCredentialRecord
	err := r.pool.QueryRow(ctx, `
		SELECT p.trading_account_id, t.owner_username, t.account_name,
		       p.signer_address, p.funder_address, p.wallet_type, p.signature_type,
		       p.credential_nonce, p.private_key_enc, p.api_key_enc,
		       p.api_secret_enc, p.passphrase_enc, p.binding_status,
		       p.created_at, p.updated_at
		FROM polymarket_account_credentials p
		JOIN trading_accounts t ON t.id=p.trading_account_id
		WHERE t.owner_username=$1 AND p.trading_account_id=$2`,
		owner, accountID,
	).Scan(
		&item.TradingAccountID,
		&item.OwnerUsername,
		&item.AccountName,
		&item.SignerAddress,
		&item.FunderAddress,
		&item.WalletType,
		&item.SignatureType,
		&item.CredentialNonce,
		&item.PrivateKeyEnc,
		&item.APIKeyEnc,
		&item.APISecretEnc,
		&item.PassphraseEnc,
		&item.BindingStatus,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolymarketCredentialRecord{}, ErrTradingAccountNotFound
	}
	return item, err
}

func (r *PolymarketCredentialRepository) UpdateCredentialsByOwner(
	ctx context.Context,
	owner string,
	accountID int64,
	apiKeyEnc, apiSecretEnc, passphraseEnc []byte,
	status string,
) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
		UPDATE polymarket_account_credentials p
		SET api_key_enc=$3, api_secret_enc=$4, passphrase_enc=$5,
		    binding_status=$6, updated_at=NOW()
		FROM trading_accounts t
		WHERE t.id=p.trading_account_id AND t.owner_username=$1
		  AND p.trading_account_id=$2`,
		owner, accountID, apiKeyEnc, apiSecretEnc, passphraseEnc, status,
	)
	if err != nil {
		return fmt.Errorf("update polymarket credentials: %w", err)
	}
	if command.RowsAffected() == 0 {
		return ErrTradingAccountNotFound
	}
	_, err = tx.Exec(ctx, `
		UPDATE trading_accounts
		SET api_key_enc=$3, api_secret_enc=$4, passphrase_enc=$5, updated_at=NOW()
		WHERE owner_username=$1 AND id=$2`,
		owner, accountID, apiKeyEnc, apiSecretEnc, passphraseEnc,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PolymarketCredentialRepository) UpdateBindingStatusByOwner(
	ctx context.Context,
	owner string,
	accountID int64,
	bindingStatus string,
) error {
	command, err := r.pool.Exec(ctx, `
		UPDATE polymarket_account_credentials p
		SET binding_status=$3, updated_at=NOW()
		FROM trading_accounts t
		WHERE t.id=p.trading_account_id AND t.owner_username=$1
		  AND p.trading_account_id=$2`,
		owner, accountID, bindingStatus,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrTradingAccountNotFound
	}
	return nil
}
