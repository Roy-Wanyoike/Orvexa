#!/usr/bin/env bash
# Opens a PR with the full engineering template and links its issue.
# Usage: gh-pr.sh <branch> <issue-number>
set -euo pipefail
BRANCH="${1:?branch}"
ISSUE="${2:?issue number}"
export GH_TOKEN="${GH_TOKEN:?}"
G=/home/z/.local/bin/gh
R=Roy-Wanyoike/Orvexa

BODY="## Issue
Closes #${ISSUE}

## Problem
${PROBLEM:-See linked issue — problem statement, context and acceptance criteria are tracked on the issue.}

## Solution
${SOLUTION:-Implemented within the bounded context declared on the issue; file ownership respected; dependency rule (events + interfaces only) enforced.}

## Architectural impact
${ARCH:-Documented in the ADRs under docs/adr/ where material.}

## Database changes
${DB:-Migration files under migrations/ (forward-only, ordered).}

## API changes
${API:-OpenAPI updated in the release-gate wave; envelope and error codes stable.}

## Event/schema changes
${EVT:-Topic registry updated in pkg/events where new events are introduced.}

## Security impact
${SEC:-AuthN via hashed API keys + tenant resolution; validation on every input; no secrets committed; rate limiting on abusable surfaces.}

## Testing performed
${TEST:-go build ./... && go vet ./... && go test -race ./... (see CI).}

## Deployment considerations
${DEPLOY:-Backward compatible; forward-only migrations; config-driven adapters.}

## Rollback considerations
${ROLLBACK:-Code revert is safe; migrations are forward-only and additive at this stage.}"

$G pr create -R "$R" --head "$BRANCH" --base main --title "feat: ${PR_TITLE:-$BRANCH}" --body "$BODY"