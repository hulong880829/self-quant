package admin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"time"

	"selfquant/backend/internal/account"
)

var ErrInvalidServiceToken = errors.New("invalid service token")

type Service struct {
	repo         *Repository
	signer       *Signer
	gatewayToken string
	adminToken   string
	accounts     accountSummaryClient
	traders      arbitrageCountClient
}

type accountSummaryClient interface {
	GetAdminAccountSummaryInternal(ctx context.Context, serviceToken string) ([]account.OwnerAccountRow, error)
}

type arbitrageCountClient interface {
	GetArbitrageCountsByOwnerInternal(ctx context.Context, serviceToken string) ([]ArbitrageCount, error)
}

type ArbitrageCount struct {
	OwnerUsername string
	Running       int64
	Closing       int64
	Closed        int64
	Failed        int64
}

type Overview struct {
	Platform OverviewRow
	Brokers  []OverviewRow
}

type OverviewRow struct {
	BrokerID         string
	Name             string
	Status           string
	PlatformAccounts int64
	Running          int64
	Closing          int64
	Closed           int64
	Failed           int64
	EquityUSD        string
	CoveredAccounts  int64
	FailedSnapshots  int64
	StaleSnapshots   int64
}

func NewService(repo *Repository, signer *Signer, gatewayToken, adminToken string) *Service {
	return &Service{repo: repo, signer: signer, gatewayToken: gatewayToken, adminToken: adminToken}
}

func (s *Service) WithAccountSummary(client accountSummaryClient) *Service {
	s.accounts = client
	return s
}

func (s *Service) WithTraderCounts(client arbitrageCountClient) *Service {
	s.traders = client
	return s
}

func (s *Service) authorizeGateway(token string) error {
	want := []byte(strings.TrimSpace(s.gatewayToken))
	got := []byte(strings.TrimSpace(token))
	if len(want) == 0 || len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrInvalidServiceToken
	}
	return nil
}

func (s *Service) CreateBroker(ctx context.Context, serviceToken, actor, name string) (Broker, string, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Broker"
	}
	secret, err := randomSecret()
	if err != nil {
		return Broker{}, "", err
	}
	hash, err := account.HashPassword(secret)
	if err != nil {
		return Broker{}, "", err
	}
	broker, err := s.repo.CreateBroker(ctx, name, hash, maskSecret(secret))
	if err != nil {
		return Broker{}, "", err
	}
	_ = s.repo.WriteAudit(ctx, actor, "create_broker", "broker", broker.BrokerID, map[string]any{"name": name})
	return broker, secret, nil
}

func (s *Service) ListBrokers(ctx context.Context, serviceToken string) ([]Broker, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return nil, err
	}
	return s.repo.ListBrokers(ctx)
}

func (s *Service) GetBroker(ctx context.Context, serviceToken, brokerID string) (Broker, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, err
	}
	return s.repo.GetBroker(ctx, strings.TrimSpace(brokerID))
}

func (s *Service) DisableBroker(ctx context.Context, serviceToken, actor, brokerID string) (Broker, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, err
	}
	broker, err := s.repo.SetBrokerStatus(ctx, strings.TrimSpace(brokerID), "disabled", true)
	if err != nil {
		return Broker{}, err
	}
	_ = s.repo.WriteAudit(ctx, actor, "disable_broker", "broker", broker.BrokerID, nil)
	return broker, nil
}

func (s *Service) RestoreBroker(ctx context.Context, serviceToken, actor, brokerID string) (Broker, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, err
	}
	broker, err := s.repo.SetBrokerStatus(ctx, strings.TrimSpace(brokerID), "active", true)
	if err != nil {
		return Broker{}, err
	}
	_ = s.repo.WriteAudit(ctx, actor, "restore_broker", "broker", broker.BrokerID, nil)
	return broker, nil
}

func (s *Service) DeleteBroker(ctx context.Context, serviceToken, actor, brokerID string) (Broker, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, err
	}
	broker, err := s.repo.GetBroker(ctx, strings.TrimSpace(brokerID))
	if err != nil {
		return Broker{}, err
	}
	if err := s.repo.DeleteBroker(ctx, broker.BrokerID); err != nil {
		return Broker{}, err
	}
	_ = s.repo.WriteAudit(ctx, actor, "delete_broker", "broker", broker.BrokerID, nil)
	return broker, nil
}

func (s *Service) ResetBrokerSecret(ctx context.Context, serviceToken, actor, brokerID string) (Broker, string, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, "", err
	}
	secret, err := randomSecret()
	if err != nil {
		return Broker{}, "", err
	}
	hash, err := account.HashPassword(secret)
	if err != nil {
		return Broker{}, "", err
	}
	broker, err := s.repo.ResetSecret(ctx, strings.TrimSpace(brokerID), hash, maskSecret(secret))
	if err != nil {
		return Broker{}, "", err
	}
	_ = s.repo.WriteAudit(ctx, actor, "reset_broker_secret", "broker", broker.BrokerID, nil)
	return broker, secret, nil
}

func (s *Service) IssueAccessToken(ctx context.Context, serviceToken, clientID, clientSecret string) (string, int64, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return "", 0, err
	}
	broker, err := s.repo.GetBroker(ctx, strings.TrimSpace(clientID))
	if err != nil {
		return "", 0, ErrInvalidBrokerSecret
	}
	if broker.Status != "active" {
		return "", 0, ErrBrokerDisabled
	}
	if err := account.CheckPassword(broker.ClientSecretHash, clientSecret); err != nil {
		return "", 0, ErrInvalidBrokerSecret
	}
	token, expiresIn, err := s.signer.Issue(broker.BrokerID, broker.TokenVersion)
	if err != nil {
		return "", 0, err
	}
	return token, expiresIn, nil
}

func (s *Service) BrokerAuthState(ctx context.Context, serviceToken, brokerID string) (Broker, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Broker{}, err
	}
	return s.repo.GetBroker(ctx, strings.TrimSpace(brokerID))
}

func (s *Service) ListAudit(ctx context.Context, serviceToken string, limit int32) ([]AuditEvent, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return nil, err
	}
	return s.repo.ListAudit(ctx, int(limit))
}

func (s *Service) Overview(ctx context.Context, serviceToken string) (Overview, error) {
	if err := s.authorizeGateway(serviceToken); err != nil {
		return Overview{}, err
	}
	brokers, err := s.repo.ListBrokers(ctx)
	if err != nil {
		return Overview{}, err
	}
	owners := []account.OwnerAccountRow{}
	if s.accounts != nil {
		owners, err = s.accounts.GetAdminAccountSummaryInternal(ctx, s.adminToken)
		if err != nil {
			return Overview{}, fmt.Errorf("account summary: %w", err)
		}
	}
	counts := map[string]ArbitrageCount{}
	if s.traders != nil {
		rows, err := s.traders.GetArbitrageCountsByOwnerInternal(ctx, s.adminToken)
		if err != nil {
			return Overview{}, fmt.Errorf("trader counts: %w", err)
		}
		for _, row := range rows {
			counts[row.OwnerUsername] = row
		}
	}
	equities := map[string]account.EquityAggregate{}
	if snapshots, snapErr := s.repo.ListEquitySnapshots(ctx); snapErr == nil {
		equities = account.AggregateEquitySnapshots(snapshotsToAccountViews(snapshots), 15*time.Minute, time.Now().UTC())
	}
	byBroker := map[string]OverviewRow{}
	platform := OverviewRow{BrokerID: "", Name: "platform", Status: "active"}
	for _, owner := range owners {
		row := byBroker[owner.BrokerID]
		row.BrokerID = owner.BrokerID
		row.PlatformAccounts++
		if count, ok := counts[owner.Username]; ok {
			row.Running += count.Running
			row.Closing += count.Closing
			row.Closed += count.Closed
			row.Failed += count.Failed
		}
		byBroker[owner.BrokerID] = row
		platform.PlatformAccounts++
		if count, ok := counts[owner.Username]; ok {
			platform.Running += count.Running
			platform.Closing += count.Closing
			platform.Closed += count.Closed
			platform.Failed += count.Failed
		}
	}
	applyEquity(&platform, equities)
	out := Overview{Platform: platform, Brokers: make([]OverviewRow, 0, len(brokers))}
	for _, broker := range brokers {
		row := byBroker[broker.BrokerID]
		row.BrokerID = broker.BrokerID
		row.Name = broker.Name
		row.Status = broker.Status
		applyEquity(&row, equities)
		out.Brokers = append(out.Brokers, row)
	}
	return out, nil
}

func snapshotsToAccountViews(rows []EquitySnapshotView) []account.EquitySnapshotView {
	out := make([]account.EquitySnapshotView, 0, len(rows))
	for _, row := range rows {
		out = append(out, account.EquitySnapshotView{
			OwnerUsername: row.OwnerUsername, BrokerID: row.BrokerID, VenueIdentity: row.VenueIdentity,
			EquityUSD: row.EquityUSD, Status: row.Status, ObservedAt: row.ObservedAt,
		})
	}
	return out
}

func applyEquity(row *OverviewRow, equities map[string]account.EquityAggregate) {
	if row == nil {
		return
	}
	var agg account.EquityAggregate
	if row.Name == "platform" {
		for _, item := range equities {
			agg.EquityUSD = agg.EquityUSD.Add(item.EquityUSD)
			agg.CoveredAccounts += item.CoveredAccounts
			agg.FailedSnapshots += item.FailedSnapshots
			agg.StaleSnapshots += item.StaleSnapshots
		}
	} else {
		item, ok := equities[row.BrokerID]
		if !ok {
			return
		}
		agg = item
	}
	row.EquityUSD = agg.EquityUSD.String()
	row.CoveredAccounts = agg.CoveredAccounts
	row.FailedSnapshots = agg.FailedSnapshots
	row.StaleSnapshots = agg.StaleSnapshots
}
