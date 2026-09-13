package info

import (
	"encoding/json"
	"fmt"
)

// StakingSummary is the per-user staking snapshot returned by
// /info {"type":"delegatorSummary"}.
type StakingSummary struct {
	Delegated              string `json:"delegated"`
	Undelegated            string `json:"undelegated"`
	TotalPendingWithdrawal string `json:"totalPendingWithdrawal"`
	NPendingWithdrawals    int    `json:"nPendingWithdrawals"`
}

// StakingDelegation is one active-delegation row in /info
// {"type":"delegations"}.
type StakingDelegation struct {
	Validator            string `json:"validator"`
	Amount               string `json:"amount"`
	LockedUntilTimestamp int64  `json:"lockedUntilTimestamp"`
}

// StakingReward is one staking-reward row in /info
// {"type":"delegatorRewards"}.
type StakingReward struct {
	Time        int64  `json:"time"`
	Source      string `json:"source"`
	TotalAmount string `json:"totalAmount"`
}

// StakeGroup exposes the staking-info shortcuts. Accessed via the
// Client.Stake field, populated by New.
type StakeGroup struct{ i *Client }

// Summary returns the staking summary for addr.
func (g *StakeGroup) Summary(addr string) (*StakingSummary, error) {
	resp, err := g.i.client.Post("/info", map[string]any{
		"type": "delegatorSummary",
		"user": addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch staking summary: %w", err)
	}

	var result StakingSummary
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal staking summary: %w", err)
	}
	return &result, nil
}

// Delegations returns the active delegations for addr.
func (g *StakeGroup) Delegations(addr string) ([]StakingDelegation, error) {
	resp, err := g.i.client.Post("/info", map[string]any{
		"type": "delegations",
		"user": addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch staking delegations: %w", err)
	}

	var result []StakingDelegation
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal staking delegations: %w", err)
	}
	return result, nil
}

// Rewards returns the staking reward history for addr.
func (g *StakeGroup) Rewards(addr string) ([]StakingReward, error) {
	resp, err := g.i.client.Post("/info", map[string]any{
		"type": "delegatorRewards",
		"user": addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch staking rewards: %w", err)
	}

	var result []StakingReward
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal staking rewards: %w", err)
	}
	return result, nil
}
