package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func (a *binanceAdapter) ApplyAccountProfile(
	ctx context.Context,
	credentials Credentials,
	_ AccountProfileRequest,
) (AccountProfileResult, error) {
	raw, err := a.signed(ctx, http.MethodGet, "/papi/v1/account", credentials, url.Values{}, nil)
	if err != nil {
		step := profileErrorStep(AccountProfileStepUnifiedAccount, "binance", raw, err)
		blocked := AccountProfileStep(
			AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code,
			"Portfolio Margin is not available; enable it manually before retrying",
		)
		position := AccountProfileStep(
			AccountProfileStepOneWayPosition, step.Status, step.Code,
			"position mode cannot be changed until Portfolio Margin is available",
		)
		return NewAccountProfileResult(step, blocked, position), nil
	}
	var account struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if decodeErr := unmarshalJSON(raw, &account); decodeErr != nil {
		return AccountProfileResult{}, decodeErr
	}
	if account.Code != 0 {
		err = fmt.Errorf("%w: Binance code %d: %s", ErrRejected, account.Code, account.Msg)
		step := profileErrorStep(AccountProfileStepUnifiedAccount, "binance", raw, err)
		return NewAccountProfileResult(
			step,
			AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
			AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
		), nil
	}
	unified := AccountProfileStep(
		AccountProfileStepUnifiedAccount, AccountProfileStatusCompliant, "", "Portfolio Margin is enabled",
	)
	margin := AccountProfileStep(
		AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusCompliant, "",
		"Portfolio Margin provides shared cross-asset margin",
	)
	position, err := a.binanceApplyOneWay(ctx, credentials)
	if err != nil {
		return AccountProfileResult{}, err
	}
	return NewAccountProfileResult(unified, margin, position), nil
}

func (a *binanceAdapter) binanceApplyOneWay(
	ctx context.Context,
	credentials Credentials,
) (AccountProfileStepResult, error) {
	mode, err := a.GetPositionMode(ctx, credentials, Instrument{ContractType: "perpetual"})
	if err != nil {
		return profileErrorStep(AccountProfileStepOneWayPosition, "binance", nil, err), nil
	}
	if mode == PositionModeOneWay {
		return AccountProfileStep(
			AccountProfileStepOneWayPosition, AccountProfileStatusCompliant, "",
			"position mode is already one-way",
		), nil
	}
	values := url.Values{"dualSidePosition": {"false"}}
	raw, err := a.signed(
		ctx, http.MethodPost, "/papi/v1/um/positionSide/dual", credentials, values, nil,
	)
	if err != nil {
		return profileErrorStep(AccountProfileStepOneWayPosition, "binance", raw, err), nil
	}
	if code, message := binanceCodeMessage(raw); code != "" && code != "0" {
		return AccountProfileStep(
			AccountProfileStepOneWayPosition, AccountProfileStatusManualRequired, code, message,
		), nil
	}
	mode, err = a.GetPositionMode(ctx, credentials, Instrument{ContractType: "perpetual"})
	if err != nil || mode != PositionModeOneWay {
		if err == nil {
			err = errors.New("position mode remained hedge after update")
		}
		return profileErrorStep(AccountProfileStepOneWayPosition, "binance", nil, err), nil
	}
	return AccountProfileStep(
		AccountProfileStepOneWayPosition, AccountProfileStatusApplied, "",
		"position mode changed to one-way",
	), nil
}

func (a *okxAdapter) ApplyAccountProfile(
	ctx context.Context,
	credentials Credentials,
	_ AccountProfileRequest,
) (AccountProfileResult, error) {
	config, raw, err := a.okxAccountConfig(ctx, credentials)
	if err != nil {
		step := profileErrorStep(AccountProfileStepUnifiedAccount, "okx", raw, err)
		return NewAccountProfileResult(
			step,
			AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
			AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
		), nil
	}
	accountStatus := AccountProfileStatusCompliant
	accountMessage := "account level is already multi-currency margin"
	if config.AccountLevel != "3" {
		if step := a.okxSetAccountLevel(ctx, credentials); step.Status != AccountProfileStatusApplied {
			return NewAccountProfileResult(
				AccountProfileStep(AccountProfileStepUnifiedAccount, step.Status, step.Code, step.Message),
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(
					AccountProfileStepOneWayPosition, AccountProfileStatusManualRequired, step.Code,
					"account level must be changed before position mode can be updated",
				),
			), nil
		}
		accountStatus = AccountProfileStatusApplied
		accountMessage = "account level changed to multi-currency margin"
		config, _, err = a.okxAccountConfig(ctx, credentials)
		if err != nil || config.AccountLevel != "3" {
			if err == nil {
				err = errors.New("account level remained non-compliant after update")
			}
			step := profileErrorStep(AccountProfileStepUnifiedAccount, "okx", nil, err)
			return NewAccountProfileResult(
				step,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
			), nil
		}
	}
	unified := AccountProfileStep(
		AccountProfileStepUnifiedAccount, accountStatus, "", accountMessage,
	)
	margin := AccountProfileStep(
		AccountProfileStepMultiAssetCrossMargin, accountStatus, "", accountMessage,
	)
	position := AccountProfileStep(
		AccountProfileStepOneWayPosition, AccountProfileStatusCompliant, "",
		"position mode is already net mode",
	)
	if config.PositionMode != "net_mode" {
		body := compactJSON(map[string]string{"posMode": "net_mode"})
		raw, err = a.signed(
			ctx, http.MethodPost, "/api/v5/account/set-position-mode", credentials, body, nil,
		)
		if err != nil || !okxResponseOK(raw) {
			if err == nil {
				err = fmt.Errorf("%w: %s", ErrRejected, okxResponseMessage(raw))
			}
			position = profileErrorStep(AccountProfileStepOneWayPosition, "okx", raw, err)
		} else {
			verified, verifyRaw, verifyErr := a.okxAccountConfig(ctx, credentials)
			if verifyErr != nil || verified.PositionMode != "net_mode" {
				if verifyErr == nil {
					verifyErr = errors.New("position mode remained non-compliant after update")
				}
				position = profileErrorStep(
					AccountProfileStepOneWayPosition, "okx", verifyRaw, verifyErr,
				)
			} else {
				position = AccountProfileStep(
					AccountProfileStepOneWayPosition, AccountProfileStatusApplied, "",
					"position mode changed to net mode",
				)
			}
		}
	}
	return NewAccountProfileResult(unified, margin, position), nil
}

type okxProfileConfig struct {
	AccountLevel string
	PositionMode string
}

func (a *okxAdapter) okxAccountConfig(
	ctx context.Context,
	credentials Credentials,
) (okxProfileConfig, []byte, error) {
	raw, err := a.signed(
		ctx, http.MethodGet, "/api/v5/account/config", credentials, nil, nil,
	)
	if err != nil {
		return okxProfileConfig{}, raw, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			AccountLevel string `json:"acctLv"`
			PositionMode string `json:"posMode"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return okxProfileConfig{}, raw, err
	}
	if payload.Code != "0" || len(payload.Data) == 0 {
		return okxProfileConfig{}, raw, fmt.Errorf(
			"%w: OKX account config code %s: %s", ErrRejected, payload.Code, payload.Msg,
		)
	}
	return okxProfileConfig{
		AccountLevel: payload.Data[0].AccountLevel,
		PositionMode: payload.Data[0].PositionMode,
	}, raw, nil
}

func (a *okxAdapter) okxSetAccountLevel(
	ctx context.Context,
	credentials Credentials,
) AccountProfileStepResult {
	body := compactJSON(map[string]string{"acctLv": "3"})
	raw, err := a.signed(
		ctx, http.MethodPost, "/api/v5/account/set-account-switch-preset",
		credentials, body, nil,
	)
	if err != nil || !okxResponseOK(raw) {
		if err == nil {
			err = fmt.Errorf("%w: %s", ErrRejected, okxResponseMessage(raw))
		}
		return profileErrorStep(AccountProfileStepUnifiedAccount, "okx", raw, err)
	}
	values := url.Values{"acctLv": {"3"}}
	raw, err = a.signed(
		ctx, http.MethodGet,
		"/api/v5/account/set-account-switch-precheck?"+values.Encode(),
		credentials, nil, nil,
	)
	if err != nil || !okxResponseOK(raw) {
		if err == nil {
			err = fmt.Errorf("%w: %s", ErrRejected, okxResponseMessage(raw))
		}
		return profileErrorStep(AccountProfileStepUnifiedAccount, "okx", raw, err)
	}
	if reason := okxUnmatchedReason(raw); reason != "" {
		return AccountProfileStep(
			AccountProfileStepUnifiedAccount, AccountProfileStatusManualRequired,
			"precheck_failed", reason,
		)
	}
	raw, err = a.signed(
		ctx, http.MethodPost, "/api/v5/account/set-account-level",
		credentials, body, nil,
	)
	if err != nil || !okxResponseOK(raw) {
		if err == nil {
			err = fmt.Errorf("%w: %s", ErrRejected, okxResponseMessage(raw))
		}
		return profileErrorStep(AccountProfileStepUnifiedAccount, "okx", raw, err)
	}
	return AccountProfileStep(
		AccountProfileStepUnifiedAccount, AccountProfileStatusApplied, "",
		"account level changed to multi-currency margin",
	)
}

func (a *bybitAdapter) ApplyAccountProfile(
	ctx context.Context,
	credentials Credentials,
	request AccountProfileRequest,
) (AccountProfileResult, error) {
	info, raw, err := a.bybitAccountInfo(ctx, credentials)
	if err != nil {
		step := profileErrorStep(AccountProfileStepUnifiedAccount, "bybit", raw, err)
		return NewAccountProfileResult(
			step,
			AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
			AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
		), nil
	}
	unified := AccountProfileStep(
		AccountProfileStepUnifiedAccount, AccountProfileStatusCompliant, "",
		"Unified Trading Account is enabled",
	)
	if info.UnifiedMarginStatus <= 1 {
		body := compactJSON(map[string]any{})
		raw, err = a.signed(
			ctx, http.MethodPost, "/v5/account/upgrade-to-uta", credentials, body, nil,
		)
		status, code, message := bybitUpgradeResult(raw)
		if err != nil || code != "0" || status == "FAIL" {
			if err == nil {
				err = fmt.Errorf("%w: %s", ErrRejected, message)
			}
			step := profileErrorStep(AccountProfileStepUnifiedAccount, "bybit", raw, err)
			return NewAccountProfileResult(
				step,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
			), nil
		}
		if status == "PROCESS" {
			pending := AccountProfileStep(
				AccountProfileStepUnifiedAccount, AccountProfileStatusPending, "",
				"Bybit account upgrade is still processing",
			)
			return NewAccountProfileResult(
				pending,
				AccountProfileStep(
					AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusPending, "",
					"waiting for Unified Trading Account upgrade",
				),
				AccountProfileStep(
					AccountProfileStepOneWayPosition, AccountProfileStatusPending, "",
					"waiting for Unified Trading Account upgrade",
				),
			), nil
		}
		info, raw, err = a.bybitAccountInfo(ctx, credentials)
		if err != nil || info.UnifiedMarginStatus <= 1 {
			if err == nil {
				err = errors.New("Unified Trading Account upgrade has not completed")
			}
			pending := AccountProfileStep(
				AccountProfileStepUnifiedAccount, AccountProfileStatusPending, "",
				profileMessage(err),
			)
			return NewAccountProfileResult(
				pending,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusPending, "", pending.Message),
				AccountProfileStep(AccountProfileStepOneWayPosition, AccountProfileStatusPending, "", pending.Message),
			), nil
		}
		unified.Status = AccountProfileStatusApplied
		unified.Message = "Unified Trading Account upgrade completed"
	}
	margin := AccountProfileStep(
		AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusCompliant, "",
		"margin mode is already regular cross margin",
	)
	if info.MarginMode != "REGULAR_MARGIN" {
		raw, err = a.signed(
			ctx, http.MethodPost, "/v5/account/set-margin-mode", credentials,
			compactJSON(map[string]string{"setMarginMode": "REGULAR_MARGIN"}), nil,
		)
		if err != nil || !bybitResponseOK(raw) {
			if err == nil {
				err = fmt.Errorf("%w: %s", ErrRejected, bybitResponseMessage(raw))
			}
			margin = profileErrorStep(AccountProfileStepMultiAssetCrossMargin, "bybit", raw, err)
		} else {
			verified, verifyRaw, verifyErr := a.bybitAccountInfo(ctx, credentials)
			if verifyErr != nil || verified.MarginMode != "REGULAR_MARGIN" {
				if verifyErr == nil {
					verifyErr = errors.New("margin mode remained non-compliant after update")
				}
				margin = profileErrorStep(
					AccountProfileStepMultiAssetCrossMargin, "bybit", verifyRaw, verifyErr,
				)
			} else {
				margin = AccountProfileStep(
					AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusApplied, "",
					"margin mode changed to regular cross margin",
				)
			}
		}
	}
	position := a.bybitApplyOneWay(ctx, credentials, request.Instruments)
	return NewAccountProfileResult(unified, margin, position), nil
}

type bybitProfileInfo struct {
	UnifiedMarginStatus int
	MarginMode          string
}

func (a *bybitAdapter) bybitAccountInfo(
	ctx context.Context,
	credentials Credentials,
) (bybitProfileInfo, []byte, error) {
	raw, err := a.signed(ctx, http.MethodGet, "/v5/account/info", credentials, nil, nil)
	if err != nil {
		return bybitProfileInfo{}, raw, err
	}
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			UnifiedMarginStatus int    `json:"unifiedMarginStatus"`
			MarginMode          string `json:"marginMode"`
		} `json:"result"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return bybitProfileInfo{}, raw, err
	}
	if payload.RetCode != 0 {
		return bybitProfileInfo{}, raw, fmt.Errorf(
			"%w: Bybit code %d: %s", ErrRejected, payload.RetCode, payload.RetMsg,
		)
	}
	return bybitProfileInfo{
		UnifiedMarginStatus: payload.Result.UnifiedMarginStatus,
		MarginMode:          payload.Result.MarginMode,
	}, raw, nil
}

func (a *bybitAdapter) bybitApplyOneWay(
	ctx context.Context,
	credentials Credentials,
	instruments []Instrument,
) AccountProfileStepResult {
	coins := make(map[string]struct{})
	for _, instrument := range instruments {
		coin := strings.ToUpper(firstNonEmpty(instrument.SettleAsset, instrument.QuoteAsset))
		if coin != "" {
			coins[coin] = struct{}{}
		}
	}
	if len(coins) == 0 {
		coins["USDT"] = struct{}{}
	}
	ordered := make([]string, 0, len(coins))
	for coin := range coins {
		ordered = append(ordered, coin)
	}
	sort.Strings(ordered)
	for _, coin := range ordered {
		raw, err := a.signed(
			ctx, http.MethodPost, "/v5/position/switch-mode", credentials,
			compactJSON(map[string]any{"category": "linear", "coin": coin, "mode": 0}), nil,
		)
		if err != nil || !bybitResponseOK(raw) {
			if err == nil {
				err = fmt.Errorf("%w: %s", ErrRejected, bybitResponseMessage(raw))
			}
			step := profileErrorStep(AccountProfileStepOneWayPosition, "bybit", raw, err)
			step.Message = coin + ": " + step.Message
			return step
		}
	}
	return AccountProfileStep(
		AccountProfileStepOneWayPosition, AccountProfileStatusApplied, "",
		"position mode set to one-way for "+strings.Join(ordered, ", "),
	)
}

func (a *bitgetAdapter) ApplyAccountProfile(
	ctx context.Context,
	credentials Credentials,
	_ AccountProfileRequest,
) (AccountProfileResult, error) {
	settings, raw, err := a.bitgetAccountSettings(ctx, credentials)
	if err != nil {
		status, statusRaw, statusErr := a.bitgetUpgradeStatus(ctx, credentials)
		if statusErr != nil {
			step := profileErrorStep(AccountProfileStepUnifiedAccount, "bitget", statusRaw, statusErr)
			return NewAccountProfileResult(
				step,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
			), nil
		}
		switch status.Status {
		case "process":
			return bitgetPendingProfile(status.Reason), nil
		case "success":
			settings, raw, err = a.bitgetAccountSettings(ctx, credentials)
			if err != nil {
				step := profileErrorStep(AccountProfileStepUnifiedAccount, "bitget", raw, err)
				return NewAccountProfileResult(
					step,
					AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
					AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
				), nil
			}
		default:
			raw, err = a.signed(
				ctx, http.MethodPost, "/api/v2/spot/account/upgrade",
				credentials, compactJSON(map[string]any{}), nil,
			)
			if err != nil || !bitgetResponseOK(raw) {
				if err == nil {
					err = fmt.Errorf("%w: %s", ErrRejected, bitgetResponseMessage(raw))
				}
				step := profileErrorStep(AccountProfileStepUnifiedAccount, "bitget", raw, err)
				return NewAccountProfileResult(
					step,
					AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
					AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
				), nil
			}
			return bitgetPendingProfile("account upgrade request accepted"), nil
		}
	}
	unified := AccountProfileStep(
		AccountProfileStepUnifiedAccount, AccountProfileStatusCompliant, "",
		"Unified Trading Account is enabled",
	)
	margin := AccountProfileStep(
		AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusCompliant, "",
		"account mode is already advanced multi-asset",
	)
	if !bitgetAdvanced(settings) {
		raw, err = a.signed(
			ctx, http.MethodPost, "/api/v3/account/adjust-account-mode",
			credentials, compactJSON(map[string]string{"mode": "advanced"}), nil,
		)
		if err != nil || !bitgetResponseOK(raw) {
			if err == nil {
				err = fmt.Errorf("%w: %s", ErrRejected, bitgetResponseMessage(raw))
			}
			margin = profileErrorStep(AccountProfileStepMultiAssetCrossMargin, "bitget", raw, err)
		} else {
			verified, verifyRaw, verifyErr := a.bitgetAccountSettings(ctx, credentials)
			if verifyErr != nil || !bitgetAdvanced(verified) {
				if verifyErr == nil {
					verifyErr = errors.New("account mode remained non-compliant after update")
				}
				margin = profileErrorStep(
					AccountProfileStepMultiAssetCrossMargin, "bitget", verifyRaw, verifyErr,
				)
			} else {
				settings = verified
				margin = AccountProfileStep(
					AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusApplied, "",
					"account mode changed to advanced multi-asset",
				)
			}
		}
	}
	position := AccountProfileStep(
		AccountProfileStepOneWayPosition, AccountProfileStatusCompliant, "",
		"position mode is already one-way",
	)
	if settings.HoldMode != "one_way_mode" {
		raw, err = a.signed(
			ctx, http.MethodPost, "/api/v3/account/set-hold-mode",
			credentials, compactJSON(map[string]string{"holdMode": "one_way_mode"}), nil,
		)
		if err != nil || !bitgetResponseOK(raw) {
			if err == nil {
				err = fmt.Errorf("%w: %s", ErrRejected, bitgetResponseMessage(raw))
			}
			position = profileErrorStep(AccountProfileStepOneWayPosition, "bitget", raw, err)
		} else {
			verified, verifyRaw, verifyErr := a.bitgetAccountSettings(ctx, credentials)
			if verifyErr != nil || verified.HoldMode != "one_way_mode" {
				if verifyErr == nil {
					verifyErr = errors.New("position mode remained non-compliant after update")
				}
				position = profileErrorStep(
					AccountProfileStepOneWayPosition, "bitget", verifyRaw, verifyErr,
				)
			} else {
				position = AccountProfileStep(
					AccountProfileStepOneWayPosition, AccountProfileStatusApplied, "",
					"position mode changed to one-way",
				)
			}
		}
	}
	return NewAccountProfileResult(unified, margin, position), nil
}

type bitgetProfileSettings struct {
	AccountMode  string
	AccountLevel string
	AssetMode    string
	HoldMode     string
}

func (a *bitgetAdapter) bitgetAccountSettings(
	ctx context.Context,
	credentials Credentials,
) (bitgetProfileSettings, []byte, error) {
	raw, err := a.signed(
		ctx, http.MethodGet, "/api/v3/account/settings", credentials, nil, nil,
	)
	if err != nil {
		return bitgetProfileSettings{}, raw, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccountMode  string `json:"accountMode"`
			AccountLevel string `json:"accountLevel"`
			AssetMode    string `json:"assetMode"`
			HoldMode     string `json:"holdMode"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return bitgetProfileSettings{}, raw, err
	}
	if payload.Code != "00000" {
		return bitgetProfileSettings{}, raw, fmt.Errorf(
			"%w: Bitget code %s: %s", ErrRejected, payload.Code, payload.Msg,
		)
	}
	return bitgetProfileSettings{
		AccountMode:  payload.Data.AccountMode,
		AccountLevel: payload.Data.AccountLevel,
		AssetMode:    payload.Data.AssetMode,
		HoldMode:     payload.Data.HoldMode,
	}, raw, nil
}

type bitgetUpgradeState struct {
	Status string
	Reason string
}

func (a *bitgetAdapter) bitgetUpgradeStatus(
	ctx context.Context,
	credentials Credentials,
) (bitgetUpgradeState, []byte, error) {
	raw, err := a.signed(
		ctx, http.MethodGet, "/api/v2/spot/account/upgrade-status",
		credentials, nil, nil,
	)
	if err != nil {
		return bitgetUpgradeState{}, raw, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return bitgetUpgradeState{}, raw, err
	}
	if payload.Code != "00000" {
		return bitgetUpgradeState{}, raw, fmt.Errorf(
			"%w: Bitget code %s: %s", ErrRejected, payload.Code, payload.Msg,
		)
	}
	return bitgetUpgradeState{
		Status: strings.ToLower(payload.Data.Status),
		Reason: payload.Data.Reason,
	}, raw, nil
}

func (a *gateAdapter) ApplyAccountProfile(
	ctx context.Context,
	credentials Credentials,
	request AccountProfileRequest,
) (AccountProfileResult, error) {
	mode, raw, err := a.gateUnifiedMode(ctx, credentials)
	if err != nil {
		step := profileErrorStep(AccountProfileStepUnifiedAccount, "gate", raw, err)
		return NewAccountProfileResult(
			step,
			AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
			AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
		), nil
	}
	accountStatus := AccountProfileStatusCompliant
	accountMessage := "unified account is already in multi-currency mode"
	if mode != "multi_currency" {
		body := compactJSON(map[string]any{
			"mode":     "multi_currency",
			"settings": map[string]bool{"usdt_futures": true},
		})
		raw, err = a.signed(
			ctx, http.MethodPut, "/api/v4/unified/unified_mode", credentials, body, nil,
		)
		if err != nil {
			step := profileErrorStep(AccountProfileStepUnifiedAccount, "gate", raw, err)
			return NewAccountProfileResult(
				step,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(
					AccountProfileStepOneWayPosition, AccountProfileStatusManualRequired, step.Code,
					"unified account mode must be changed before position mode can be updated",
				),
			), nil
		}
		if code, message := gateResponseError(raw); code != "" {
			step := AccountProfileStep(
				AccountProfileStepUnifiedAccount, AccountProfileStatusManualRequired,
				code, message,
			)
			return NewAccountProfileResult(
				step,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(
					AccountProfileStepOneWayPosition, AccountProfileStatusManualRequired, step.Code,
					"unified account mode must be changed before position mode can be updated",
				),
			), nil
		}
		mode, raw, err = a.gateUnifiedMode(ctx, credentials)
		if err != nil || mode != "multi_currency" {
			if err == nil {
				err = errors.New("unified mode remained non-compliant after update")
			}
			step := profileErrorStep(AccountProfileStepUnifiedAccount, "gate", raw, err)
			return NewAccountProfileResult(
				step,
				AccountProfileStep(AccountProfileStepMultiAssetCrossMargin, step.Status, step.Code, step.Message),
				AccountProfileStep(AccountProfileStepOneWayPosition, step.Status, step.Code, step.Message),
			), nil
		}
		accountStatus = AccountProfileStatusApplied
		accountMessage = "unified account changed to multi-currency mode"
	}
	unified := AccountProfileStep(
		AccountProfileStepUnifiedAccount, accountStatus, "", accountMessage,
	)
	margin := AccountProfileStep(
		AccountProfileStepMultiAssetCrossMargin, accountStatus, "", accountMessage,
	)
	position := a.gateApplyOneWay(ctx, credentials, request.Instruments)
	return NewAccountProfileResult(unified, margin, position), nil
}

func (a *gateAdapter) gateUnifiedMode(
	ctx context.Context,
	credentials Credentials,
) (string, []byte, error) {
	raw, err := a.signed(
		ctx, http.MethodGet, "/api/v4/unified/unified_mode", credentials, nil, nil,
	)
	if err != nil {
		return "", raw, err
	}
	var payload struct {
		Mode    string `json:"mode"`
		Label   string `json:"label"`
		Message string `json:"message"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", raw, err
	}
	if payload.Label != "" {
		return "", raw, fmt.Errorf(
			"%w: Gate code %s: %s", ErrRejected, payload.Label, payload.Message,
		)
	}
	return payload.Mode, raw, nil
}

func (a *gateAdapter) gateApplyOneWay(
	ctx context.Context,
	credentials Credentials,
	instruments []Instrument,
) AccountProfileStepResult {
	settles := make(map[string]struct{})
	for _, instrument := range instruments {
		settles[gateSettle(instrument)] = struct{}{}
	}
	if len(settles) == 0 {
		settles["usdt"] = struct{}{}
	}
	ordered := make([]string, 0, len(settles))
	for settle := range settles {
		ordered = append(ordered, settle)
	}
	sort.Strings(ordered)
	changed := false
	for _, settle := range ordered {
		path := "/api/v4/futures/" + settle + "/accounts"
		raw, err := a.signed(ctx, http.MethodGet, path, credentials, nil, nil)
		if err != nil {
			step := profileErrorStep(AccountProfileStepOneWayPosition, "gate", raw, err)
			step.Message = settle + ": " + step.Message
			return step
		}
		if code, message := gateResponseError(raw); code != "" {
			return AccountProfileStep(
				AccountProfileStepOneWayPosition, AccountProfileStatusManualRequired,
				code, settle+": "+message,
			)
		}
		var account struct {
			PositionMode string `json:"position_mode"`
			InDualMode   bool   `json:"in_dual_mode"`
			Label        string `json:"label"`
			Message      string `json:"message"`
		}
		if err := unmarshalJSON(raw, &account); err != nil {
			return profileErrorStep(AccountProfileStepOneWayPosition, "gate", raw, err)
		}
		if account.Label != "" {
			return AccountProfileStep(
				AccountProfileStepOneWayPosition, AccountProfileStatusManualRequired,
				account.Label, settle+": "+account.Message,
			)
		}
		if (account.PositionMode == "" || account.PositionMode == "single") && !account.InDualMode {
			continue
		}
		raw, err = a.signed(
			ctx, http.MethodPost,
			"/api/v4/futures/"+settle+"/set_position_mode",
			credentials, compactJSON(map[string]string{"position_mode": "single"}), nil,
		)
		if err != nil {
			step := profileErrorStep(AccountProfileStepOneWayPosition, "gate", raw, err)
			step.Message = settle + ": " + step.Message
			return step
		}
		verifyRaw, verifyErr := a.signed(
			ctx, http.MethodGet, path, credentials, nil, nil,
		)
		if verifyErr != nil {
			step := profileErrorStep(
				AccountProfileStepOneWayPosition, "gate", verifyRaw, verifyErr,
			)
			step.Message = settle + ": " + step.Message
			return step
		}
		var verified struct {
			PositionMode string `json:"position_mode"`
			InDualMode   bool   `json:"in_dual_mode"`
		}
		if verifyErr = unmarshalJSON(verifyRaw, &verified); verifyErr != nil ||
			(verified.PositionMode != "" && verified.PositionMode != "single") ||
			verified.InDualMode {
			if verifyErr == nil {
				verifyErr = errors.New("position mode remained non-compliant after update")
			}
			step := profileErrorStep(
				AccountProfileStepOneWayPosition, "gate", verifyRaw, verifyErr,
			)
			step.Message = settle + ": " + step.Message
			return step
		}
		changed = true
	}
	status := AccountProfileStatusCompliant
	message := "position mode is already one-way"
	if changed {
		status = AccountProfileStatusApplied
		message = "position mode changed to one-way for " + strings.Join(ordered, ", ")
	}
	return AccountProfileStep(AccountProfileStepOneWayPosition, status, "", message)
}

func profileErrorStep(step, venue string, raw []byte, err error) AccountProfileStepResult {
	code, message := profileErrorDetails(raw, err)
	status := AccountProfileStatusFailed
	if errors.Is(err, ErrRejected) {
		status = AccountProfileStatusManualRequired
	}
	if message == "" {
		message = venue + " account setting request failed"
	}
	return AccountProfileStep(step, status, code, message)
}

func profileErrorDetails(raw []byte, err error) (string, string) {
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	code := firstNonEmpty(
		mapString(payload, "code"), mapString(payload, "retCode"), mapString(payload, "label"),
	)
	message := firstNonEmpty(
		mapString(payload, "msg"), mapString(payload, "message"), mapString(payload, "retMsg"),
	)
	if message == "" {
		message = profileMessage(err)
	}
	return code, profileSafeMessage(message)
}

func profileMessage(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}

func profileSafeMessage(message string) string {
	message = strings.TrimSpace(message)
	lower := strings.ToLower(message)
	for _, secret := range []string{
		"apikey", "api-key", "api key", "secret", "passphrase", "signature",
	} {
		if strings.Contains(lower, secret) {
			return "venue account setting request failed"
		}
	}
	if len(message) > 400 {
		return message[:400]
	}
	return message
}

func mapString(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return fmt.Sprint(typed)
	}
}

func binanceCodeMessage(raw []byte) (string, string) {
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if len(raw) == 0 || unmarshalJSON(raw, &payload) != nil {
		return "", ""
	}
	return strconv.Itoa(payload.Code), payload.Msg
}

func okxResponseOK(raw []byte) bool {
	var payload struct {
		Code string `json:"code"`
	}
	return unmarshalJSON(raw, &payload) == nil && payload.Code == "0"
}

func okxResponseMessage(raw []byte) string {
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = unmarshalJSON(raw, &payload)
	return strings.TrimSpace("OKX code " + payload.Code + ": " + payload.Msg)
}

func okxUnmatchedReason(raw []byte) string {
	var payload struct {
		Data []struct {
			UnmatchedInfo []any `json:"unmatchedInfo"`
		} `json:"data"`
	}
	if unmarshalJSON(raw, &payload) != nil {
		return ""
	}
	for _, item := range payload.Data {
		if len(item.UnmatchedInfo) > 0 {
			encoded, _ := json.Marshal(item.UnmatchedInfo)
			return "OKX account switch precheck failed: " + string(encoded)
		}
	}
	return ""
}

func bybitResponseOK(raw []byte) bool {
	var payload struct {
		RetCode int `json:"retCode"`
	}
	return unmarshalJSON(raw, &payload) == nil && payload.RetCode == 0
}

func bybitResponseMessage(raw []byte) string {
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			Reasons []struct {
				Code    string `json:"reasonCode"`
				Message string `json:"reasonMsg"`
			} `json:"reasons"`
		} `json:"result"`
	}
	_ = unmarshalJSON(raw, &payload)
	reasons := make([]string, 0, len(payload.Result.Reasons))
	for _, reason := range payload.Result.Reasons {
		reasons = append(reasons, strings.TrimSpace(reason.Code+" "+reason.Message))
	}
	if len(reasons) > 0 {
		return strings.Join(reasons, "; ")
	}
	return strings.TrimSpace(fmt.Sprintf("Bybit code %d: %s", payload.RetCode, payload.RetMsg))
}

func bybitUpgradeResult(raw []byte) (string, string, string) {
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			Status  string `json:"unifiedUpdateStatus"`
			Message struct {
				Items []string `json:"msg"`
			} `json:"unifiedUpdateMsg"`
		} `json:"result"`
	}
	if unmarshalJSON(raw, &payload) != nil {
		return "", "", ""
	}
	message := payload.RetMsg
	if len(payload.Result.Message.Items) > 0 {
		message = strings.Join(payload.Result.Message.Items, "; ")
	}
	return strings.ToUpper(payload.Result.Status), strconv.Itoa(payload.RetCode), message
}

func bitgetResponseOK(raw []byte) bool {
	var payload struct {
		Code string `json:"code"`
	}
	return unmarshalJSON(raw, &payload) == nil && payload.Code == "00000"
}

func bitgetResponseMessage(raw []byte) string {
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = unmarshalJSON(raw, &payload)
	return strings.TrimSpace("Bitget code " + payload.Code + ": " + payload.Msg)
}

func bitgetAdvanced(settings bitgetProfileSettings) bool {
	accountMode := strings.ToLower(settings.AccountMode)
	accountLevel := strings.ToLower(settings.AccountLevel)
	assetMode := strings.ToLower(settings.AssetMode)
	return accountMode == "unified" && accountLevel == "advanced" &&
		(assetMode == "multi_assets" || assetMode == "multi_asset")
}

func bitgetPendingProfile(reason string) AccountProfileResult {
	if strings.TrimSpace(reason) == "" {
		reason = "Bitget Unified Trading Account upgrade is processing"
	}
	return NewAccountProfileResult(
		AccountProfileStep(AccountProfileStepUnifiedAccount, AccountProfileStatusPending, "", reason),
		AccountProfileStep(
			AccountProfileStepMultiAssetCrossMargin, AccountProfileStatusPending, "",
			"waiting for Unified Trading Account upgrade",
		),
		AccountProfileStep(
			AccountProfileStepOneWayPosition, AccountProfileStatusPending, "",
			"waiting for Unified Trading Account upgrade",
		),
	)
}

func gateResponseError(raw []byte) (string, string) {
	var payload struct {
		Label   string `json:"label"`
		Message string `json:"message"`
	}
	if unmarshalJSON(raw, &payload) != nil {
		return "", ""
	}
	return payload.Label, profileSafeMessage(payload.Message)
}
