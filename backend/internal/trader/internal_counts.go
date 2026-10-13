package trader

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
)

type OwnerArbitrageCount struct {
	OwnerUsername string
	Running       int64
	Closing       int64
	Closed        int64
	Failed        int64
}

type arbitrageCountStore interface {
	CountArbitrageCombinationsByOwner(context.Context) ([]OwnerArbitrageCount, error)
}

func (s *Service) WithAdminInternalToken(token string) *Service {
	s.adminToken = strings.TrimSpace(token)
	return s
}

func (s *Service) authorizeAdmin(token string) error {
	want := []byte(strings.TrimSpace(s.adminToken))
	got := []byte(strings.TrimSpace(token))
	if len(want) == 0 || len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrInvalidServiceToken
	}
	return nil
}

func (s *Service) GetArbitrageCountsByOwnerInternal(
	ctx context.Context, serviceToken string,
) ([]OwnerArbitrageCount, error) {
	if err := s.authorizeAdmin(serviceToken); err != nil {
		return nil, err
	}
	if s.counts == nil {
		return nil, fmt.Errorf("arbitrage count store unavailable")
	}
	return s.counts.CountArbitrageCombinationsByOwner(ctx)
}

func (r *Repository) CountArbitrageCombinationsByOwner(
	ctx context.Context,
) ([]OwnerArbitrageCount, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT owner_username, status, count(*)
		FROM trader_arbitrage_combinations
		GROUP BY owner_username, status`)
	if err != nil {
		return nil, fmt.Errorf("count arbitrage by owner: %w", err)
	}
	defer rows.Close()
	byOwner := map[string]*OwnerArbitrageCount{}
	order := make([]string, 0)
	for rows.Next() {
		var owner, status string
		var count int64
		if err := rows.Scan(&owner, &status, &count); err != nil {
			return nil, err
		}
		item, ok := byOwner[owner]
		if !ok {
			item = &OwnerArbitrageCount{OwnerUsername: owner}
			byOwner[owner] = item
			order = append(order, owner)
		}
		switch status {
		case "running":
			item.Running = count
		case "closing":
			item.Closing = count
		case "closed":
			item.Closed = count
		case "failed":
			item.Failed = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]OwnerArbitrageCount, 0, len(order))
	for _, owner := range order {
		out = append(out, *byOwner[owner])
	}
	return out, nil
}
