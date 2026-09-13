package ranking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	RankingSnapshotSchemaVersion = 1
	CurrentModelVersion          = "conditional_replay_v3_cost_12bps"
)

type SnapshotPersistence interface {
	Load(context.Context, Period, string) (*Snapshot, error)
	Upsert(context.Context, Snapshot, string) error
}

type PostgresSnapshotPersistence struct {
	pool *pgxpool.Pool
}

type persistedSnapshotPayload struct {
	Items            []Opportunity  `json:"items"`
	RejectionSummary map[string]int `json:"rejectionSummary,omitempty"`
}

func NewPostgresSnapshotPersistence(pool *pgxpool.Pool) *PostgresSnapshotPersistence {
	return &PostgresSnapshotPersistence{pool: pool}
}

func (p *PostgresSnapshotPersistence) Load(
	ctx context.Context, period Period, modelVersion string,
) (*Snapshot, error) {
	if p == nil || p.pool == nil {
		return nil, errors.New("ranking snapshot persistence is unavailable")
	}
	var (
		schemaVersion int
		storedModel   string
		generation    int64
		payload       []byte
		calculatedAt  time.Time
		successfulAt  time.Time
		dataThrough   time.Time
	)
	err := p.pool.QueryRow(ctx, `
		SELECT schema_version, model_version, generation, payload,
		       calculated_at, last_successful_at, data_through
		FROM funding_ranking_snapshots
		WHERE period=$1`, string(period),
	).Scan(
		&schemaVersion, &storedModel, &generation, &payload,
		&calculatedAt, &successfulAt, &dataThrough,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load funding ranking snapshot %s: %w", period, err)
	}
	if schemaVersion != RankingSnapshotSchemaVersion || storedModel != modelVersion {
		return nil, nil
	}
	var decoded persistedSnapshotPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode funding ranking snapshot %s: %w", period, err)
	}
	snapshot := &Snapshot{
		Period: period, Items: decoded.Items, Generation: uint64(generation),
		Status: SnapshotStale, CalculatedAt: calculatedAt.UTC(),
		LastSuccessfulAt: successfulAt.UTC(), DataThrough: dataThrough.UTC(), Stale: true,
		RejectionSummary: cloneRejections(decoded.RejectionSummary),
	}
	if err := validatePersistedSnapshot(*snapshot, modelVersion); err != nil {
		return nil, fmt.Errorf("validate funding ranking snapshot %s: %w", period, err)
	}
	return snapshot, nil
}

func (p *PostgresSnapshotPersistence) Upsert(
	ctx context.Context, snapshot Snapshot, modelVersion string,
) error {
	if p == nil || p.pool == nil {
		return errors.New("ranking snapshot persistence is unavailable")
	}
	if snapshot.LastSuccessfulAt.IsZero() {
		return errors.New("ranking snapshot has no successful calculation")
	}
	if err := validatePersistedSnapshot(snapshot, modelVersion); err != nil {
		return err
	}
	payload, err := json.Marshal(persistedSnapshotPayload{
		Items:            cloneOpportunities(snapshot.Items),
		RejectionSummary: cloneRejections(snapshot.RejectionSummary),
	})
	if err != nil {
		return fmt.Errorf("encode funding ranking snapshot %s: %w", snapshot.Period, err)
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO funding_ranking_snapshots (
			period, schema_version, model_version, generation, payload,
			calculated_at, last_successful_at, data_through
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (period, schema_version, model_version) DO UPDATE
		SET generation=EXCLUDED.generation,
		    payload=EXCLUDED.payload,
		    calculated_at=EXCLUDED.calculated_at,
		    last_successful_at=EXCLUDED.last_successful_at,
		    data_through=EXCLUDED.data_through,
		    updated_at=now()
		WHERE funding_ranking_snapshots.generation <= EXCLUDED.generation`,
		string(snapshot.Period), RankingSnapshotSchemaVersion, modelVersion,
		int64(snapshot.Generation), payload, snapshot.CalculatedAt,
		snapshot.LastSuccessfulAt, snapshot.DataThrough,
	)
	if err != nil {
		return fmt.Errorf("persist funding ranking snapshot %s: %w", snapshot.Period, err)
	}
	return nil
}

func validatePersistedSnapshot(snapshot Snapshot, modelVersion string) error {
	if snapshot.Period != Period8h && snapshot.Period != Period24h {
		return ErrInvalidPeriod
	}
	if len(snapshot.Items) > 100 {
		return fmt.Errorf("snapshot contains %d items, maximum is 100", len(snapshot.Items))
	}
	if snapshot.Generation > math.MaxInt64 {
		return errors.New("snapshot generation exceeds PostgreSQL bigint")
	}
	for index, item := range snapshot.Items {
		if item.Period != snapshot.Period {
			return fmt.Errorf("item %d period does not match snapshot", index)
		}
		if item.ModelState != "" && item.ModelState != modelVersion {
			return fmt.Errorf("item %d model version is incompatible", index)
		}
		if item.GlobalSymbol == "" || item.Long.Exchange == "" || item.Short.Exchange == "" ||
			!finite(item.PeriodExpectedReturn) ||
			!finite(item.CombinedExpectedAnnualized) ||
			!finite(item.ProfitProbability) || !finite(item.P5Return) ||
			!finite(item.Coverage) || !finite(item.Confidence) {
			return fmt.Errorf("item %d is invalid", index)
		}
	}
	return nil
}
