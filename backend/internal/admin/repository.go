package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrBrokerNotFound = errors.New("broker not found")
	ErrBrokerHasAccounts = errors.New("broker still has platform accounts")
	ErrInvalidBrokerSecret = errors.New("invalid client credentials")
	ErrBrokerDisabled = errors.New("broker is disabled")
)

type Broker struct {
	BrokerID           string
	Name               string
	ClientSecretHash   string
	ClientSecretMasked string
	Status             string
	TokenVersion       int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type AuditEvent struct {
	ID            int64
	ActorUsername string
	ActorID       string
	Action        string
	ResourceType  string
	ResourceID    string
	CreatedAt     time.Time
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) CreateBroker(ctx context.Context, name, hash, masked string) (Broker, error) {
	id, err := newBrokerID()
	if err != nil {
		return Broker{}, err
	}
	return scanBroker(r.pool.QueryRow(ctx, `
		INSERT INTO brokers (broker_id, name, client_secret_hash, client_secret_masked, status, token_version)
		VALUES ($1, $2, $3, $4, 'active', 1)
		RETURNING broker_id, name, client_secret_hash, client_secret_masked, status, token_version, created_at, updated_at`,
		id, name, hash, masked))
}

func (r *Repository) ListBrokers(ctx context.Context) ([]Broker, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT broker_id, name, client_secret_hash, client_secret_masked, status, token_version, created_at, updated_at
		FROM brokers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Broker, 0)
	for rows.Next() {
		broker, err := scanBroker(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, broker)
	}
	return items, rows.Err()
}

func (r *Repository) GetBroker(ctx context.Context, brokerID string) (Broker, error) {
	return scanBroker(r.pool.QueryRow(ctx, `
		SELECT broker_id, name, client_secret_hash, client_secret_masked, status, token_version, created_at, updated_at
		FROM brokers WHERE broker_id=$1`, brokerID))
}

func (r *Repository) SetBrokerStatus(ctx context.Context, brokerID, status string, bump bool) (Broker, error) {
	query := `UPDATE brokers SET status=$2, updated_at=now()`
	if bump {
		query += `, token_version=token_version+1`
	}
	query += ` WHERE broker_id=$1 RETURNING broker_id, name, client_secret_hash, client_secret_masked, status, token_version, created_at, updated_at`
	return scanBroker(r.pool.QueryRow(ctx, query, brokerID, status))
}

func (r *Repository) ResetSecret(ctx context.Context, brokerID, hash, masked string) (Broker, error) {
	return scanBroker(r.pool.QueryRow(ctx, `
		UPDATE brokers
		SET client_secret_hash=$2, client_secret_masked=$3, token_version=token_version+1, updated_at=now()
		WHERE broker_id=$1
		RETURNING broker_id, name, client_secret_hash, client_secret_masked, status, token_version, created_at, updated_at`,
		brokerID, hash, masked))
}

func (r *Repository) DeleteBroker(ctx context.Context, brokerID string) error {
	var count int
	if err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM accounts WHERE broker_id=$1`, brokerID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ErrBrokerHasAccounts
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM brokers WHERE broker_id=$1`, brokerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBrokerNotFound
	}
	return nil
}

func (r *Repository) WriteAudit(ctx context.Context, actorUsername, action, resourceType, resourceID string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO admin_audit_events (actor_username, action, resource_type, resource_id, details)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, actorUsername, action, resourceType, resourceID, string(payload))
	return err
}

func (r *Repository) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, actor_username, actor_id, action, resource_type, resource_id, created_at
		FROM admin_audit_events ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AuditEvent, 0)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.ActorUsername, &event.ActorID, &event.Action, &event.ResourceType, &event.ResourceID, &event.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, event)
	}
	return items, rows.Err()
}

func scanBroker(row interface{ Scan(dest ...any) error }) (Broker, error) {
	var broker Broker
	err := row.Scan(
		&broker.BrokerID, &broker.Name, &broker.ClientSecretHash, &broker.ClientSecretMasked,
		&broker.Status, &broker.TokenVersion, &broker.CreatedAt, &broker.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Broker{}, ErrBrokerNotFound
	}
	if err != nil {
		return Broker{}, err
	}
	return broker, nil
}

func newBrokerID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "brk_" + hex.EncodeToString(raw), nil
}

func randomSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

type EquitySnapshotView struct {
	OwnerUsername string
	BrokerID      string
	VenueIdentity string
	EquityUSD     string
	Status        string
	ObservedAt    time.Time
}

func (r *Repository) ListEquitySnapshots(ctx context.Context) ([]EquitySnapshotView, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT t.owner_username, COALESCE(a.broker_id, ''), s.venue_identity,
		       COALESCE(s.equity_usd, ''), s.status, COALESCE(s.observed_at, to_timestamp(0))
		FROM trading_account_equity_snapshots s
		JOIN trading_accounts t ON t.id = s.trading_account_id
		JOIN accounts a ON a.username = t.owner_username
		WHERE a.permission <> 'admin' AND a.status <> 'archived'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]EquitySnapshotView, 0)
	for rows.Next() {
		var row EquitySnapshotView
		if err := rows.Scan(&row.OwnerUsername, &row.BrokerID, &row.VenueIdentity, &row.EquityUSD, &row.Status, &row.ObservedAt); err != nil {
			return nil, err
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

func maskSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) < 8 {
		return "********"
	}
	return secret[:4] + "..." + secret[len(secret)-4:]
}
