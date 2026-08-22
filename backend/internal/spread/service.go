package spread

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

type Service struct {
	store      HistoryStore
	cacheTTL   time.Duration
	queries    *semaphore.Weighted
	group      singleflight.Group
	mu         sync.Mutex
	cache      map[string]cachedHistory
}

type cachedHistory struct {
	value     History
	expiresAt time.Time
}

func NewService(store HistoryStore, cacheTTL time.Duration, maxConcurrent int64) *Service {
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Second
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	return &Service{
		store: store, cacheTTL: cacheTTL,
		queries: semaphore.NewWeighted(maxConcurrent),
		cache:   make(map[string]cachedHistory),
	}
}

func (s *Service) GetHistory(ctx context.Context, request HistoryRequest) (History, error) {
	normalized, err := NormalizeRequest(request)
	if err != nil {
		return History{}, err
	}
	key := cacheKey(normalized)
	if history, ok := s.cached(key); ok {
		return history, nil
	}
	value, err, _ := s.group.Do(key, func() (any, error) {
		if history, ok := s.cached(key); ok {
			return history, nil
		}
		if err := s.queries.Acquire(ctx, 1); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return History{}, fmt.Errorf("%w: %w", ErrTimeout, err)
			}
			return History{}, fmt.Errorf("%w: %w", ErrQueryFailed, err)
		}
		defer s.queries.Release(1)
		history, queryErr := s.store.QueryHistory(ctx, normalized)
		if queryErr != nil {
			return History{}, queryErr
		}
		s.storeCache(key, history)
		return history, nil
	})
	if err != nil {
		return History{}, err
	}
	history, _ := value.(History)
	return history, nil
}

func cacheKey(request HistoryRequest) string {
	return request.Venue + "|" + CanonicalSymbol(request.BaseAsset, request.QuoteAsset) +
		"|" + string(request.Range)
}

func (s *Service) cached(key string) (History, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.cache[key]
	if !ok || time.Now().After(item.expiresAt) {
		if ok {
			delete(s.cache, key)
		}
		return History{}, false
	}
	return item.value, true
}

func (s *Service) storeCache(key string, history History) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = cachedHistory{value: history, expiresAt: time.Now().Add(s.cacheTTL)}
}
