package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

// CenterModelRevision serializes model-ops writes across gateway replicas. The
// configuration digest still includes the actual channel and pricing state, so
// changes made through the ordinary admin UI invalidate a stale caller too.
type CenterModelRevision struct {
	ModelID  string `gorm:"primaryKey;size:128"`
	Sequence uint64 `gorm:"not null"`
}

var ErrCenterModelRevision = errors.New("model configuration revision changed")

// ApplyCenterModelBilling changes only one model's gateway-owned price. A
// durable request record is committed with the option maps; repeating the
// same request cannot charge or overwrite another administrator's revision.
// The supplied check is called after locking the model revision row.
func ApplyCenterModelBilling(modelID, requestID, payloadHash string, check func(uint64) bool, mode, expression string, price *float64) (bool, error) {
	if price != nil && (math.IsNaN(*price) || math.IsInf(*price, 0) || *price < 0 || *price > 1000) {
		return false, fmt.Errorf("invalid per-request price")
	}
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation CenterOperation
		err := tx.Where("request_id = ?", requestID).Take(&operation).Error
		if err == nil {
			if operation.OrganizationID != "model-ops" || operation.Kind != "billing" || operation.PayloadHash != payloadHash || operation.Actor != modelID {
				return ErrCenterOperationConflict
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var revision CenterModelRevision
		err = lockForUpdate(tx).Where("model_id = ?", modelID).Take(&revision).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			revision = CenterModelRevision{ModelID: modelID}
			if err = tx.Create(&revision).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !check(revision.Sequence) {
			return ErrCenterModelRevision
		}
		keys := []string{"billing_setting.billing_mode", "billing_setting.billing_expr", "ModelPrice"}
		values := make(map[string]string, len(keys))
		for _, key := range keys {
			var option Option
			err := lockForUpdate(tx).Where("key = ?", key).Take(&option).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				var initial any
				switch key {
				case "billing_setting.billing_mode":
					initial = billing_setting.GetBillingModeCopy()
				case "billing_setting.billing_expr":
					initial = billing_setting.GetBillingExprCopy()
				case "ModelPrice":
					initial = ratio_setting.GetModelPriceCopy()
				}
				encoded, marshalErr := common.Marshal(initial)
				if marshalErr != nil {
					return marshalErr
				}
				option = Option{Key: key, Value: string(encoded)}
				if err = tx.Create(&option).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			values[key] = option.Value
		}
		modes := map[string]string{}
		expressions := map[string]string{}
		prices := map[string]float64{}
		if err := common.Unmarshal([]byte(values[keys[0]]), &modes); err != nil {
			return err
		}
		if err := common.Unmarshal([]byte(values[keys[1]]), &expressions); err != nil {
			return err
		}
		if err := common.Unmarshal([]byte(values[keys[2]]), &prices); err != nil {
			return err
		}
		if modes == nil || expressions == nil || prices == nil {
			return fmt.Errorf("billing option map is unavailable")
		}
		if mode == billing_setting.BillingModeTieredExpr {
			modes[modelID] = mode
			expressions[modelID] = expression
			delete(prices, modelID)
		} else {
			modes[modelID] = billing_setting.BillingModeRatio
			delete(expressions, modelID)
			prices[modelID] = *price
		}
		for key, value := range map[string]any{keys[0]: modes, keys[1]: expressions, keys[2]: prices} {
			encoded, err := common.Marshal(value)
			if err != nil {
				return err
			}
			if err := tx.Model(&Option{}).Where("key = ?", key).Update("value", string(encoded)).Error; err != nil {
				return err
			}
			values[key] = string(encoded)
		}
		changed := tx.Model(&CenterModelRevision{}).Where("model_id = ? AND sequence = ?", modelID, revision.Sequence).
			Update("sequence", revision.Sequence+1)
		if changed.Error != nil {
			return changed.Error
		}
		if changed.RowsAffected != 1 {
			return ErrCenterModelRevision
		}
		if err := tx.Create(&CenterOperation{RequestID: requestID, OrganizationID: "model-ops", Kind: "billing", PayloadHash: payloadHash, Actor: modelID, CreatedAt: time.Now().UTC()}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil || replayed {
		return replayed, err
	}
	// Reload from the committed database values rather than from a stale copy of
	// the global maps. Other gateway replicas synchronize options normally.
	loadOptionsFromDatabase()
	RefreshPricing()
	return false, nil
}

func CenterBillingPayloadHash(modelID, mode, expression string, price *float64) string {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%v", modelID, mode, expression, price)
	if price != nil {
		value = fmt.Sprintf("%s\x00%s\x00%s\x00%.12g", modelID, mode, expression, *price)
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
