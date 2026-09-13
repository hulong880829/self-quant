package exchange

import "context"

const (
	AccountProfileStepUnifiedAccount        = "unified_account"
	AccountProfileStepMultiAssetCrossMargin = "multi_asset_cross_margin"
	AccountProfileStepOneWayPosition        = "one_way_position"

	AccountProfileStatusCompliant      = "compliant"
	AccountProfileStatusApplied        = "applied"
	AccountProfileStatusPending        = "pending"
	AccountProfileStatusManualRequired = "manual_required"
	AccountProfileStatusFailed         = "failed"
)

// AccountProfileRequest describes the venue products whose account-level
// position settings must be compatible with the trader catalog.
type AccountProfileRequest struct {
	Instruments []Instrument
}

type AccountProfileStepResult struct {
	Step    string
	Status  string
	Code    string
	Message string
}

type AccountProfileResult struct {
	OverallStatus string
	Steps         []AccountProfileStepResult
}

// AccountProfileManager is implemented only by venues for which the trader can
// safely inspect and best-effort apply the supported account-level settings.
type AccountProfileManager interface {
	ApplyAccountProfile(context.Context, Credentials, AccountProfileRequest) (AccountProfileResult, error)
}

func NewAccountProfileResult(steps ...AccountProfileStepResult) AccountProfileResult {
	result := AccountProfileResult{Steps: steps, OverallStatus: AccountProfileStatusCompliant}
	for _, step := range steps {
		switch step.Status {
		case AccountProfileStatusFailed:
			result.OverallStatus = AccountProfileStatusFailed
			return result
		case AccountProfileStatusManualRequired:
			if result.OverallStatus != AccountProfileStatusFailed {
				result.OverallStatus = AccountProfileStatusManualRequired
			}
		case AccountProfileStatusPending:
			if result.OverallStatus == AccountProfileStatusCompliant ||
				result.OverallStatus == AccountProfileStatusApplied {
				result.OverallStatus = AccountProfileStatusPending
			}
		case AccountProfileStatusApplied:
			if result.OverallStatus == AccountProfileStatusCompliant {
				result.OverallStatus = AccountProfileStatusApplied
			}
		}
	}
	return result
}

func AccountProfileStep(step, status, code, message string) AccountProfileStepResult {
	return AccountProfileStepResult{
		Step: step, Status: status, Code: code, Message: message,
	}
}
