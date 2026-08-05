package service

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrSD2ChannelSafetyConfig     = errors.New("wxmaas channel safety configuration is invalid")
	ErrSD2ChannelCreateDisabled   = errors.New("wxmaas channel creation is disabled")
	ErrSD2ChannelConcurrencyLimit = errors.New("wxmaas channel concurrency limit reached")
	ErrSD2ChannelDailyBudget      = errors.New("wxmaas channel daily budget reached")
)

type WxmaasChannelSafetySettings struct {
	CreateEnabled                bool
	PollEnabled                  bool
	MaxConcurrency               int
	MaxTaskCostMicrounitsCNY     int64
	HardDailyBudgetMicrounitsCNY int64
}

func ParseWxmaasChannelSafetySettings(channel *model.Channel) (WxmaasChannelSafetySettings, error) {
	if channel == nil || channel.Type != constant.ChannelTypeWxmaasSeedance {
		return WxmaasChannelSafetySettings{}, fmt.Errorf("%w: wxmaas channel is required", ErrSD2ChannelSafetyConfig)
	}
	var setting dto.ChannelSettings
	if channel.Setting == nil || *channel.Setting == "" {
		return WxmaasChannelSafetySettings{}, fmt.Errorf("%w: explicit safety settings are required", ErrSD2ChannelSafetyConfig)
	}
	if err := common.Unmarshal([]byte(*channel.Setting), &setting); err != nil {
		return WxmaasChannelSafetySettings{}, fmt.Errorf("%w: malformed channel setting", ErrSD2ChannelSafetyConfig)
	}
	if err := ValidateWxmaasChannelSafetySettings(setting); err != nil {
		return WxmaasChannelSafetySettings{}, err
	}
	return WxmaasChannelSafetySettings{
		CreateEnabled:                *setting.CreateEnabled,
		PollEnabled:                  *setting.PollEnabled,
		MaxConcurrency:               setting.MaxConcurrency,
		MaxTaskCostMicrounitsCNY:     setting.MaxTaskCostMicrounitsCNY,
		HardDailyBudgetMicrounitsCNY: setting.HardDailyBudgetMicrounitsCNY,
	}, nil
}

func ValidateWxmaasChannelSafetySettings(setting dto.ChannelSettings) error {
	if setting.CreateEnabled == nil || setting.PollEnabled == nil {
		return fmt.Errorf("%w: create_enabled and poll_enabled must be explicit", ErrSD2ChannelSafetyConfig)
	}
	if setting.MaxConcurrency != 1 {
		return fmt.Errorf("%w: max_concurrency must equal 1", ErrSD2ChannelSafetyConfig)
	}
	if setting.MaxTaskCostMicrounitsCNY <= 0 {
		return fmt.Errorf("%w: max_task_cost_microunits_cny must be positive", ErrSD2ChannelSafetyConfig)
	}
	if setting.HardDailyBudgetMicrounitsCNY <= 0 || setting.HardDailyBudgetMicrounitsCNY < setting.MaxTaskCostMicrounitsCNY {
		return fmt.Errorf("%w: hard_daily_budget_microunits_cny must be positive and at least the single-task limit", ErrSD2ChannelSafetyConfig)
	}
	return nil
}

func WxmaasChannelPollingAllowed(channel *model.Channel) (bool, error) {
	settings, err := ParseWxmaasChannelSafetySettings(channel)
	if err != nil {
		return false, err
	}
	return settings.PollEnabled, nil
}

func lockWxmaasChannelSafetyTx(tx *gorm.DB, channelID int) (*model.Channel, WxmaasChannelSafetySettings, error) {
	if tx == nil || channelID <= 0 {
		return nil, WxmaasChannelSafetySettings{}, fmt.Errorf("%w: invalid channel", ErrSD2ChannelSafetyConfig)
	}
	var channel model.Channel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", channelID).First(&channel).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, WxmaasChannelSafetySettings{}, fmt.Errorf("%w: channel not found", ErrSD2ChannelSafetyConfig)
		}
		return nil, WxmaasChannelSafetySettings{}, err
	}
	settings, err := ParseWxmaasChannelSafetySettings(&channel)
	return &channel, settings, err
}

type wxmaasDailyExposure struct {
	ActiveCount   int64
	PotentialCost int64
	RecordedCost  int64
}

func wxmaasDailyExposureTx(tx *gorm.DB, channelID int, now time.Time) (wxmaasDailyExposure, error) {
	// UTC keeps the budget boundary deterministic across replicas. Concurrency
	// is intentionally global and therefore does not reset at this boundary.
	dayStart := now.UTC().Truncate(24 * time.Hour).Unix()
	dayEnd := time.Unix(dayStart, 0).UTC().Add(24 * time.Hour).Unix()
	activeStates := []model.TaskSubmissionState{
		model.TaskSubmissionStatePrepared,
		model.TaskSubmissionStateSending,
		model.TaskSubmissionStateUnknown,
		model.TaskSubmissionStateConfirmed,
	}
	activeQuery := tx.Model(&model.TaskSubmission{}).
		Where("channel_id = ?", channelID).
		Where("state IN ? AND provider_cost_state <> ?", activeStates, model.TaskSubmissionProviderCostRecorded)
	var exposure wxmaasDailyExposure
	if err := activeQuery.Count(&exposure.ActiveCount).Error; err != nil {
		return exposure, err
	}
	if err := tx.Model(&model.TaskSubmission{}).
		Where("channel_id = ? AND created_at >= ? AND created_at < ?", channelID, dayStart, dayEnd).
		Where("state IN ? AND provider_cost_state <> ?", activeStates, model.TaskSubmissionProviderCostRecorded).
		Select("COALESCE(SUM(provider_cost_potential_microunits), 0)").Scan(&exposure.PotentialCost).Error; err != nil {
		return exposure, err
	}
	if err := tx.Model(&model.TaskSubmission{}).
		Where("channel_id = ? AND created_at >= ? AND created_at < ? AND provider_cost_state = ?", channelID, dayStart, dayEnd, model.TaskSubmissionProviderCostRecorded).
		Select("COALESCE(SUM(COALESCE(provider_cost_microunits, provider_cost_potential_microunits)), 0)").Scan(&exposure.RecordedCost).Error; err != nil {
		return exposure, err
	}
	return exposure, nil
}

func reserveWxmaasChannelSafetyTx(tx *gorm.DB, submission *model.TaskSubmission) error {
	if submission.ChannelType != constant.ChannelTypeWxmaasSeedance {
		return nil
	}
	_, settings, err := lockWxmaasChannelSafetyTx(tx, submission.ChannelID)
	if err != nil {
		return err
	}
	if !settings.CreateEnabled {
		return ErrSD2ChannelCreateDisabled
	}
	exposure, err := wxmaasDailyExposureTx(tx, submission.ChannelID, time.Now())
	if err != nil {
		return err
	}
	if exposure.ActiveCount >= int64(settings.MaxConcurrency) {
		return ErrSD2ChannelConcurrencyLimit
	}
	if exposure.PotentialCost < 0 || exposure.RecordedCost < 0 || exposure.PotentialCost > math.MaxInt64-exposure.RecordedCost {
		return ErrSD2ChannelDailyBudget
	}
	used := exposure.PotentialCost + exposure.RecordedCost
	if used > settings.HardDailyBudgetMicrounitsCNY-settings.MaxTaskCostMicrounitsCNY {
		return ErrSD2ChannelDailyBudget
	}
	submission.ProviderCostPotentialMicrounits = settings.MaxTaskCostMicrounitsCNY
	return nil
}

func checkWxmaasChannelSafetyBeforeSendTx(tx *gorm.DB, submission *model.TaskSubmission) error {
	if submission == nil || submission.ChannelType != constant.ChannelTypeWxmaasSeedance {
		return nil
	}
	_, settings, err := lockWxmaasChannelSafetyTx(tx, submission.ChannelID)
	if err != nil {
		return err
	}
	if !settings.CreateEnabled {
		return ErrSD2ChannelCreateDisabled
	}
	if submission.ProviderCostPotentialMicrounits <= 0 || submission.ProviderCostPotentialMicrounits != settings.MaxTaskCostMicrounitsCNY {
		return fmt.Errorf("%w: frozen task limit no longer matches channel configuration", ErrSD2ChannelSafetyConfig)
	}
	exposure, err := wxmaasDailyExposureTx(tx, submission.ChannelID, time.Now())
	if err != nil {
		return err
	}
	if exposure.ActiveCount > int64(settings.MaxConcurrency) {
		return ErrSD2ChannelConcurrencyLimit
	}
	if exposure.PotentialCost < 0 || exposure.RecordedCost < 0 || exposure.PotentialCost > math.MaxInt64-exposure.RecordedCost {
		return ErrSD2ChannelDailyBudget
	}
	used := exposure.PotentialCost + exposure.RecordedCost
	if used > settings.HardDailyBudgetMicrounitsCNY {
		return ErrSD2ChannelDailyBudget
	}
	return nil
}

func IsSD2ChannelSafetyError(err error) bool {
	return isSD2ChannelSafetyError(err)
}
