package rpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	adminv1 "selfquant/backend/gen/admin/v1"
	"selfquant/backend/internal/admin"
)

type adminAPI interface {
	CreateBroker(context.Context, string, string, string) (admin.Broker, string, error)
	ListBrokers(context.Context, string) ([]admin.Broker, error)
	GetBroker(context.Context, string, string) (admin.Broker, error)
	DisableBroker(context.Context, string, string, string) (admin.Broker, error)
	RestoreBroker(context.Context, string, string, string) (admin.Broker, error)
	DeleteBroker(context.Context, string, string, string) (admin.Broker, error)
	ResetBrokerSecret(context.Context, string, string, string) (admin.Broker, string, error)
	IssueAccessToken(context.Context, string, string, string) (string, int64, error)
	BrokerAuthState(context.Context, string, string) (admin.Broker, error)
	ListAudit(context.Context, string, int32) ([]admin.AuditEvent, error)
	Overview(context.Context, string) (admin.Overview, error)
}

type AdminServer struct {
	adminv1.UnimplementedAdminServiceServer
	service adminAPI
}

func NewAdminServer(service adminAPI) *AdminServer {
	return &AdminServer{service: service}
}

func (s *AdminServer) CreateBrokerInternal(ctx context.Context, request *adminv1.CreateBrokerInternalRequest) (*adminv1.BrokerSecretResponse, error) {
	broker, secret, err := s.service.CreateBroker(ctx, request.GetServiceToken(), request.GetActorUsername(), request.GetName())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.BrokerSecretResponse{Broker: brokerToProto(broker), ClientSecret: secret}, nil
}

func (s *AdminServer) ListBrokersInternal(ctx context.Context, request *adminv1.AdminActorRequest) (*adminv1.ListBrokersInternalResponse, error) {
	items, err := s.service.ListBrokers(ctx, request.GetServiceToken())
	if err != nil {
		return nil, mapAdminError(err)
	}
	response := &adminv1.ListBrokersInternalResponse{Brokers: make([]*adminv1.Broker, 0, len(items))}
	for _, item := range items {
		response.Brokers = append(response.Brokers, brokerToProto(item))
	}
	return response, nil
}

func (s *AdminServer) GetBrokerInternal(ctx context.Context, request *adminv1.GetBrokerInternalRequest) (*adminv1.BrokerResponse, error) {
	broker, err := s.service.GetBroker(ctx, request.GetServiceToken(), request.GetBrokerId())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.BrokerResponse{Broker: brokerToProto(broker)}, nil
}

func (s *AdminServer) DisableBrokerInternal(ctx context.Context, request *adminv1.GetBrokerInternalRequest) (*adminv1.BrokerResponse, error) {
	broker, err := s.service.DisableBroker(ctx, request.GetServiceToken(), request.GetActorUsername(), request.GetBrokerId())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.BrokerResponse{Broker: brokerToProto(broker)}, nil
}

func (s *AdminServer) RestoreBrokerInternal(ctx context.Context, request *adminv1.GetBrokerInternalRequest) (*adminv1.BrokerResponse, error) {
	broker, err := s.service.RestoreBroker(ctx, request.GetServiceToken(), request.GetActorUsername(), request.GetBrokerId())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.BrokerResponse{Broker: brokerToProto(broker)}, nil
}

func (s *AdminServer) DeleteBrokerInternal(ctx context.Context, request *adminv1.GetBrokerInternalRequest) (*adminv1.BrokerResponse, error) {
	broker, err := s.service.DeleteBroker(ctx, request.GetServiceToken(), request.GetActorUsername(), request.GetBrokerId())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.BrokerResponse{Broker: brokerToProto(broker)}, nil
}

func (s *AdminServer) ResetBrokerSecretInternal(ctx context.Context, request *adminv1.GetBrokerInternalRequest) (*adminv1.BrokerSecretResponse, error) {
	broker, secret, err := s.service.ResetBrokerSecret(ctx, request.GetServiceToken(), request.GetActorUsername(), request.GetBrokerId())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.BrokerSecretResponse{Broker: brokerToProto(broker), ClientSecret: secret}, nil
}

func (s *AdminServer) IssueBrokerAccessTokenInternal(ctx context.Context, request *adminv1.IssueBrokerAccessTokenInternalRequest) (*adminv1.IssueBrokerAccessTokenInternalResponse, error) {
	token, expiresIn, err := s.service.IssueAccessToken(ctx, request.GetServiceToken(), request.GetClientId(), request.GetClientSecret())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.IssueBrokerAccessTokenInternalResponse{
		AccessToken: token, TokenType: "Bearer", ExpiresIn: expiresIn,
	}, nil
}

func (s *AdminServer) GetBrokerAuthStateInternal(ctx context.Context, request *adminv1.GetBrokerAuthStateInternalRequest) (*adminv1.GetBrokerAuthStateInternalResponse, error) {
	broker, err := s.service.BrokerAuthState(ctx, request.GetServiceToken(), request.GetBrokerId())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.GetBrokerAuthStateInternalResponse{
		BrokerId: broker.BrokerID, Status: broker.Status, TokenVersion: broker.TokenVersion,
	}, nil
}

func (s *AdminServer) ListAuditEventsInternal(ctx context.Context, request *adminv1.ListAuditEventsInternalRequest) (*adminv1.ListAuditEventsInternalResponse, error) {
	items, err := s.service.ListAudit(ctx, request.GetServiceToken(), request.GetLimit())
	if err != nil {
		return nil, mapAdminError(err)
	}
	response := &adminv1.ListAuditEventsInternalResponse{Events: make([]*adminv1.AuditEvent, 0, len(items))}
	for _, item := range items {
		response.Events = append(response.Events, &adminv1.AuditEvent{
			Id: item.ID, ActorUsername: item.ActorUsername, ActorId: item.ActorID,
			Action: item.Action, ResourceType: item.ResourceType, ResourceId: item.ResourceID,
			CreatedAt: timestamppb.New(item.CreatedAt),
		})
	}
	return response, nil
}

func (s *AdminServer) GetOverviewInternal(ctx context.Context, request *adminv1.AdminActorRequest) (*adminv1.GetOverviewInternalResponse, error) {
	overview, err := s.service.Overview(ctx, request.GetServiceToken())
	if err != nil {
		return nil, mapAdminError(err)
	}
	return &adminv1.GetOverviewInternalResponse{
		Platform: overviewRowToProto(overview.Platform),
		Brokers:  overviewRowsToProto(overview.Brokers),
	}, nil
}

func brokerToProto(broker admin.Broker) *adminv1.Broker {
	return &adminv1.Broker{
		BrokerId: broker.BrokerID, Name: broker.Name, ClientSecretMasked: broker.ClientSecretMasked,
		Status: broker.Status, TokenVersion: broker.TokenVersion,
		CreatedAt: timestamppb.New(broker.CreatedAt), UpdatedAt: timestamppb.New(broker.UpdatedAt),
	}
}

func overviewRowToProto(row admin.OverviewRow) *adminv1.OverviewBrokerRow {
	return &adminv1.OverviewBrokerRow{
		BrokerId: row.BrokerID, Name: row.Name, Status: row.Status,
		PlatformAccounts: row.PlatformAccounts, Running: row.Running, Closing: row.Closing,
		Closed: row.Closed, Failed: row.Failed, EquityUsd: row.EquityUSD,
		CoveredAccounts: row.CoveredAccounts, FailedSnapshots: row.FailedSnapshots,
		StaleSnapshots: row.StaleSnapshots,
	}
}

func overviewRowsToProto(rows []admin.OverviewRow) []*adminv1.OverviewBrokerRow {
	out := make([]*adminv1.OverviewBrokerRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, overviewRowToProto(row))
	}
	return out
}

func mapAdminError(err error) error {
	switch {
	case errors.Is(err, admin.ErrInvalidServiceToken):
		return status.Error(codes.Unauthenticated, "invalid service token")
	case errors.Is(err, admin.ErrBrokerNotFound):
		return status.Error(codes.NotFound, "broker not found")
	case errors.Is(err, admin.ErrInvalidBrokerSecret), errors.Is(err, admin.ErrBrokerDisabled):
		return status.Error(codes.Unauthenticated, "invalid client credentials")
	case errors.Is(err, admin.ErrBrokerHasAccounts):
		return status.Error(codes.FailedPrecondition, "broker still has platform accounts")
	default:
		return status.Error(codes.Internal, "admin operation failed")
	}
}
