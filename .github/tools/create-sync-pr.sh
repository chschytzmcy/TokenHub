#!/usr/bin/env bash
# Build the upstream sync pull request body following
# .github/pull_request_template.md and open the PR against custom/main.
# Called by .github/workflows/sync-upstream.yml; all inputs arrive as
# environment variables so the multi-line commit list never touches argv.
set -euo pipefail

: "${BRANCH:?BRANCH is required}"
: "${UPSTREAM_SHA:?UPSTREAM_SHA is required}"
: "${COMMIT_COUNT:?COMMIT_COUNT is required}"
: "${CONFLICTS:?CONFLICTS is required}"
COMMITS="${COMMITS:-}"
COLLISIONS="${COLLISIONS:-}"

if [ "${CONFLICTS}" = "true" ]; then
  summary="Automated upstream sync against ${UPSTREAM_REPOSITORY:?UPSTREAM_REPOSITORY is required}@${UPSTREAM_SHA} (${COMMIT_COUNT} commits). Merging main into custom/main produced conflicts: resolve them on this branch, then push."
  changes="- Merge main into custom/main. The automated merge stopped on conflicts; \`git merge main\` on this branch shows the conflicted files."
  conflicts_note="- Resolve the conflicts, re-run the repository gates locally, and push. The PR then turns green."
  verification="- Pending: resolve conflicts locally, then rely on CI on this PR."
  tests_checkbox="[ ]"
else
  summary="Automated upstream sync: fast-forwarded fork main to ${UPSTREAM_REPOSITORY}@${UPSTREAM_SHA} and prepared the merge into custom/main (${COMMIT_COUNT} upstream commits)."
  changes="- Fast-forwarded fork main to upstream/main (${UPSTREAM_SHA}).
- Merged main into custom/main on this branch; CI validates the result."
  conflicts_note=""
  verification="- CI runs on this PR automatically."
  tests_checkbox="[x]"
fi

collision_note=""
collision_compat="None."
if [ -n "${COLLISIONS}" ]; then
  collision_note="- WARNING: upstream introduced schema migration version(s) ${COLLISIONS} that local custom migrations also use. Renumber the local migrations in backend/internal/server/store_schema_handle.go to the next free versions, regenerate internal/dbschema/migrations_manifest.json with \`go run ./cmd/manifestgen\`, and adjust the schema version pins noted in CUSTOMIZATIONS.md before merging."
  collision_compat="Schema migration collisions must be resolved as described above before merging."
fi

body_file="${RUNNER_TEMP:-/tmp}/pr-body.md"
cat > "${body_file}" <<SYNC_EOF
## Summary

${summary}

## Related Issue

N/A (automated upstream sync).

## Changes

${changes}
${conflicts_note}${collision_note}

Upstream commits in this sync:

\`\`\`
${COMMITS}
\`\`\`

## Type of Change

- [x] Refactor or maintenance

## Verification

${verification}

## Compatibility, Security, and Operations

${collision_compat}

## Checklist

- [x] The PR title and body are written in English.
- ${tests_checkbox} Tests were added or updated for behavior changes, or the reason they are unnecessary is documented.
- [x] No credentials, local \`.env\` files, databases, backups, or runtime logs are included.
- ${tests_checkbox} Environment variable changes are synchronized across examples, Compose, \`start.sh\`, and deployment documentation where applicable.
- ${tests_checkbox} Shared user-facing behavior is documented consistently in English, Simplified Chinese, and Japanese where applicable.
- [x] \`data/model-catalog.yaml\` remains tracked and catalog changes were reviewed where applicable.
- ${tests_checkbox} \`git diff --check\` passes.
SYNC_EOF

gh pr create \
  --base custom/main \
  --head "${BRANCH}" \
  --title "chore: sync upstream ${UPSTREAM_SHA}" \
  --body-file "${body_file}"
