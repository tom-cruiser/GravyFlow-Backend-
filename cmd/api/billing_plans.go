package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// Self-service subscription plans
//
// There is no payment processor wired into this codebase (no Stripe or
// equivalent, no API keys in .env) — this is deliberately a quota-tier
// switcher, not a checkout flow. "Upgrading" instantly changes the caller's
// own quota limits (reusing resources.go's existing UpdateQuota engine, the
// same one adminUpdateQuotaHandler and adminRestoreDefaultQuotaHandler in
// admin_billing.go already use) and records the choice for display, but no
// money moves and no card is ever collected. The frontend must say so
// explicitly rather than implying a real charge — see
// app/dashboard/settings/plan-picker.tsx.
// ============================================================================

// BillingPlan is a fixed, server-authoritative preset — the client only ever
// sends a plan key (see UpgradePlanRequest), never quota numbers, so a
// tampered request can select a plan but can't grant arbitrary resources.
type BillingPlan struct {
	Key            string  `json:"key"`
	Name           string  `json:"name"`
	PriceDisplay   string  `json:"priceDisplay"`
	MaxCPU         float64 `json:"maxCpu"`
	MaxMemoryMB    int64   `json:"maxMemoryMb"`
	MaxApps        int64   `json:"maxApps"`
	MaxStorageMB   int64   `json:"maxStorageMb"`
	MaxBandwidthGB int64   `json:"maxBandwidthGb"`
}

// billingPlans is the full catalog. "free" intentionally matches
// defaultMax* (resources.go) exactly — it's what EnsureResourceAccounting
// already gives every new signup, so picking "Free" here is a no-op rather
// than a surprise change.
var billingPlans = []BillingPlan{
	{
		Key: "free", Name: "Free", PriceDisplay: "$0/mo",
		MaxCPU: defaultMaxCPU, MaxMemoryMB: defaultMaxMemoryMB, MaxApps: defaultMaxApps,
		MaxStorageMB: defaultMaxStorageMB, MaxBandwidthGB: defaultMaxBandwidthGB,
	},
	{
		Key: "pro", Name: "Pro", PriceDisplay: "$29/mo",
		MaxCPU: 8, MaxMemoryMB: 8192, MaxApps: 15,
		MaxStorageMB: 20480, MaxBandwidthGB: 1000,
	},
	{
		Key: "business", Name: "Business", PriceDisplay: "$99/mo",
		MaxCPU: 32, MaxMemoryMB: 32768, MaxApps: 100,
		MaxStorageMB: 102400, MaxBandwidthGB: 5000,
	},
}

func findBillingPlan(key string) (BillingPlan, bool) {
	for _, p := range billingPlans {
		if p.Key == key {
			return p, true
		}
	}
	return BillingPlan{}, false
}

// listBillingPlansHandler is the single source of truth for plan names,
// pricing display strings, and limits, so the frontend never hardcodes
// numbers that could drift from what upgradeBillingPlanHandler actually
// grants.
func listBillingPlansHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"plans": billingPlans})
}

type UpgradePlanRequest struct {
	Plan string `json:"plan" binding:"required"`
}

// upgradeBillingPlanHandler switches the CALLER's own plan/quota tier — any
// authenticated user, not just admins, since this only ever acts on the
// caller's own account. It deliberately does not guard against "downgrading"
// below current usage (e.g. Business → Free while over the Free app limit):
// that's a real product decision (block it? grace-period? force cleanup
// first?) this simple picker doesn't attempt to make.
func upgradeBillingPlanHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req UpgradePlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	plan, ok := findBillingPlan(strings.ToLower(strings.TrimSpace(req.Plan)))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown_plan", "details": "plan must be one of: free, pro, business"})
		return
	}

	previous, err := deploymentStore.GetQuota(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_quota", "details": err.Error()})
		return
	}

	maxCPU, maxMemory, maxApps, maxStorage, maxBandwidth := plan.MaxCPU, plan.MaxMemoryMB, plan.MaxApps, plan.MaxStorageMB, plan.MaxBandwidthGB
	record, err := deploymentStore.UpdateQuota(c.Request.Context(), user.ID, &maxCPU, &maxMemory, &maxApps, &maxStorage, &maxBandwidth, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_update_quota", "details": err.Error()})
		return
	}

	if err := deploymentStore.SetQuotaPlan(c.Request.Context(), user.ID, plan.Key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_set_plan", "details": err.Error()})
		return
	}
	record.Plan = plan.Key

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "user.plan.changed", "user", user.ID, map[string]any{"previousPlan": previous.Plan, "newPlan": plan.Key}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for user.plan.changed on %q: %v\n", user.ID, err)
	}

	c.JSON(http.StatusOK, record)
}
