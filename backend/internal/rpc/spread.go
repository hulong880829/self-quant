package rpc

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	spreadv1 "selfquant/backend/gen/spread/v1"
	"selfquant/backend/internal/spread"
)

type spreadService interface {
	GetHistory(context.Context, spread.HistoryRequest) (spread.History, error)
}

type SpreadServer struct {
	spreadv1.UnimplementedSpreadServiceServer
	service spreadService
}

func NewSpreadServer(service spreadService) *SpreadServer {
	return &SpreadServer{service: service}
}

func (s *SpreadServer) GetBasisSpreadHistory(
	ctx context.Context,
	request *spreadv1.GetBasisSpreadHistoryRequest,
) (*spreadv1.GetBasisSpreadHistoryResponse, error) {
	parsedRange, err := protoRange(request.GetRange())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "range is required")
	}
	history, err := s.service.GetHistory(ctx, spread.HistoryRequest{
		Venue: request.GetVenue(), CompareVenue: request.GetCompareVenue(),
		BaseAsset: request.GetBaseAsset(), QuoteAsset: request.GetQuoteAsset(),
		VenueCanonicalSymbol:        request.GetVenueCanonicalSymbol(),
		CompareVenueCanonicalSymbol: request.GetCompareVenueCanonicalSymbol(),
		Range:                       parsedRange,
	})
	if err != nil {
		return nil, mapSpreadError(err)
	}
	response := &spreadv1.GetBasisSpreadHistoryResponse{
		Venue: history.Venue, CompareVenue: history.CompareVenue,
		BaseAsset: history.BaseAsset, QuoteAsset: history.QuoteAsset,
		CanonicalSymbol: history.CanonicalSymbol, Range: request.GetRange(),
		ResolutionSeconds: int32(history.ResolutionSeconds),
		Availability:      protoAvailability(history.Availability),
		AsOf:              timestamppb.New(history.AsOf),
		Points:            make([]*spreadv1.BasisSpreadPoint, 0, len(history.Points)),
		Summary: &spreadv1.BasisSpreadSummary{
			CurrentBps: decimalString(history.Summary.CurrentBps),
			MinBps:     decimalString(history.Summary.MinBps),
			MaxBps:     decimalString(history.Summary.MaxBps),
			AvgBps:     decimalString(history.Summary.AvgBps),
			Coverage:   decimalString(history.Summary.Coverage),
		},
	}
	for _, point := range history.Points {
		response.Points = append(response.Points, &spreadv1.BasisSpreadPoint{
			Ts: timestamppb.New(point.TS), SpreadBps: decimalString(point.SpreadBps),
			SpotAsk: decimalString(point.SpotAsk), PerpetualAsk: decimalString(point.PerpetualAsk),
			Samples: int32(point.Samples),
		})
	}
	return response, nil
}

func protoRange(value spreadv1.BasisSpreadRange) (spread.Range, error) {
	switch value {
	case spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_1H:
		return spread.Range1h, nil
	case spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_4H:
		return spread.Range4h, nil
	case spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_8H:
		return spread.Range8h, nil
	case spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_24H:
		return spread.Range24h, nil
	case spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_7D:
		return spread.Range7d, nil
	default:
		return "", spread.ErrInvalidArgument
	}
}

func protoAvailability(value spread.Availability) spreadv1.BasisSpreadAvailability {
	if value == spread.AvailabilityAvailable {
		return spreadv1.BasisSpreadAvailability_BASIS_SPREAD_AVAILABILITY_AVAILABLE
	}
	return spreadv1.BasisSpreadAvailability_BASIS_SPREAD_AVAILABILITY_UNAVAILABLE
}

func mapSpreadError(err error) error {
	switch {
	case errors.Is(err, spread.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, "invalid basis spread request")
	case errors.Is(err, spread.ErrUnavailable):
		return status.Error(codes.FailedPrecondition, "basis spread unavailable")
	case errors.Is(err, spread.ErrTimeout), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.Error(codes.DeadlineExceeded, "basis spread query timed out")
	default:
		return status.Error(codes.Unavailable, "basis spread query failed")
	}
}

func decimalString(value interface{ String() string }) string {
	if value == nil {
		return "0"
	}
	text := strings.TrimSpace(value.String())
	if text == "" {
		return "0"
	}
	return text
}
