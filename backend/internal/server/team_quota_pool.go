package server

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// teamQuotaPoolScope labels pool rejections, metrics, and alerts. It follows
// the quota policy scope vocabulary so error details and dashboards stay
// consistent with the existing project/team/user scopes.
const teamQuotaPoolScope = "team"

// teamQuotaPoolBucketKey identifies the shared pool bucket for a team inside
// the QuotaBucket table. It follows the synthetic "user:{userID}" precedent:
// pool counters reuse the same table and window helpers, so no dedicated
// schema is required for enforcement.
func teamQuotaPoolBucketKey(teamID string) string {
	return "team:" + strings.TrimSpace(teamID)
}

// TeamQuotaPool is the shared quota pool configured on a team resource. Every
// API key of a project whose primary team (Project.TeamID) is that team draws
// from the same pool: their combined day/month consumption can never exceed
// the pool no matter how individual keys split it. Minute-level rate limits
// and concurrency stay per key.
type TeamQuotaPool struct {
	TeamID  string
	Limits  QuotaLimits
	Dynamic bool
}

// Enabled reports whether a pool applies. A pool without any positive limit
// never blocks and never writes counters, so configuring only a future
// allocation mode stays harmless.
func (p TeamQuotaPool) Enabled() bool {
	if p.TeamID == "" {
		return false
	}
	return p.Limits.DailyTokens > 0 || p.Limits.MonthlyTokens > 0 || p.Limits.DailyCostUSD > 0 || p.Limits.MonthlyCostUSD > 0
}

// teamQuotaPoolFromFields parses the pool_* fields of a teams AdminResource.
// Requests and concurrency are deliberately not pool dimensions: a shared
// request-rate cap across teams of applications would couple unrelated
// callers, while the pool exists to bound aggregate token and cost spend.
func teamQuotaPoolFromFields(teamID string, fields map[string]any) TeamQuotaPool {
	mode := strings.ToLower(strings.TrimSpace(stringField(fields, "pool_allocation_mode")))
	return TeamQuotaPool{
		TeamID: strings.TrimSpace(teamID),
		Limits: QuotaLimits{
			DailyTokens:    int64Field(fields, "pool_daily_tokens"),
			MonthlyTokens:  int64Field(fields, "pool_monthly_tokens"),
			DailyCostUSD:   float64Field(fields, "pool_daily_cost_usd"),
			MonthlyCostUSD: float64Field(fields, "pool_monthly_cost_usd"),
		},
		Dynamic: mode == "dynamic",
	}
}

// teamQuotaPoolForProject resolves the pool of the project's primary team. A
// project without a team, an inactive or missing team resource, or a pool
// without any positive limit returns a disabled pool and admission proceeds
// unchanged.
func teamQuotaPoolForProject(tx *gorm.DB, project Project) (TeamQuotaPool, error) {
	teamID := strings.TrimSpace(project.TeamID)
	if teamID == "" {
		return TeamQuotaPool{}, nil
	}
	var team AdminResource
	if err := tx.First(&team, "kind = ? AND id = ?", "teams", teamID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TeamQuotaPool{}, nil
		}
		return TeamQuotaPool{}, err
	}
	if team.Status != StatusActive {
		return TeamQuotaPool{}, nil
	}
	return teamQuotaPoolFromFields(team.ID, team.Fields), nil
}

// validateTeamQuotaPoolFields validates the pool_* fields of a teams resource
// mutation. It rejects malformed limits and unknown allocation modes so a
// typo cannot silently disable or corrupt pool enforcement.
func validateTeamQuotaPoolFields(fields map[string]any) error {
	if fields == nil {
		return nil
	}
	for _, key := range []string{"pool_daily_tokens", "pool_monthly_tokens"} {
		value, ok := fields[key]
		if !ok || value == nil {
			continue
		}
		if !validNonNegativeInt64(value) {
			return NewHTTPError(http.StatusBadRequest, "invalid_team_quota_pool_limit", "Team quota pool token limits must be non-negative 64-bit integers")
		}
	}
	for _, key := range []string{"pool_daily_cost_usd", "pool_monthly_cost_usd"} {
		value, ok := fields[key]
		if !ok || value == nil {
			continue
		}
		if !validNonNegativeFloat(value) {
			return NewHTTPError(http.StatusBadRequest, "invalid_team_quota_pool_limit", "Team quota pool cost limits must be non-negative numbers")
		}
	}
	if value, ok := fields["pool_allocation_mode"]; ok && value != nil {
		mode := strings.ToLower(strings.TrimSpace(stringField(fields, "pool_allocation_mode")))
		if mode != "" && mode != "off" && mode != "dynamic" {
			return NewHTTPError(http.StatusBadRequest, "invalid_team_quota_pool_mode", "Team quota pool allocation mode must be off or dynamic")
		}
	}
	return nil
}

func validNonNegativeFloat(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed >= 0
	case int64:
		return typed >= 0
	case float64:
		return !isNaNOrInf(typed) && typed >= 0
	case json.Number:
		parsed, err := strconv.ParseFloat(string(typed), 64)
		return err == nil && parsed >= 0
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return err == nil && parsed >= 0
	default:
		return false
	}
}

func isNaNOrInf(value float64) bool {
	return math.IsNaN(value) || math.IsInf(value, 0)
}

// strictCostLimitChanged mirrors strictLimitChanged for float cost limits.
func strictCostLimitChanged(current float64, candidate float64) bool {
	return candidate > 0 && (current <= 0 || candidate < current)
}

// TeamQuotaShare is one key's share of the team pool for one period, as
// computed by the rebalancer.
type TeamQuotaShare struct {
	Tokens  int64
	CostUSD float64
}

// TeamQuotaAllocation is one rebalancer decision: the share of the team pool
// granted to a key for one quota period. Shares are advisory caps merged into
// admission (strictest of static limits and share); the pool bucket itself
// remains the hard backstop.
type TeamQuotaAllocation struct {
	ID               string    `json:"id" gorm:"primaryKey;size:64"`
	TeamID           string    `json:"team_id" gorm:"index:idx_team_quota_allocation_lookup,unique"`
	APIKeyID         string    `json:"api_key_id" gorm:"index:idx_team_quota_allocation_lookup,unique;size:64"`
	PeriodType       string    `json:"period_type" gorm:"index:idx_team_quota_allocation_lookup,unique;size:16"`
	PeriodRef        string    `json:"period_ref" gorm:"index:idx_team_quota_allocation_lookup,unique;size:32"`
	AllocatedTokens  int64     `json:"allocated_tokens"`
	AllocatedCostUSD float64   `json:"allocated_cost_usd"`
	BasisJSON        string    `json:"basis_json,omitempty" gorm:"type:text"`
	CreatedAt        time.Time `json:"created_at"`
}

// teamQuotaShareLimits converts an allocation row into the QuotaLimits merge
// candidate for admission. Only the period matching the row type is filled so
// day and month shares merge independently.
func (a TeamQuotaAllocation) shareLimits() QuotaLimits {
	limits := QuotaLimits{}
	switch a.PeriodType {
	case "day":
		limits.DailyTokens = a.AllocatedTokens
		limits.DailyCostUSD = a.AllocatedCostUSD
	case "month":
		limits.MonthlyTokens = a.AllocatedTokens
		limits.MonthlyCostUSD = a.AllocatedCostUSD
	}
	return limits
}
