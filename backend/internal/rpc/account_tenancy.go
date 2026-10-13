package rpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	accountv1 "selfquant/backend/gen/account/v1"
	"selfquant/backend/internal/account"
)

func (s *AccountServer) ListPlatformAccountsInternal(
	ctx context.Context,
	request *accountv1.ListPlatformAccountsInternalRequest,
) (*accountv1.ListPlatformAccountsInternalResponse, error) {
	items, err := s.service.ListPlatformAccountsInternal(
		ctx, request.GetServiceToken(), request.GetBrokerId(),
	)
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	response := &accountv1.ListPlatformAccountsInternalResponse{
		Accounts: make([]*accountv1.PlatformAccount, 0, len(items)),
	}
	for _, item := range items {
		response.Accounts = append(response.Accounts, platformAccountToProto(item))
	}
	return response, nil
}

func (s *AccountServer) CreatePlatformAccountInternal(
	ctx context.Context,
	request *accountv1.CreatePlatformAccountInternalRequest,
) (*accountv1.PlatformAccountResponse, error) {
	item, err := s.service.CreatePlatformAccountInternal(ctx, request.GetServiceToken(), account.CreatePlatformAccountInput{
		ActorUsername: request.GetActorUsername(),
		ActorID:       request.GetActorId(),
		BrokerID:      request.GetBrokerId(),
		Username:      request.GetUsername(),
		Password:      request.GetPassword(),
		CreatedSource: request.GetCreatedSource(),
	})
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.PlatformAccountResponse{Account: platformAccountToProto(item)}, nil
}

func (s *AccountServer) DisablePlatformAccountInternal(
	ctx context.Context,
	request *accountv1.MutatePlatformAccountInternalRequest,
) (*accountv1.PlatformAccountResponse, error) {
	item, err := s.service.DisablePlatformAccountInternal(ctx, request.GetServiceToken(), mutateInput(request))
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.PlatformAccountResponse{Account: platformAccountToProto(item)}, nil
}

func (s *AccountServer) RestorePlatformAccountInternal(
	ctx context.Context,
	request *accountv1.MutatePlatformAccountInternalRequest,
) (*accountv1.PlatformAccountResponse, error) {
	item, err := s.service.RestorePlatformAccountInternal(ctx, request.GetServiceToken(), mutateInput(request))
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.PlatformAccountResponse{Account: platformAccountToProto(item)}, nil
}

func (s *AccountServer) DeletePlatformAccountInternal(
	ctx context.Context,
	request *accountv1.MutatePlatformAccountInternalRequest,
) (*accountv1.PlatformAccountResponse, error) {
	item, err := s.service.DeletePlatformAccountInternal(ctx, request.GetServiceToken(), mutateInput(request))
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.PlatformAccountResponse{Account: platformAccountToProto(item)}, nil
}

func (s *AccountServer) ResetPlatformAccountPasswordInternal(
	ctx context.Context,
	request *accountv1.ResetPlatformAccountPasswordInternalRequest,
) (*accountv1.PlatformAccountResponse, error) {
	item, err := s.service.ResetPlatformAccountPasswordInternal(
		ctx, request.GetServiceToken(), account.MutatePlatformAccountInput{
			ActorUsername: request.GetActorUsername(),
			ActorID:       request.GetActorId(),
			BrokerID:      request.GetBrokerId(),
			Username:      request.GetUsername(),
		}, request.GetPassword(),
	)
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.PlatformAccountResponse{Account: platformAccountToProto(item)}, nil
}

func (s *AccountServer) UpdatePlatformAccountBrokerInternal(
	ctx context.Context,
	request *accountv1.UpdatePlatformAccountBrokerInternalRequest,
) (*accountv1.PlatformAccountResponse, error) {
	item, err := s.service.UpdatePlatformAccountBrokerInternal(ctx, request.GetServiceToken(), account.MutatePlatformAccountInput{
		ActorUsername: request.GetActorUsername(),
		Username:      request.GetUsername(),
		BrokerID:      request.GetBrokerId(),
	})
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.PlatformAccountResponse{Account: platformAccountToProto(item)}, nil
}

func (s *AccountServer) IssueBrokerDelegatedSessionInternal(
	ctx context.Context,
	request *accountv1.IssueBrokerDelegatedSessionInternalRequest,
) (*accountv1.IssueBrokerDelegatedSessionInternalResponse, error) {
	session, err := s.service.IssueBrokerDelegatedSessionInternal(
		ctx, request.GetServiceToken(), request.GetBrokerId(),
		request.GetTargetUsername(), request.GetActorId(),
	)
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	return &accountv1.IssueBrokerDelegatedSessionInternalResponse{
		SessionToken: session.Token,
		Username:     session.Username,
		Permission:   session.Permission,
	}, nil
}

func (s *AccountServer) GetAdminAccountSummaryInternal(
	ctx context.Context,
	request *accountv1.GetAdminAccountSummaryInternalRequest,
) (*accountv1.GetAdminAccountSummaryInternalResponse, error) {
	rows, err := s.service.GetAdminAccountSummaryInternal(ctx, request.GetServiceToken())
	if err != nil {
		return nil, mapPlatformAccountError(err)
	}
	response := &accountv1.GetAdminAccountSummaryInternalResponse{
		Owners: make([]*accountv1.OwnerAccountRow, 0, len(rows)),
	}
	for _, row := range rows {
		response.Owners = append(response.Owners, &accountv1.OwnerAccountRow{
			Username:   row.Username,
			BrokerId:   row.BrokerID,
			Status:     row.Status,
			Permission: row.Permission,
		})
	}
	return response, nil
}

func mutateInput(request *accountv1.MutatePlatformAccountInternalRequest) account.MutatePlatformAccountInput {
	return account.MutatePlatformAccountInput{
		ActorUsername: request.GetActorUsername(),
		ActorID:       request.GetActorId(),
		BrokerID:      request.GetBrokerId(),
		Username:      request.GetUsername(),
	}
}

func platformAccountToProto(item account.PlatformAccountView) *accountv1.PlatformAccount {
	out := &accountv1.PlatformAccount{
		Username:      item.Username,
		Permission:    item.Permission,
		BrokerId:      item.BrokerID,
		Status:        item.Status,
		CreatedSource: item.CreatedSource,
		CreatedBy:     item.CreatedBy,
		AuthVersion:   item.AuthVersion,
	}
	if item.DeletedAt != nil {
		out.DeletedAt = timestamppb.New(item.DeletedAt.UTC())
	}
	return out
}

func mapPlatformAccountError(err error) error {
	switch {
	case errors.Is(err, account.ErrInvalidServiceToken):
		return status.Error(codes.Unauthenticated, "invalid service token")
	case errors.Is(err, account.ErrNotFound),
		errors.Is(err, account.ErrDelegatedForbidden),
		errors.Is(err, account.ErrBrokerMissing):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, account.ErrAccountExists):
		return status.Error(codes.AlreadyExists, "platform account already exists")
	case errors.Is(err, account.ErrAccountConflict):
		return status.Error(codes.FailedPrecondition, "platform account has active work")
	case errors.Is(err, account.ErrInvalidPlatformAccount),
		errors.Is(err, account.ErrBrokerInactive):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, "platform account operation failed")
	}
}
