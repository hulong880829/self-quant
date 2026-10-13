package account

import (
	"crypto/subtle"
	"strings"
)

func authorizeCaller(expected, provided string) error {
	want := []byte(strings.TrimSpace(expected))
	got := []byte(strings.TrimSpace(provided))
	if len(want) == 0 || len(want) != len(got) ||
		subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrInvalidServiceToken
	}
	return nil
}

func (s *Service) authorizeGateway(token string) error {
	return authorizeCaller(s.gatewayToken, token)
}

func (s *Service) authorizeAdmin(token string) error {
	return authorizeCaller(s.adminToken, token)
}

func (s *Service) authorizeReport(token string) error {
	return authorizeCaller(s.internalToken, token)
}

func (s *Service) authorizeTrader(token string) error {
	return authorizeCaller(s.traderToken, token)
}
