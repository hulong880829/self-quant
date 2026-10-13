package jev

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"selfquant/backend/internal/config"
)

type Service struct {
	cfg   config.Jev
	store Store
	cache atomic.Pointer[Snapshot]
	now   func() time.Time
}

func NewService(cfg config.Jev, store Store) *Service {
	return &Service{cfg: cfg, store: store, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetSnapshot(snapshot Snapshot) {
	copy := snapshot
	s.cache.Store(&copy)
}

func (s *Service) GetSnapshot(ctx context.Context, ifNoneMatch, strategyVersion string) (*Snapshot, bool, error) {
	version := NormalizeStrategyVersion(strategyVersion)
	if version == StrategyVersion {
		if snapshot := s.cache.Load(); snapshot != nil {
			if matchETag(ifNoneMatch, snapshot.ETag) {
				return snapshot, true, nil
			}
			return snapshot, false, nil
		}
	}
	run, err := s.resolveRun(ctx, version)
	if err != nil {
		return nil, false, err
	}
	if run == nil {
		return nil, false, ErrNoSnapshot
	}
	snapshot, err := s.store.LatestSnapshot(ctx, run.ID)
	if err != nil {
		return nil, false, err
	}
	if snapshot == nil {
		return nil, false, ErrNoSnapshot
	}
	if version == StrategyVersion {
		s.SetSnapshot(*snapshot)
	}
	if matchETag(ifNoneMatch, snapshot.ETag) {
		return snapshot, true, nil
	}
	return snapshot, false, nil
}

func (s *Service) ListPositions(ctx context.Context, limit int, cursor, ifNoneMatch, strategyVersion string) ([]Position, string, string, bool, error) {
	etag, notModified, run, err := s.etagGuard(ctx, ifNoneMatch, strategyVersion)
	if err != nil || notModified {
		return nil, "", etag, notModified, err
	}
	items, next, err := s.store.ListPositionsPage(ctx, run.ID, ClampLimit(limit), ParseCursor(cursor))
	if err != nil {
		return nil, "", etag, false, err
	}
	return items, FormatCursor(next), etag, false, nil
}

func (s *Service) ListDecisions(ctx context.Context, limit int, cursor, ifNoneMatch, strategyVersion string) ([]Decision, string, string, bool, error) {
	etag, notModified, run, err := s.etagGuard(ctx, ifNoneMatch, strategyVersion)
	if err != nil || notModified {
		return nil, "", etag, notModified, err
	}
	items, next, err := s.store.ListDecisionsPage(ctx, run.ID, ClampLimit(limit), ParseCursor(cursor))
	if err != nil {
		return nil, "", etag, false, err
	}
	return items, FormatCursor(next), etag, false, nil
}

func (s *Service) ListTrades(ctx context.Context, limit int, cursor, ifNoneMatch, strategyVersion string) ([]Trade, string, string, bool, error) {
	etag, notModified, run, err := s.etagGuard(ctx, ifNoneMatch, strategyVersion)
	if err != nil || notModified {
		return nil, "", etag, notModified, err
	}
	items, next, err := s.store.ListTradesPage(ctx, run.ID, ClampLimit(limit), ParseCursor(cursor))
	if err != nil {
		return nil, "", etag, false, err
	}
	return items, FormatCursor(next), etag, false, nil
}

func (s *Service) GetEquityCurve(ctx context.Context, rangeValue, ifNoneMatch, strategyVersion string) ([]EquityPoint, string, string, bool, error) {
	etag, notModified, run, err := s.etagGuard(ctx, ifNoneMatch, strategyVersion)
	if err != nil || notModified {
		return nil, rangeValue, etag, notModified, err
	}
	from := EquityFrom(rangeValue, s.now())
	points, err := s.store.ListEquityPoints(ctx, run.ID, from)
	if err != nil {
		return nil, rangeValue, etag, false, err
	}
	return DownsampleEquity(points, MaxEquityPoints), rangeValue, etag, false, nil
}

func (s *Service) etagGuard(ctx context.Context, ifNoneMatch, strategyVersion string) (string, bool, *Run, error) {
	snapshot, notModified, err := s.GetSnapshot(ctx, ifNoneMatch, strategyVersion)
	if err != nil {
		return "", false, nil, err
	}
	if notModified {
		return snapshot.ETag, true, nil, nil
	}
	run, err := s.resolveRun(ctx, strategyVersion)
	if err != nil {
		return snapshot.ETag, false, nil, err
	}
	if run == nil {
		return snapshot.ETag, false, nil, ErrNoSnapshot
	}
	return snapshot.ETag, false, run, nil
}

func (s *Service) resolveRun(ctx context.Context, strategyVersion string) (*Run, error) {
	version := NormalizeStrategyVersion(strategyVersion)
	run, err := s.store.ActiveRun(ctx, version)
	if err != nil || run != nil {
		return run, err
	}
	runs, err := s.store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	var best *Run
	for i := range runs {
		if runs[i].StrategyVersion != version {
			continue
		}
		if best == nil || runs[i].ID > best.ID {
			copy := runs[i]
			best = &copy
		}
	}
	return best, nil
}

func NormalizeStrategyVersion(value string) string {
	if strings.TrimSpace(value) == "" {
		return StrategyVersion
	}
	return strings.TrimSpace(value)
}

func matchETag(ifNoneMatch, etag string) bool {
	left := strings.TrimSpace(ifNoneMatch)
	right := strings.TrimSpace(etag)
	return left != "" && right != "" && left == right
}
