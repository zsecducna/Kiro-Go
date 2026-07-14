package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

func isQuotaErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	// Match the 429 status code only as a digit-boundary token (parity with
	// pool.HasStatusToken used by the auth classifier) so a stray "429" inside
	// an upstream body token/ID can't false-trigger RecordError(true). "quota"
	// remains a word marker.
	return pool.HasStatusToken(lower, "429") || strings.Contains(lower, "quota")
}

func isOverageErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	// 402 must be a digit-boundary token, not an arbitrary substring (parity
	// with pool.HasStatusToken), ANDed with the "overage" word marker.
	return pool.HasStatusToken(lower, "402") && strings.Contains(lower, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

// shouldRetryAccountRefreshOnError reports whether a RefreshAccountInfo error
// looks like a stale/invalid token worth one token-refresh + retry in the admin
// "refresh account" endpoint (handler.go). This is a RETRY trigger, NOT a ban
// classifier — a false positive only costs a redundant refresh + retry. Status
// codes 401/403 are matched by digit boundary (pool.HasStatusToken) for parity
// with the ban classifiers, so a stray digit in a request ID/token can't fire a
// spurious refresh+retry; "invalid"/"expired" remain word markers.
func shouldRetryAccountRefreshOnError(msg string) bool {
	lower := strings.ToLower(msg)
	return pool.HasStatusToken(lower, "401") || pool.HasStatusToken(lower, "403") ||
		strings.Contains(lower, "invalid") || strings.Contains(lower, "expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case pool.IsAuthFailure(err):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
