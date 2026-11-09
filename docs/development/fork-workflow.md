# Fork Development Workflow

This repository is a downstream fork of [astaxie/TokenHub](https://github.com/astaxie/TokenHub) with business-specific customizations. This file defines how work is developed, validated, and synchronized with upstream. It complements `AGENTS.md` (repository conventions) and `CUSTOMIZATIONS.md` (the ledger of local deviations).

Read this file before editing. Load `docs/development/workflows/fast-dev.md` or `docs/development/workflows/feature-dev.md` only when the user explicitly selects that workflow.

## Branch topology

| Branch | Role | Who writes to it |
| --- | --- | --- |
| `main` | Pure upstream mirror. Fast-forward only, never holds fork commits. | The sync workflow only. |
| `custom/main` | The fork's default branch. Holds all downstream customization. | You, through pull requests. |
| `chore/sync-upstream-<date>-<sha>` | One short-lived branch per upstream sync. | The sync workflow. |

Rules that follow from this topology:

- Never develop on `main`. It is disposable: the next sync can move it forward at any time.
- Every change to `custom/main` arrives through a pull request, even when a single person authors it.
- `origin` is the fork. `upstream` is the read-only upstream; its push URL is disabled on purpose.
- The fork's default branch is `custom/main`. Scheduled workflows only run from the default branch, so this must stay true.

## Developing a feature

1. Branch from the latest `custom/main`:

   ```bash
   git switch custom/main
   git pull --ff-only
   git switch -c feat/<short-topic>
   ```

2. Implement the change. Keep new behavior in new files where possible, and keep edits to upstream files minimal. Broad edits to shared files make the next upstream merge expensive.

### Keeping customization code isolated

New logic belongs in **new source files**. Existing upstream files — request admission, settlement, routing, and other main flows — receive only a thin wiring call into the new file, never inline logic.

Concretely:

- Put the implementation in a dedicated file named after the feature, for example `store_team_pool.go` or `team_quota_pool.go`. A customization that needs a store, a policy, and an HTTP surface can spread across several such files.
- The call site in the upstream file should read as: resolve the customization state, make one call, merge the result back. Aim for a handful of lines, not a branch with logic in it.
- Mark every wiring point with a `FORK:` comment block that states what the feature does, where its implementation lives, what the call returns, and what to check when an upstream merge touches that area. These blocks are the merge instructions: whoever resolves a conflict sees them at the conflict site.
- Search for them before resolving a merge: `grep -rn "FORK:" backend/internal/server frontend/features`.

Keep the upstream-side footprint small enough to measure. When adding a feature, report the split between "new files, zero merge conflict" and "wiring lines inside upstream files", because the second number is the recurring cost of every future sync.

3. Validate locally before pushing. The narrowest checks first, then the full set:

   ```bash
   # Backend
   cd backend
   gofmt -w <changed-go-files>
   go test ./internal/server/ -run 'TestYourFeature'   # narrow first
   go test ./...                                       # full
   go vet ./...

   # Frontend
   cd ../frontend
   npm run lint && npm run typecheck && npm test

   # Repository gates
   cd ..
   node --test tools/*.test.mjs
   node tools/check-env-contract.mjs
   node tools/check-doc-translations.mjs
   node tools/check-ui-translations.mjs
   node tools/check-source-lines.mjs
   git diff --check
   ```

   The `internal/server` package holds thousands of tests and can exceed the default ten-minute Go timeout when run serially. Run it the way CI does:

   ```bash
   cd backend && node ../tools/test-backend-shard.mjs 0 4   # and 1, 2, 3
   ```

4. Commit with a Conventional Commits subject, English, imperative, under 72 characters. Group related work so each commit stays reviewable and independently revertible:

   ```
   feat(quota): add team quota pools with history-based dynamic shares
   fix(routing): qualify branch refs in the upstream sync workflow
   ```

5. Push the branch and open a pull request against `custom/main`. Use `.github/pull_request_template.md` for the body, keep every section, and write the whole pull request in English. Merge only after CI is green.

## Upstream synchronization

The `.github/workflows/sync-upstream.yml` workflow runs daily and on demand. It:

1. fast-forwards `main` from upstream, and fails closed if `main` ever diverges;
2. creates a branch and merges `main` into it;
3. opens a `chore: sync upstream <sha>` pull request against `custom/main`.

It never merges on its own. Review the pull request, wait for CI, and merge it yourself.

### When the merge conflicts

The workflow detects conflicts, aborts the merge, and pushes an empty marker commit so the pull request stays open with GitHub's conflict badge. Resolve it locally:

```bash
git switch chore/sync-upstream-<date>-<sha>
git fetch origin
git merge origin/custom/main
# resolve each conflicted file
git add <resolved files>
git commit
git push
```

The pull request turns green once CI passes on the resolved branch.

Where conflicts come from, and how to resolve them:

- **New customization files** never conflict. Most of the merge is automatic.
- **Wiring files** conflict: `backend/internal/server/store_call_admission.go`, `store_routing_calls.go`, `config.go`, `routes.go`, `types.go`, `admin_resources_routing.go`, and the frontend resource configs. Consult `CUSTOMIZATIONS.md` to see which customization each edit belongs to, then re-apply that customization on top of the upstream code.
- **Documentation and environment files** conflict when upstream edits the same table row. Keep both the upstream row and the customization row.

### When upstream claims a migration version

Local custom migrations occupy version numbers in the same sequence as upstream ones. The workflow compares both manifests and warns in the pull request body when the versions collide. When that happens:

```bash
# 1. Pick the next free version in backend/internal/server/store_schema_handle.go
# 2. Regenerate the embedded manifest
cd backend && go run ./cmd/manifestgen
# 3. Update the version pins that the migration touches:
#    - CurrentCompatibilityManifest() in backend/internal/server/version_compat.go
#    - the expected version in backend/internal/dbcli/cli_test.go
# 4. Re-run the backend checks and push
```

Record the renumbering in `CUSTOMIZATIONS.md`.

### When the sync workflow cannot run

- **The pull request does not start CI.** Pull requests created with the default `GITHUB_TOKEN` do not trigger workflows. The repository stores a fine-grained personal access token as the `SYNC_TOKEN1` secret so sync pull requests run CI automatically. If that secret is missing or expired, the pull request still opens; push an empty commit to the sync branch to start CI.
- **The workflow fails at the fast-forward step.** `main` diverged from upstream, which means something wrote to the mirror branch. Reset it to upstream and re-run: `git push origin upstream/main:main --force` from a checkout of `upstream/main`.
- **The workflow is paused.** GitHub disables scheduled workflows in repositories with no activity for sixty days. Reactivate them on the Actions tab.

## Keeping local changes upstreamable

Every local deviation is recorded in `CUSTOMIZATIONS.md` with its files, its purpose, and whether it can be proposed upstream. Update that table in the same commit as the change it describes.

Prefer upstreaming generic capabilities. A fix, a documentation improvement, or a feature that other deployments would want belongs in an upstream pull request; the fork then stops carrying it, and the next sync simply brings it back. Keep only business-specific behavior local.

## Checklist before opening a pull request

- [ ] The branch was cut from an up-to-date `custom/main`.
- [ ] `gofmt`, `go vet`, `golangci-lint`, the backend tests, the frontend checks, and the repository gates pass.
- [ ] `git diff --check` is clean and no runtime artifacts, local databases, backups, or `.env` files are included.
- [ ] Environment variable additions are synchronized across `backend/.env.example`, `deploy/.env.example`, `deploy/docker-compose.yml`, and the deployment documentation in all three languages.
- [ ] User-facing copy exists in the translation catalogs for English, Simplified Chinese, and Japanese, with Simplified Chinese as the source literal.
- [ ] `CUSTOMIZATIONS.md` reflects the change.
- [ ] The pull request body follows `.github/pull_request_template.md` and is written in English.
