package server

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"tokenhub/backend/internal/dbschema"
)

// teamQuotaAllocationMigration creates the rebalancer decision table. It stays
// out of schemaModels: the frozen baseline must not grow, so a versioned
// expansion owns the table for both dialects (the column types are valid on
// SQLite and PostgreSQL alike, matching the metering migration precedent).
func teamQuotaAllocationMigration() dbschema.Migration {
	return dbschema.Migration{Version: 11, Name: "add-team-quota-allocations", Statements: []string{
		`CREATE TABLE IF NOT EXISTS team_quota_allocations (id text PRIMARY KEY, team_id text NOT NULL, api_key_id text NOT NULL, period_type text NOT NULL, period_ref text NOT NULL, allocated_tokens bigint NOT NULL DEFAULT 0, allocated_cost_usd double precision NOT NULL DEFAULT 0, basis_json text NOT NULL DEFAULT '', created_at timestamp NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_team_quota_allocation_lookup ON team_quota_allocations (team_id, api_key_id, period_type, period_ref)`,
		`CREATE INDEX IF NOT EXISTS idx_team_quota_allocations_team_period ON team_quota_allocations (team_id, period_type, period_ref)`,
	}, StatementBudget: 10}
}

// teamQuotaDayUsage is one key's settled usage on one calendar day, as
// aggregated from usage_records for the rebalancer demand model.
type teamQuotaDayUsage struct {
	Tokens  float64
	CostUSD float64
}

// TeamQuotaPoolUsage reports the shared pool consumption for the current day
// and month windows. It mirrors GetQuotaPolicyUsage for the team scope so the
// admin resource attachment stays in this file.
func (s *GormStore) TeamQuotaPoolUsage(teamID string) (QuotaPolicyUsage, error) {
	usage := QuotaPolicyUsage{}
	now, err := s.databaseNow(s.db)
	if err != nil {
		return QuotaPolicyUsage{}, err
	}
	periods, err := currentQuotaPeriods(s.db, now)
	if err != nil {
		return QuotaPolicyUsage{}, err
	}
	bucketID := teamQuotaPoolBucketKey(teamID)
	for _, period := range []struct {
		scope   string
		bucket  string
		counter *QuotaCounter
	}{
		{scope: "day", bucket: periods.Day, counter: &usage.Daily},
		{scope: "month", bucket: periods.Month, counter: &usage.Monthly},
	} {
		var item QuotaBucket
		err := s.db.Where("key_id = ? AND scope = ? AND bucket = ? AND attributed_user_id = ?", bucketID, period.scope, period.bucket, unattributedQuotaUserID).First(&item).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return QuotaPolicyUsage{}, err
		}
		if err == nil {
			*period.counter = item.QuotaCounter
		}
	}
	return usage, nil
}

// teamQuotaRebalanceKeys lists the active keys that draw from a team pool:
// keys of active projects whose primary team is the team.
func (s *GormStore) teamQuotaRebalanceKeys(tx *gorm.DB, teamID string) ([]string, error) {
	keyIDs := make([]string, 0)
	err := tx.Model(&APIKey{}).Where("status = ? AND project_id IN (?)", StatusActive,
		tx.Model(&Project{}).Select("id").Where("team_id = ? AND status = ?", teamID, StatusActive)).
		Order("id asc").Pluck("id", &keyIDs).Error
	return keyIDs, err
}

// teamQuotaDailyDemand aggregates per-key per-day usage over the demand
// window. The day expression is dialect-specific so both SQLite and Postgres
// return the same YYYY-MM-DD text.
func (s *GormStore) teamQuotaDailyDemand(tx *gorm.DB, keyIDs []string, since time.Time) (map[string][]teamQuotaDayUsage, error) {
	demand := make(map[string][]teamQuotaDayUsage, len(keyIDs))
	if len(keyIDs) == 0 {
		return demand, nil
	}
	dayExpr := "strftime('%Y-%m-%d', created_at)"
	if s.dbDriver == "postgres" {
		dayExpr = "to_char(created_at, 'YYYY-MM-DD')"
	}
	var rows []struct {
		APIKeyID string
		Day      string
		Tokens   float64
		CostUSD  float64
	}
	err := tx.Model(&UsageRecord{}).
		Select("api_key_id, "+dayExpr+" as day, SUM(total_tokens) as tokens, SUM(cost_usd) as cost_usd").
		Where("api_key_id IN ? AND created_at >= ?", keyIDs, since).
		Group("api_key_id, day").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		demand[row.APIKeyID] = append(demand[row.APIKeyID], teamQuotaDayUsage{Tokens: row.Tokens, CostUSD: row.CostUSD})
	}
	return demand, nil
}

// teamQuotaPeriodAllocations loads the current allocation rows of a team and
// period type, keyed by API key, for the rebalancer's drift comparison.
func (s *GormStore) teamQuotaPeriodAllocations(tx *gorm.DB, teamID, periodType, periodRef string) (map[string]TeamQuotaAllocation, error) {
	var rows []TeamQuotaAllocation
	err := tx.Where("team_id = ? AND period_type = ? AND period_ref = ?", teamID, periodType, periodRef).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]TeamQuotaAllocation, len(rows))
	for _, row := range rows {
		byKey[row.APIKeyID] = row
	}
	return byKey, nil
}

// teamQuotaAllocationLimits loads the current-period dynamic share rows for a
// key and merges them into one QuotaLimits candidate. Missing rows for a
// period leave that dimension at zero so admission falls back to static
// limits until the rebalancer has produced a share.
func (s *GormStore) teamQuotaAllocationLimits(tx *gorm.DB, teamID, keyID string, periods quotaPeriods) (QuotaLimits, error) {
	var limits QuotaLimits
	for _, period := range []struct {
		periodType string
		periodRef  string
	}{
		{periodType: "day", periodRef: periods.Day},
		{periodType: "month", periodRef: periods.Month},
	} {
		var row TeamQuotaAllocation
		err := tx.First(&row, "team_id = ? AND api_key_id = ? AND period_type = ? AND period_ref = ?", teamID, keyID, period.periodType, period.periodRef).Error
		if err != nil {
			if err == gorm.ErrRecordNotFound {
				continue
			}
			return QuotaLimits{}, err
		}
		limits = mergeQuotaLimits(limits, row.shareLimits())
	}
	return limits, nil
}

// replaceTeamQuotaAllocations upserts the rebalancer decision for one team and
// period type, replacing the previous share of every key. Rows for keys that
// dropped out of the team are deleted so stale shares can never cap a key
// after it moved away.
func (s *GormStore) replaceTeamQuotaAllocations(tx *gorm.DB, teamID string, periodType string, periodRef string, shares map[string]TeamQuotaShare, basisJSON string) error {
	var existing []TeamQuotaAllocation
	if err := tx.Where("team_id = ? AND period_type = ? AND period_ref = ?", teamID, periodType, periodRef).Find(&existing).Error; err != nil {
		return err
	}
	existingByKey := make(map[string]TeamQuotaAllocation, len(existing))
	for _, row := range existing {
		existingByKey[row.APIKeyID] = row
	}
	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(shares))
	for keyID, share := range shares {
		seen[keyID] = struct{}{}
		row, ok := existingByKey[keyID]
		if !ok {
			row = TeamQuotaAllocation{
				ID:         NewID("tqa"),
				TeamID:     teamID,
				APIKeyID:   keyID,
				PeriodType: periodType,
				PeriodRef:  periodRef,
			}
		}
		row.AllocatedTokens = share.Tokens
		row.AllocatedCostUSD = share.CostUSD
		row.BasisJSON = basisJSON
		row.CreatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
	}
	for keyID := range existingByKey {
		if _, ok := seen[keyID]; !ok {
			if err := tx.Delete(&TeamQuotaAllocation{}, "team_id = ? AND api_key_id = ? AND period_type = ? AND period_ref = ?", teamID, keyID, periodType, periodRef).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// listTeamQuotaAllocations returns the allocation rows of a team, optionally
// narrowed to one period type. Rows sort by period then key so the admin view
// reads deterministically.
func (s *GormStore) listTeamQuotaAllocations(teamID string, periodType string) ([]TeamQuotaAllocation, error) {
	query := s.db.Where("team_id = ?", teamID)
	if periodType != "" {
		query = query.Where("period_type = ?", periodType)
	}
	rows := make([]TeamQuotaAllocation, 0)
	if err := query.Order("period_ref desc, api_key_id asc").Limit(500).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// raiseTeamQuotaAlerts mirrors raiseUserQuotaAlerts for the shared pool: one
// warning per dimension when the pool reaches its configured limit.
func raiseTeamQuotaAlerts(tx *gorm.DB, teamID string, dayCounter, monthCounter *QuotaCounter, limits QuotaLimits) error {
	checks := []struct {
		limit   float64
		current float64
		code    string
		message string
	}{
		{float64(limits.DailyTokens), float64(dayCounter.TotalTokens), "daily_tokens_near_limit", "Daily team quota pool is near or above limit"},
		{float64(limits.MonthlyTokens), float64(monthCounter.TotalTokens), "monthly_tokens_near_limit", "Monthly team quota pool is near or above limit"},
		{limits.DailyCostUSD, dayCounter.CostUSD, "daily_cost_near_limit", "Daily team quota pool cost is near or above limit"},
		{limits.MonthlyCostUSD, monthCounter.CostUSD, "monthly_cost_near_limit", "Monthly team quota pool cost is near or above limit"},
	}
	for _, check := range checks {
		if check.limit <= 0 || check.current < check.limit {
			continue
		}
		if err := tx.Create(&AlertEvent{
			ID:         NewID("alt"),
			ScopeType:  teamQuotaPoolScope,
			ScopeID:    teamID,
			Severity:   "warning",
			Code:       check.code,
			Message:    check.message,
			ResourceID: teamID,
			CreatedAt:  time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}
