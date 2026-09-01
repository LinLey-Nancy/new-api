package service

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	BillingSourceWallet       = "wallet"
	BillingSourceSubscription = "subscription"
	BillingSourceManagedToken = "managed_token"
)

// PreConsumeBilling 根据用户计费偏好创建 BillingSession 并执行预扣费。
// 会话存储在 relayInfo.Billing 上，供后续 Settle / Refund 使用。
func PreConsumeBilling(c *gin.Context, preConsumedQuota int, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	if relayInfo != nil && relayInfo.QuotaClamp != nil {
		return types.NewErrorWithStatusCode(
			relayInfo.QuotaClamp,
			types.ErrorCodeModelPriceError,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if preConsumedQuota < 0 {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("pre-consume quota cannot be negative: %d", preConsumedQuota),
			types.ErrorCodeModelPriceError,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	session, apiErr := NewBillingSession(c, relayInfo, preConsumedQuota)
	if apiErr != nil {
		return apiErr
	}
	relayInfo.Billing = session
	return nil
}

// ---------------------------------------------------------------------------
// SettleBilling — 后结算辅助函数
// ---------------------------------------------------------------------------

// SettleBilling 执行计费结算。如果 RelayInfo 上有 BillingSession 则通过 session 结算，
// 否则回退到旧的 PostConsumeQuota 路径（兼容按次计费等场景）。
func SettleBilling(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, actualQuota int) error {
	var billingErr error
	if relayInfo.Billing != nil {
		preConsumed := relayInfo.Billing.GetPreConsumedQuota()
		delta := actualQuota - preConsumed

		if delta > 0 {
			logger.LogInfo(ctx, fmt.Sprintf("预扣费后补扣费：%s（实际消耗：%s，预扣费：%s）",
				logger.FormatQuota(delta),
				logger.FormatQuota(actualQuota),
				logger.FormatQuota(preConsumed),
			))
		} else if delta < 0 {
			logger.LogInfo(ctx, fmt.Sprintf("预扣费后返还扣费：%s（实际消耗：%s，预扣费：%s）",
				logger.FormatQuota(-delta),
				logger.FormatQuota(actualQuota),
				logger.FormatQuota(preConsumed),
			))
		} else {
			logger.LogInfo(ctx, fmt.Sprintf("预扣费与实际消耗一致，无需调整：%s（按次计费）",
				logger.FormatQuota(actualQuota),
			))
		}

		billingErr = relayInfo.Billing.Settle(actualQuota)

		// 发送额度通知（订阅计费使用订阅剩余额度）
		if billingErr == nil && actualQuota != 0 && relayInfo.BillingSource != BillingSourceManagedToken {
			if relayInfo.BillingSource == BillingSourceSubscription {
				checkAndSendSubscriptionQuotaNotify(relayInfo)
			} else {
				checkAndSendQuotaNotify(relayInfo, actualQuota-preConsumed, preConsumed)
			}
		}
		usageErr := settleManagedUsageReservation(ctx)
		if billingErr != nil {
			return billingErr
		}
		return usageErr
	}

	// 回退：无 BillingSession 时使用旧路径
	quotaDelta := actualQuota - relayInfo.FinalPreConsumedQuota
	if quotaDelta != 0 {
		billingErr = PostConsumeQuota(relayInfo, quotaDelta, relayInfo.FinalPreConsumedQuota, true)
	}
	usageErr := settleManagedUsageReservation(ctx)
	if billingErr != nil {
		return billingErr
	}
	return usageErr
}

func settleManagedUsageReservation(ctx *gin.Context) error {
	reservation := takeManagedUsageReservation(ctx)
	if reservation == nil {
		return nil
	}
	actualOutputTokens := common.GetContextKeyInt(ctx, constant.ContextKeyManagedOutputTokens)
	return common.ReconcileManagedTokenLimits(ctx.Request.Context(), reservation, actualOutputTokens)
}

// CancelManagedUsageReservation releases a worst-case output reservation when
// a request exits before actual output usage can be settled. It is safe to
// defer this on every managed request because successful settlement removes
// the reservation from the request context first.
func CancelManagedUsageReservation(ctx *gin.Context) {
	reservation := takeManagedUsageReservation(ctx)
	if reservation == nil {
		return
	}
	if err := common.ReconcileManagedTokenLimits(ctx.Request.Context(), reservation, 0); err != nil {
		common.SysLog("failed to release managed output token reservation: " + err.Error())
	}
}

func takeManagedUsageReservation(ctx *gin.Context) *common.ManagedUsageReservation {
	if ctx == nil {
		return nil
	}
	value, exists := common.GetContextKey(ctx, constant.ContextKeyManagedUsageReservation)
	if !exists || value == nil {
		return nil
	}
	reservation, ok := value.(*common.ManagedUsageReservation)
	if !ok || reservation == nil {
		return nil
	}
	common.SetContextKey(ctx, constant.ContextKeyManagedUsageReservation, nil)
	return reservation
}
