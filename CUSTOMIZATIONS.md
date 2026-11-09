# Fork Customization Ledger

This checkout is a downstream fork of TokenHub carrying business-specific
customizations. This ledger tracks every deviation from upstream so upstream
syncs (`merge`, never `rebase`) can resolve conflicts quickly and renumber
local migration versions. Keep one row per logical change and update it in the
same commit as the change itself.

## Sync procedure

1. Sync `main` from upstream (release tags preferred), then `git merge main`
   into the customization branch.
2. Renumber local `dbschema` migrations that collide with upstream versions:
   take the next free version and update `SchemaMigrationRegistry()` plus the
   regenerated `internal/dbschema/migrations_manifest.json`.
3. Re-run the repo gates and CI-equivalent checks before shipping the merge.
4. Prefer upstreaming generic capabilities (see scenario 1 below); only
   business-specific behavior stays local.

## Local changes

| Area | Files | Upstreamable? | Notes |
| --- | --- | --- | --- |
| Self-hosted SystemOne evaluator (`TOKENHUB_TYPESAFE_BASE_URL`) | `backend/internal/server/config.go`, `semantic_routing_client.go`, `semantic_routing.go`, `semantic_routing_client_test.go`, `semantic_routing_policy_test.go`, `backend/.env.example`, `deploy/.env.example`, `deploy/docker-compose.yml`, `docs/deployment.md` (+zh-CN/ja) | Yes — intended for an upstream PR | Default behavior unchanged when the variable is empty |
| Team quota pools (shared team bucket, admission + settlement) | `backend/internal/server/team_quota_pool.go`, `store_team_pool.go`, `admin_team_pool_http.go`, wiring in `store_call_admission.go`, `store_routing_calls.go`, `admin_resources_routing.go`, `routes.go`, `types.go` | No — business-specific | Pool bucket reuses the `QuotaBucket` synthetic-key precedent (`team:{teamID}`) |
| Team pool rebalancer (dynamic shares) | `backend/internal/server/team_quota_rebalancer.go`, `TeamQuotaAllocation` migration (Version 11 in `SchemaMigrationRegistry`), `config.go`, `billing_lifecycle.go`, `image_generation.go` (shutdown), env contract files | No — business-specific | **Migration renumber on sync:** `add-team-quota-allocations` currently occupies Version 11; retake the next free version after an upstream sync |
| Team pool admin UI (config fields, pool usage column) | `frontend/features/admin/resources/settings-config.tsx`, `frontend/features/admin/i18n/team-quota-pool.tsx` (+ registration in `translations.tsx`) | No — business-specific | i18n strings live in a dedicated feature catalog file to respect the line ratchet on `en.tsx`/`ja.tsx` |
| Upstream sync automation | `.github/workflows/sync-upstream.yml`, `.github/tools/create-sync-pr.sh` | No — fork infrastructure | Daily ff-only sync of `main` + sync PR against `custom/main`; detects schema migration version collisions. Requires: fork default branch set to `custom/main` (scheduled workflows only run from the default branch), repository variable `UPSTREAM_REPOSITORY`, and optionally secret `SYNC_TOKEN` (PAT) so sync PRs trigger CI |
