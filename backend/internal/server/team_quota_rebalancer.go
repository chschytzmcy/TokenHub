package server

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	teamQuotaRebalanceTaskName   = "team-quota-rebalance"
	teamQuotaRebalanceInterval   = 15 * time.Minute
	teamQuotaRebalanceWindowDays = 7
	// teamQuotaRebalanceDriftLimit keeps the share plan stable: a share is only
	// rewritten when the new value drifts more than this fraction from the
	// current one, so noisy traffic cannot flap every key's cap each cycle.
	teamQuotaRebalanceDriftLimit = 0.10
)

// teamQuotaShareDraft is one recomputed share before it is persisted.
type teamQuotaShareDraft struct {
	tokens float64
	cost   float64
}

// TeamQuotaRebalancer periodically redistributes each dynamic team pool across
// the keys that draw from it, proportional to recent demand. It only writes
// TeamQuotaAllocation rows; enforcement stays in admission (share caps merged
// strictest-wins) with the pool bucket as the hard backstop.
type TeamQuotaRebalancer struct {
	store      *GormStore
	interval   time.Duration
	windowDays int

	schedulerMu   sync.Mutex
	schedulerStop context.CancelFunc
	schedulerDone chan struct{}
}

func NewTeamQuotaRebalancer(store *GormStore, interval time.Duration, windowDays int) *TeamQuotaRebalancer {
	if interval <= 0 {
		interval = teamQuotaRebalanceInterval
	}
	if windowDays <= 0 {
		windowDays = teamQuotaRebalanceWindowDays
	}
	return &TeamQuotaRebalancer{store: store, interval: interval, windowDays: windowDays}
}

func (r *TeamQuotaRebalancer) RunDue(ctx context.Context, now time.Time) error {
	if r == nil || r.store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	revision := now.UTC().Truncate(r.interval).Unix()
	return r.store.RunClusterTask(ctx, teamQuotaRebalanceTaskName, revision, r.rebalanceOnce)
}

func (r *TeamQuotaRebalancer) StartScheduler(interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = r.interval
	}
	r.schedulerMu.Lock()
	defer r.schedulerMu.Unlock()
	if r.schedulerStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.schedulerStop = cancel
	r.schedulerDone = make(chan struct{})
	go func() {
		defer close(r.schedulerDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if err := r.RunDue(ctx, now); err != nil && ctx.Err() == nil {
					log.Printf("[tokenhub] team quota rebalance failed: %v", err)
				}
			}
		}
	}()
}

func (r *TeamQuotaRebalancer) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.schedulerMu.Lock()
	stop := r.schedulerStop
	done := r.schedulerDone
	r.schedulerMu.Unlock()
	if stop == nil {
		return nil
	}
	stop()
	select {
	case <-done:
		r.schedulerMu.Lock()
		if r.schedulerDone == done {
			r.schedulerStop = nil
			r.schedulerDone = nil
		}
		r.schedulerMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rebalanceOnce walks every active team with a dynamic pool. A failing team is
// logged and skipped so one broken configuration cannot stall the others.
func (r *TeamQuotaRebalancer) rebalanceOnce(ctx context.Context) error {
	teams := r.store.ListResources("teams")
	for _, team := range teams {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if team.Status != StatusActive {
			continue
		}
		pool := teamQuotaPoolFromFields(team.ID, team.Fields)
		if !pool.Enabled() || !pool.Dynamic {
			continue
		}
		if err := r.rebalanceTeam(team.ID, pool); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[tokenhub] team quota rebalance skipped for team %s: %v", team.ID, err)
		}
	}
	return nil
}

func (r *TeamQuotaRebalancer) rebalanceTeam(teamID string, pool TeamQuotaPool) error {
	return r.store.db.Transaction(func(tx *gorm.DB) error {
		keyIDs, err := r.store.teamQuotaRebalanceKeys(tx, teamID)
		if err != nil {
			return err
		}
		now, err := r.store.databaseNow(tx)
		if err != nil {
			return err
		}
		periods, err := currentQuotaPeriods(tx, now)
		if err != nil {
			return err
		}
		demand, err := r.store.teamQuotaDailyDemand(tx, keyIDs, now.Add(-time.Duration(r.windowDays)*24*time.Hour))
		if err != nil {
			return err
		}
		tokenDemand := teamQuotaDemandSeries(demand, keyIDs, r.windowDays, func(day teamQuotaDayUsage) float64 { return day.Tokens })
		costDemand := teamQuotaDemandSeries(demand, keyIDs, r.windowDays, func(day teamQuotaDayUsage) float64 { return day.CostUSD })
		changed := false
		for _, period := range []struct {
			periodType string
			periodRef  string
			tokens     int64
			cost       float64
		}{
			{periodType: "day", periodRef: periods.Day, tokens: pool.Limits.DailyTokens, cost: pool.Limits.DailyCostUSD},
			{periodType: "month", periodRef: periods.Month, tokens: pool.Limits.MonthlyTokens, cost: pool.Limits.MonthlyCostUSD},
		} {
			if period.tokens <= 0 && period.cost <= 0 {
				continue
			}
			existing, err := r.store.teamQuotaPeriodAllocations(tx, teamID, period.periodType, period.periodRef)
			if err != nil {
				return err
			}
			drafts := r.applyDriftHysteresis(existing,
				computeTeamQuotaShareFloats(float64(period.tokens), tokenDemand, keyIDs),
				computeTeamQuotaShareFloats(period.cost, costDemand, keyIDs))
			basis, err := json.Marshal(map[string]any{
				"window_days":  r.windowDays,
				"computed_at":  now.UTC().Format(time.RFC3339),
				"active_keys":  len(keyIDs),
				"demand_ratio": teamQuotaDemandRatio(tokenDemand),
			})
			if err != nil {
				return err
			}
			rows := make(map[string]TeamQuotaShare, len(drafts))
			for keyID, draft := range drafts {
				rows[keyID] = TeamQuotaShare{Tokens: int64(math.Round(draft.tokens)), CostUSD: draft.cost}
			}
			if teamQuotaSharesDiffer(existing, rows) {
				changed = true
			}
			if err := r.store.replaceTeamQuotaAllocations(tx, teamID, period.periodType, period.periodRef, rows, string(basis)); err != nil {
				return err
			}
		}
		if !changed {
			return nil
		}
		return tx.Create(&AlertEvent{
			ID:         NewID("alt"),
			ScopeType:  teamQuotaPoolScope,
			ScopeID:    teamID,
			Severity:   "info",
			Code:       "team_quota_rebalanced",
			Message:    "Team quota pool shares were rebalanced from recent usage",
			ResourceID: teamID,
			CreatedAt:  time.Now().UTC(),
		}).Error
	})
}

// applyDriftHysteresis keeps the previous allocation whenever the recomputed
// share stays inside the drift band, for tokens and cost independently.
func (r *TeamQuotaRebalancer) applyDriftHysteresis(existing map[string]TeamQuotaAllocation, tokens map[string]float64, cost map[string]float64) map[string]teamQuotaShareDraft {
	shares := make(map[string]teamQuotaShareDraft, len(tokens))
	for keyID, value := range tokens {
		shares[keyID] = teamQuotaShareDraft{tokens: value, cost: cost[keyID]}
	}
	for keyID, costValue := range cost {
		if _, ok := shares[keyID]; !ok {
			shares[keyID] = teamQuotaShareDraft{cost: costValue}
		}
	}
	for keyID := range shares {
		previous, ok := existing[keyID]
		if !ok {
			continue
		}
		if previous.AllocatedTokens > 0 && shares[keyID].tokens > 0 &&
			math.Abs(shares[keyID].tokens-float64(previous.AllocatedTokens))/float64(previous.AllocatedTokens) < teamQuotaRebalanceDriftLimit {
			share := shares[keyID]
			share.tokens = float64(previous.AllocatedTokens)
			shares[keyID] = share
		}
		if previous.AllocatedCostUSD > 0 && shares[keyID].cost > 0 &&
			math.Abs(shares[keyID].cost-previous.AllocatedCostUSD)/previous.AllocatedCostUSD < teamQuotaRebalanceDriftLimit {
			share := shares[keyID]
			share.cost = previous.AllocatedCostUSD
			shares[keyID] = share
		}
	}
	return shares
}

// teamQuotaDemandSeries projects the daily usage rows into one demand value per
// key: the larger of the window's daily average and the p90 of active days, so
// bursty keys keep headroom while idle windows do not inflate the share.
func teamQuotaDemandSeries(demand map[string][]teamQuotaDayUsage, keyIDs []string, windowDays int, project func(teamQuotaDayUsage) float64) map[string]float64 {
	if windowDays <= 0 {
		windowDays = teamQuotaRebalanceWindowDays
	}
	series := make(map[string]float64, len(keyIDs))
	for _, keyID := range keyIDs {
		days := demand[keyID]
		if len(days) == 0 {
			series[keyID] = 0
			continue
		}
		values := make([]float64, 0, len(days))
		total := 0.0
		for _, day := range days {
			value := project(day)
			if value > 0 {
				values = append(values, value)
			}
			total += value
		}
		average := total / float64(windowDays)
		p90 := 0.0
		if len(values) > 0 {
			sort.Float64s(values)
			p90 = values[int(float64(len(values)-1)*0.9+0.5)]
		}
		series[keyID] = math.Max(average, p90)
	}
	return series
}

// computeTeamQuotaShareFloats splits one pool dimension across keys: keys with
// no history receive an equal fair-share floor and the remainder is split
// proportionally to demand.
func computeTeamQuotaShareFloats(pool float64, demands map[string]float64, keyIDs []string) map[string]float64 {
	shares := make(map[string]float64, len(keyIDs))
	if pool <= 0 || len(keyIDs) == 0 {
		return shares
	}
	total := 0.0
	for _, keyID := range keyIDs {
		if demands[keyID] > 0 {
			total += demands[keyID]
		}
	}
	if total <= 0 {
		fair := pool / float64(len(keyIDs))
		for _, keyID := range keyIDs {
			shares[keyID] = fair
		}
		return shares
	}
	fair := pool / float64(len(keyIDs))
	remainder := pool
	weighted := make([]string, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		if demands[keyID] > 0 {
			weighted = append(weighted, keyID)
		} else {
			shares[keyID] = fair
			remainder -= fair
		}
	}
	if remainder <= 0 || len(weighted) == 0 {
		return shares
	}
	weightedTotal := 0.0
	for _, keyID := range weighted {
		weightedTotal += demands[keyID]
	}
	for _, keyID := range weighted {
		shares[keyID] = remainder * demands[keyID] / weightedTotal
	}
	return shares
}

func teamQuotaSharesDiffer(existing map[string]TeamQuotaAllocation, rows map[string]TeamQuotaShare) bool {
	if len(existing) != len(rows) {
		return true
	}
	for keyID, row := range rows {
		previous, ok := existing[keyID]
		if !ok || previous.AllocatedTokens != row.Tokens || math.Abs(previous.AllocatedCostUSD-row.CostUSD) > 1e-9 {
			return true
		}
	}
	return false
}

func teamQuotaDemandRatio(demand map[string]float64) map[string]float64 {
	total := 0.0
	for _, value := range demand {
		total += value
	}
	if total <= 0 {
		return map[string]float64{}
	}
	ratio := make(map[string]float64, len(demand))
	for keyID, value := range demand {
		if value > 0 {
			ratio[keyID] = math.Round(value/total*10000) / 10000
		}
	}
	return ratio
}
