package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// SubmissionFundingCandidates preserves the site's billing preference while
// allowing each candidate to be reserved atomically by PrepareTaskSubmission.
// Falling back here changes only the user's authorized funding source; it never
// changes provider/channel and always happens before T2.
func SubmissionFundingCandidates(userID int, preference string) ([]string, error) {
	switch common.NormalizeBillingPreference(preference) {
	case "wallet_only":
		return []string{BillingSourceWallet}, nil
	case "subscription_only":
		return []string{BillingSourceSubscription}, nil
	case "wallet_first":
		hasSubscription, err := model.HasActiveUserSubscription(userID)
		if err != nil {
			return nil, err
		}
		if hasSubscription {
			return []string{BillingSourceWallet, BillingSourceSubscription}, nil
		}
		return []string{BillingSourceWallet}, nil
	case "subscription_first":
		fallthrough
	default:
		hasSubscription, err := model.HasActiveUserSubscription(userID)
		if err != nil {
			return nil, err
		}
		if !hasSubscription {
			return []string{BillingSourceWallet}, nil
		}
		allowWallet, err := model.UserActiveSubscriptionsAllowWalletOverflow(userID)
		if err != nil {
			return nil, err
		}
		if allowWallet {
			return []string{BillingSourceSubscription, BillingSourceWallet}, nil
		}
		return []string{BillingSourceSubscription}, nil
	}
}
