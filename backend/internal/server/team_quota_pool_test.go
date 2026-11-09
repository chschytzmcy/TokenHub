package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTeamPoolGateway(t *testing.T, teamID string, poolFields map[string]any, keyLimits APIKey) (*GormStore, Project, APIKey, string) {
	t.Helper()
	store := NewMemoryStore()
	if teamID != "" {
		store.CreateResource("teams", AdminResource{ID: teamID, Name: "Pool Team", Status: StatusActive, Fields: poolFields})
	}
	project := store.CreateProject(Project{Name: "Pooled Project", Status: StatusActive, TeamID: teamID})
	keyLimits.Name = "pooled"
	keyLimits.Allowed = []string{"gpt-4.1-mini"}
	keyLimits.Status = StatusActive
	_, secret, err := store.CreateAPIKey(project.ID, keyLimits, "thk_team_pool_"+NewID("test"))
	if err != nil {
		t.Fatal(err)
	}
	provider := store.AddProvider(Provider{Name: "Mock", Type: ProviderMock, Status: StatusActive, Healthy: true})
	store.AddModel(Model{Name: "gpt-4.1-mini", Modality: "chat", Status: StatusActive})
	store.AddRoute(ModelRoute{ModelName: "gpt-4.1-mini", ProviderID: provider.ID, ProviderModel: "mock-chat", Status: StatusActive})
	return store, project, store.ListAPIKeys()[0], secret
}

func teamPoolBucket(t *testing.T, store *GormStore, teamID, scope, bucket string) QuotaBucket {
	t.Helper()
	var item QuotaBucket
	err := store.db.First(&item, "key_id = ? AND scope = ? AND bucket = ?", teamQuotaPoolBucketKey(teamID), scope, bucket).Error
	if err != nil {
		t.Fatalf("pool bucket missing: %v", err)
	}
	return item
}

func TestTeamQuotaPoolAdmissionAndSettlement(t *testing.T) {
	store, project, key, _ := newTeamPoolGateway(t, "team_pool", map[string]any{"pool_daily_tokens": 100}, APIKey{})
	periods, err := currentQuotaPeriods(store.db, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	call, err := store.StartCall(context.Background(), project, key, "gpt-4.1-mini", 60)
	if err != nil {
		t.Fatalf("first admission under pool failed: %v", err)
	}
	if call.TeamQuota == nil || !call.TeamQuota.Enabled || call.TeamQuota.PoolID != teamQuotaPoolBucketKey("team_pool") {
		t.Fatalf("pool context missing: %+v", call.TeamQuota)
	}
	store.FinishCall(call, RouteSelection{}, Usage{TotalTokens: 60}, http.StatusOK, "", "127.0.0.1", "pool-test")
	day := teamPoolBucket(t, store, "team_pool", "day", periods.Day)
	if day.TotalTokens != 60 || day.Requests != 1 {
		t.Fatalf("pool settlement wrong: %+v", day.QuotaCounter)
	}

	// Reservation 60 plus settled 60 exceeds the 100 token pool.
	if _, err := store.StartCall(context.Background(), project, key, "gpt-4.1-mini", 60); err == nil {
		t.Fatal("pool must block admission past its daily token limit")
	} else {
		httpErr := AsHTTPError(err)
		details, _ := httpErr.Details.(map[string]string)
		if httpErr.Code != "quota_exceeded" || details["scope"] != "team" {
			t.Fatalf("unexpected pool rejection: %v %+v", err, httpErr.Details)
		}
	}

	// A project without a team keeps admitting: no pool applies.
	freeProject := store.CreateProject(Project{Name: "Free Project", Status: StatusActive})
	freeKey := APIKey{Name: "free", Allowed: []string{"gpt-4.1-mini"}, Status: StatusActive}
	if _, _, err := store.CreateAPIKey(freeProject.ID, freeKey, "thk_free_"+NewID("test")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartCall(context.Background(), freeProject, store.ListAPIKeys()[1], "gpt-4.1-mini", 0); err != nil {
		t.Fatalf("project without team must bypass pool: %v", err)
	}
}

func TestTeamQuotaPoolDisabledWithoutLimits(t *testing.T) {
	// Team resource exists but configures no pool limits: pool stays disabled.
	store, project, key, _ := newTeamPoolGateway(t, "team_empty", map[string]any{"owner": "someone"}, APIKey{})
	if _, err := store.StartCall(context.Background(), project, key, "gpt-4.1-mini", 0); err != nil {
		t.Fatalf("team without pool fields must not block: %v", err)
	}
	var buckets int64
	if err := store.db.Model(&QuotaBucket{}).Where("key_id = ?", teamQuotaPoolBucketKey("team_empty")).Count(&buckets).Error; err != nil {
		t.Fatal(err)
	}
	if buckets != 0 {
		t.Fatalf("disabled pool must not write buckets, got %d", buckets)
	}
}

func TestTeamQuotaPoolShareCapsKeyBeforeStaticLimit(t *testing.T) {
	store, project, key, _ := newTeamPoolGateway(t, "team_share", map[string]any{
		"pool_daily_tokens": 100, "pool_allocation_mode": "dynamic",
	}, APIKey{})
	periods, err := currentQuotaPeriods(store.db, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&TeamQuotaAllocation{
		ID: NewID("tqa"), TeamID: "team_share", APIKeyID: key.ID,
		PeriodType: "day", PeriodRef: periods.Day, AllocatedTokens: 40, CreatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}

	// The dynamic share (40) is stricter than the absent static limit.
	if _, err := store.StartCall(context.Background(), project, key, "gpt-4.1-mini", 50); err == nil {
		t.Fatal("dynamic share must cap admission below the pool total")
	}
	// Within the share, admission succeeds.
	if _, err := store.StartCall(context.Background(), project, key, "gpt-4.1-mini", 30); err != nil {
		t.Fatalf("admission within the dynamic share failed: %v", err)
	}
}

func TestTeamQuotaPoolShareScopeAttributesToTeam(t *testing.T) {
	// The key has a generous static limit, but the dynamic share is stricter.
	// The rejection must carry the team scope, because the share is the
	// binding limit, not the key's own configuration.
	store, project, key, _ := newTeamPoolGateway(t, "team_scope", map[string]any{
		"pool_daily_tokens": 1000, "pool_allocation_mode": "dynamic",
	}, APIKey{Limits: QuotaLimits{DailyTokens: 900}})
	periods, err := currentQuotaPeriods(store.db, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&TeamQuotaAllocation{
		ID: NewID("tqa"), TeamID: "team_scope", APIKeyID: key.ID,
		PeriodType: "day", PeriodRef: periods.Day, AllocatedTokens: 30, CreatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = store.StartCall(context.Background(), project, key, "gpt-4.1-mini", 50)
	if err == nil {
		t.Fatal("the stricter dynamic share must reject the call")
	}
	httpErr := AsHTTPError(err)
	details, _ := httpErr.Details.(map[string]string)
	if httpErr.Code != "quota_exceeded" || details["scope"] != "team" {
		t.Fatalf("share rejection must carry the team scope, got %v %+v", err, httpErr.Details)
	}
}

func TestTeamQuotaPoolFieldValidation(t *testing.T) {
	store, _, _, _ := newTeamPoolGateway(t, "team_validate", nil, APIKey{})
	app := New(store).Handler()
	for _, test := range []struct {
		name    string
		payload map[string]any
		code    string
	}{
		{
			name:    "negative token pool",
			payload: map[string]any{"fields": map[string]any{"pool_daily_tokens": -5}},
			code:    "invalid_team_quota_pool_limit",
		},
		{
			name:    "negative cost pool",
			payload: map[string]any{"fields": map[string]any{"pool_daily_cost_usd": -1.5}},
			code:    "invalid_team_quota_pool_limit",
		},
		{
			name:    "unknown allocation mode",
			payload: map[string]any{"fields": map[string]any{"pool_allocation_mode": "auto"}},
			code:    "invalid_team_quota_pool_mode",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := doJSON(t, app, http.MethodPatch, "/api/admin/resources/teams/team_validate", test.payload, "")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body, test.code) {
				t.Fatalf("invalid pool field returned %d: %s", response.Code, response.Body)
			}
		})
	}
	valid := doJSON(t, app, http.MethodPatch, "/api/admin/resources/teams/team_validate", map[string]any{
		"fields": map[string]any{"pool_daily_tokens": 1000, "pool_allocation_mode": "dynamic"},
	}, "")
	if valid.Code != http.StatusOK {
		t.Fatalf("valid pool config rejected: %d %s", valid.Code, valid.Body)
	}
	teams := doJSON(t, app, http.MethodGet, "/api/admin/resources/teams", nil, "")
	if teams.Code != http.StatusOK || !strings.Contains(teams.Body, "current_usage") {
		t.Fatalf("team list must attach pool usage: %d %s", teams.Code, teams.Body)
	}
}

func TestTeamQuotaPoolAllocationsEndpoint(t *testing.T) {
	store, _, key, _ := newTeamPoolGateway(t, "team_alloc", map[string]any{"pool_daily_tokens": 100}, APIKey{})
	periods, err := currentQuotaPeriods(store.db, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&TeamQuotaAllocation{
		ID: NewID("tqa"), TeamID: "team_alloc", APIKeyID: key.ID,
		PeriodType: "day", PeriodRef: periods.Day, AllocatedTokens: 70, CreatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	app := New(store).Handler()
	response := doJSON(t, app, http.MethodGet, "/api/admin/teams/team_alloc/quota-pool/allocations", nil, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body, "allocated_tokens") {
		t.Fatalf("allocations endpoint failed: %d %s", response.Code, response.Body)
	}
	filtered := doJSON(t, app, http.MethodGet, "/api/admin/teams/team_alloc/quota-pool/allocations?period_type=month", nil, "")
	if filtered.Code != http.StatusOK || strings.Contains(filtered.Body, "allocated_tokens\":70") {
		t.Fatalf("period filter leaked day rows: %d %s", filtered.Code, filtered.Body)
	}
}

func TestComputeTeamQuotaShareFloats(t *testing.T) {
	keys := []string{"k1", "k2", "k3"}
	// A fully cold team splits the pool evenly.
	fair := computeTeamQuotaShareFloats(90, map[string]float64{}, keys)
	for _, keyID := range keys {
		if fair[keyID] != 30 {
			t.Fatalf("cold team must split evenly: %+v", fair)
		}
	}
	weighted := computeTeamQuotaShareFloats(100, map[string]float64{"k1": 90, "k2": 10, "k3": 0}, keys)
	// k3 gets the fair floor (33.3), k1 and k2 split the remaining 66.6 by 9:1.
	if weighted["k1"] <= weighted["k2"] {
		t.Fatalf("heavier demand must receive the larger share: %+v", weighted)
	}
	if math.Abs(weighted["k1"]+weighted["k2"]+weighted["k3"]-100) > 1e-6 {
		t.Fatalf("shares must sum to the pool: %+v", weighted)
	}
	if shares := computeTeamQuotaShareFloats(0, map[string]float64{"k1": 90}, keys); len(shares) != 0 {
		t.Fatalf("a pool without limits must yield no shares: %+v", shares)
	}
}

func TestTeamQuotaRebalancerProducesSharesFromUsage(t *testing.T) {
	store, _, _, _ := newTeamPoolGateway(t, "team_rebal", map[string]any{
		"pool_daily_tokens": 200, "pool_allocation_mode": "dynamic",
	}, APIKey{})
	project := store.ListProjects()[0]
	second := APIKey{Name: "second", Allowed: []string{"gpt-4.1-mini"}, Status: StatusActive}
	if _, _, err := store.CreateAPIKey(project.ID, second, "thk_second_"+NewID("test")); err != nil {
		t.Fatal(err)
	}
	keys := store.ListAPIKeys()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := store.db.Create(&UsageRecord{
			ID: NewID("use"), RequestID: NewID("req"), ProjectID: project.ID, APIKeyID: keys[0].ID,
			ModelName: "gpt-4.1-mini", TotalTokens: 300, CostUSD: 3, CreatedAt: now.Add(-time.Duration(i+1) * 24 * time.Hour),
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.db.Create(&UsageRecord{
			ID: NewID("use"), RequestID: NewID("req"), ProjectID: project.ID, APIKeyID: keys[1].ID,
			ModelName: "gpt-4.1-mini", TotalTokens: 100, CostUSD: 1, CreatedAt: now.Add(-time.Duration(i+1) * 24 * time.Hour),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	rebalancer := NewTeamQuotaRebalancer(store, teamQuotaRebalanceInterval, 7)
	if err := rebalancer.rebalanceOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.listTeamQuotaAllocations("team_rebal", "day")
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected day allocations for both keys, got %d rows err=%v", len(rows), err)
	}
	byKey := map[string]TeamQuotaAllocation{}
	for _, row := range rows {
		byKey[row.APIKeyID] = row
	}
	if byKey[keys[0].ID].AllocatedTokens <= byKey[keys[1].ID].AllocatedTokens {
		t.Fatalf("heavier key must receive the larger share: %+v", byKey)
	}
	if byKey[keys[0].ID].AllocatedTokens+byKey[keys[1].ID].AllocatedTokens != 200 {
		t.Fatalf("shares must sum to the pool: %+v", byKey)
	}

	// A second run without new usage keeps the same plan and raises no alert.
	var alertsBefore int64
	if err := store.db.Model(&AlertEvent{}).Where("code = ?", "team_quota_rebalanced").Count(&alertsBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := rebalancer.rebalanceOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var alertsAfter int64
	if err := store.db.Model(&AlertEvent{}).Where("code = ?", "team_quota_rebalanced").Count(&alertsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if alertsBefore != alertsAfter {
		t.Fatalf("stable plan must not re-raise the rebalance alert: %d -> %d", alertsBefore, alertsAfter)
	}
}

func TestTeamQuotaPoolJSONRoundTrip(t *testing.T) {
	row := TeamQuotaAllocation{PeriodType: "month", AllocatedTokens: 5, AllocatedCostUSD: 0.5}
	encoded, err := json.Marshal(row.shareLimits())
	if err != nil {
		t.Fatal(err)
	}
	var limits QuotaLimits
	if err := json.Unmarshal(encoded, &limits); err != nil {
		t.Fatal(err)
	}
	if limits.MonthlyTokens != 5 || limits.MonthlyCostUSD != 0.5 || limits.DailyTokens != 0 {
		t.Fatalf("share limits scoped to the wrong period: %+v", limits)
	}
}

func TestTeamQuotaPoolRejectionSurfacesInGateway(t *testing.T) {
	store, _, _, secret := newTeamPoolGateway(t, "team_http", map[string]any{"pool_daily_tokens": 100000}, APIKey{})
	app := New(store).Handler()
	payload := map[string]any{
		"model":    "gpt-4.1-mini",
		"messages": []map[string]any{{"role": "user", "content": "pool"}},
	}
	first := doJSON(t, app, http.MethodPost, "/v1/chat/completions", payload, secret)
	if first.Code != http.StatusOK {
		t.Fatalf("first request expected 200, got %d: %s", first.Code, first.Body)
	}
	// Push the shared pool bucket to its limit to simulate exhaustion.
	periods, err := currentQuotaPeriods(store.db, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	day := teamPoolBucket(t, store, "team_http", "day", periods.Day)
	day.TotalTokens = 100000
	if err := store.db.Save(&day).Error; err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"pool"}]}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+secret)
	resp := httptest.NewRecorder()
	app.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("pool exhaustion must reject with 429, got %d: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "quota_exceeded" || body.Error.Details["scope"] != "team" {
		t.Fatalf("pool rejection must carry team scope: %s", resp.Body.String())
	}
}
