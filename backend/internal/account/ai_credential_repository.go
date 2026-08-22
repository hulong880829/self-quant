package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrAICredentialNotFound = errors.New("ai credential not found")

type AICredentialRecord struct {
	ID            int64
	OwnerUsername string
	Provider      string
	APIKeyEnc     []byte
	APIKeyMasked  string
	Status        string
	LastError     string
	LastTestedAt  time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type AICredentialRepository struct {
	pool *pgxpool.Pool
}

func NewAICredentialRepository(pool *pgxpool.Pool) *AICredentialRepository {
	return &AICredentialRepository{pool: pool}
}

func (r *AICredentialRepository) GetByOwnerProvider(
	ctx context.Context,
	ownerUsername string,
	provider string,
) (AICredentialRecord, error) {
	return scanAICredential(r.pool.QueryRow(ctx, `
		SELECT id, owner_username, provider, api_key_enc, api_key_masked, status,
		       COALESCE(last_error, ''), last_tested_at, created_at, updated_at
		FROM ai_provider_credentials
		WHERE owner_username=$1 AND provider=$2`,
		ownerUsername, provider,
	))
}

func (r *AICredentialRepository) Upsert(
	ctx context.Context,
	record AICredentialRecord,
) (AICredentialRecord, error) {
	return scanAICredential(r.pool.QueryRow(ctx, `
		INSERT INTO ai_provider_credentials (
			owner_username, provider, api_key_enc, api_key_masked, status
		) VALUES ($1, $2, $3, $4, 'unknown')
		ON CONFLICT (owner_username, provider) DO UPDATE SET
			api_key_enc=EXCLUDED.api_key_enc,
			api_key_masked=EXCLUDED.api_key_masked,
			status='unknown',
			last_error=NULL,
			last_tested_at=NULL,
			updated_at=now()
		RETURNING id, owner_username, provider, api_key_enc, api_key_masked, status,
		          COALESCE(last_error, ''), last_tested_at, created_at, updated_at`,
		record.OwnerUsername, record.Provider, record.APIKeyEnc, record.APIKeyMasked,
	))
}

func (r *AICredentialRepository) UpdateStatus(
	ctx context.Context,
	ownerUsername string,
	provider string,
	status string,
	lastError string,
) (AICredentialRecord, error) {
	return scanAICredential(r.pool.QueryRow(ctx, `
		UPDATE ai_provider_credentials
		SET status=$3,
		    last_error=NULLIF($4, ''),
		    last_tested_at=now(),
		    updated_at=now()
		WHERE owner_username=$1 AND provider=$2
		RETURNING id, owner_username, provider, api_key_enc, api_key_masked, status,
		          COALESCE(last_error, ''), last_tested_at, created_at, updated_at`,
		ownerUsername, provider, status, lastError,
	))
}

func (r *AICredentialRepository) DeleteByOwnerProvider(
	ctx context.Context,
	ownerUsername string,
	provider string,
) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM ai_provider_credentials
		WHERE owner_username=$1 AND provider=$2`, ownerUsername, provider)
	if err != nil {
		return fmt.Errorf("delete ai credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAICredentialNotFound
	}
	return nil
}

type aiCredentialRow interface {
	Scan(...any) error
}

func scanAICredential(row aiCredentialRow) (AICredentialRecord, error) {
	var record AICredentialRecord
	var lastTestedAt *time.Time
	err := row.Scan(
		&record.ID,
		&record.OwnerUsername,
		&record.Provider,
		&record.APIKeyEnc,
		&record.APIKeyMasked,
		&record.Status,
		&record.LastError,
		&lastTestedAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AICredentialRecord{}, ErrAICredentialNotFound
	}
	if err != nil {
		return AICredentialRecord{}, fmt.Errorf("scan ai credential: %w", err)
	}
	if lastTestedAt != nil {
		record.LastTestedAt = *lastTestedAt
	}
	return record, nil
}
