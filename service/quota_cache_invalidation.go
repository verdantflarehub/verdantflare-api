package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"

	"github.com/bytedance/gopkg/util/gopool"
)

const (
	quotaCacheInvalidationInterval  = 2 * time.Second
	quotaCacheInvalidationLease     = 30 * time.Second
	quotaCacheInvalidationBatchSize = 100
	submissionRecoveryInterval      = 15 * time.Second
	stalePreparedSubmissionAge      = 10 * time.Minute
)

var quotaCacheInvalidationWorkerOnce sync.Once

// StartQuotaCacheInvalidationOutboxWorker starts the recoverable cache side of
// submission billing. Database balances remain authoritative while an event is
// pending, so a delayed worker cannot authorize an overspend.
func StartQuotaCacheInvalidationOutboxWorker() {
	quotaCacheInvalidationWorkerOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		workerID := fmt.Sprintf("%s-quota-outbox-%s", common.NodeName, common.GetRandomString(8))
		gopool.Go(func() {
			ctx := context.Background()
			ticker := time.NewTicker(quotaCacheInvalidationInterval)
			defer ticker.Stop()
			var lastRecovery time.Time
			for {
				if _, err := ProcessQuotaCacheInvalidationOutbox(ctx, workerID, quotaCacheInvalidationBatchSize); err != nil {
					logger.LogWarn(ctx, fmt.Sprintf("quota cache invalidation outbox failed: %v", err))
				}
				if time.Since(lastRecovery) >= submissionRecoveryInterval {
					lastRecovery = time.Now()
					if _, err := SweepStaleSendingTaskSubmissions(quotaCacheInvalidationBatchSize); err != nil {
						logger.LogWarn(ctx, fmt.Sprintf("stale sending submission recovery failed: %v", err))
					}
					preparedBefore := common.GetTimestamp() - int64(stalePreparedSubmissionAge.Seconds())
					if _, err := SweepStalePreparedTaskSubmissions(preparedBefore, quotaCacheInvalidationBatchSize); err != nil {
						logger.LogWarn(ctx, fmt.Sprintf("stale prepared submission recovery failed: %v", err))
					}
				}
				<-ticker.C
			}
		})
	})
}

// ProcessQuotaCacheInvalidationOutbox processes at most limit events. Event
// delivery is at-least-once; deleting these caches is intentionally idempotent.
func ProcessQuotaCacheInvalidationOutbox(ctx context.Context, workerID string, limit int) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if workerID == "" {
		return 0, errors.New("quota cache invalidation worker id is required")
	}
	now := common.GetTimestamp()
	events, err := model.ClaimQuotaCacheInvalidationOutbox(limit, workerID, now+int64(quotaCacheInvalidationLease.Seconds()))
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		err := invalidateQuotaAggregate(event)
		if err == nil {
			if err := model.CompleteQuotaCacheInvalidationOutbox(event.ID, workerID); err != nil {
				return processed, err
			}
			processed++
			continue
		}

		backoff := time.Second << min(event.Attempts, 8)
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		safeError := truncateSafeText(err.Error(), 512)
		if releaseErr := model.ReleaseQuotaCacheInvalidationOutbox(event.ID, workerID, common.GetTimestamp()+int64(backoff.Seconds()), safeError); releaseErr != nil {
			return processed, errors.Join(err, releaseErr)
		}
	}
	return processed, nil
}

func invalidateQuotaAggregate(event *model.QuotaCacheInvalidationOutbox) error {
	if event == nil || event.AggregateID <= 0 {
		return errors.New("invalid quota cache aggregate")
	}
	switch event.AggregateType {
	case model.QuotaCacheAggregateUserBilling:
		userID := int(event.AggregateID)
		if int64(userID) != event.AggregateID {
			return errors.New("quota cache aggregate id overflow")
		}
		if err := model.InvalidateUserCache(userID); err != nil {
			return err
		}
		return model.InvalidateUserTokensCache(userID)
	default:
		return fmt.Errorf("unsupported quota cache aggregate type: %s", event.AggregateType)
	}
}
