#!/usr/bin/env bash
set -Eeuo pipefail

# Fail-closed, read-only admission check for reusing completed PR CI/Security
# evidence on a single squash-merged main push. The release builder still
# runs for the exact main SHA. A missing/ambiguous proof runs full main suites.
refuse(){
  printf 'reuse_validated_pr=false\n'
  printf 'CI reuse: full validation required (%s).\n' "$1" >&2
  exit 0
}

[ "${GITHUB_EVENT_NAME:-}" = push ] || refuse 'not a main push'
[ "${GITHUB_REF:-}" = refs/heads/main ] || refuse 'not a main push'
[ -n "${GITHUB_EVENT_PATH:-}" ] && [ -r "$GITHUB_EVENT_PATH" ] || refuse 'missing push event'
command -v gh >/dev/null 2>&1 || refuse 'GitHub API client unavailable'
command -v jq >/dev/null 2>&1 || refuse 'JSON parser unavailable'
[ -n "${GH_TOKEN:-}" ] || refuse 'read-only Actions token unavailable'

repo="${GITHUB_REPOSITORY:-}"
sha="${GITHUB_SHA:-}"
[[ "$repo" =~ ^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$ ]] || refuse 'invalid repository identity'
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || refuse 'invalid main commit SHA'

# Require a single ordinary squash commit. This excludes direct/multi-commit
# pushes, merge commits, force pushes, and ambiguous history transitions.
before="$(jq -r '.before // empty' "$GITHUB_EVENT_PATH" 2>/dev/null)" || refuse 'malformed push event'
after="$(jq -r '.after // empty' "$GITHUB_EVENT_PATH" 2>/dev/null)" || refuse 'malformed push event'
[ "$after" = "$sha" ] || refuse 'push SHA mismatch'
[[ "$before" =~ ^[0-9a-f]{40}$ ]] || refuse 'invalid before SHA'
[ "$before" != 0000000000000000000000000000000000000000 ] || refuse 'new branch push'
jq -e --arg repo "$repo" '.forced == false and .deleted == false and .created == false and .ref == "refs/heads/main" and .repository.full_name == $repo' "$GITHUB_EVENT_PATH" >/dev/null 2>&1 || refuse 'unsafe push event'
[ "$(git rev-parse HEAD 2>/dev/null)" = "$sha" ] || refuse 'checkout does not match push'
parents="$(git show -s --format='%P' "$sha" 2>/dev/null)" || refuse 'missing pushed commit'
[ "$parents" = "$before" ] || refuse 'not a single squash commit based on before'
main_tree="$(git rev-parse "$sha^{tree}" 2>/dev/null)" || refuse 'unreadable main tree'

# GitHub's associated-PR endpoint retains the merged PR linkage even when
# source branches are later deleted. Refuse ambiguous or cross-repository PRs.
prs="$(gh api "repos/$repo/commits/$sha/pulls" 2>/dev/null)" || refuse 'merged PR lookup failed'
pr="$(jq -er --arg repo "$repo" --arg sha "$sha" --arg base "$before" '
  [.[] | select(.merged_at != null and .state == "closed"
     and .merge_commit_sha == $sha and .base.ref == "main" and .base.sha == $base
     and .head.repo.full_name == $repo and (.head.sha|type) == "string"
     and (.head.ref|type) == "string")]
  | select(length == 1) | .[0]
' <<< "$prs" 2>/dev/null)" || refuse 'no unambiguous merged PR for this main parent'
head="$(jq -r '.head.sha' <<< "$pr")"
branch="$(jq -r '.head.ref' <<< "$pr")"
merged_at="$(jq -r '.merged_at' <<< "$pr")"
[[ "$head" =~ ^[0-9a-f]{40}$ ]] || refuse 'invalid PR head'
[ -n "$branch" ] && [ "$branch" != null ] || refuse 'invalid PR branch'
[[ "$merged_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T ]] || refuse 'missing merge time'

head_tree="$(gh api "repos/$repo/git/commits/$head" --jq '.tree.sha' 2>/dev/null)" || refuse 'PR head tree lookup failed'
[ "$head_tree" = "$main_tree" ] || refuse 'main tree differs from validated PR head'

# If the previous main is an ancestor of the tested PR head, GitHub's PR
# synthetic merge tests exactly that head tree. This avoids guessing whether
# a stale PR merge-ref included additional base changes.
compare="$(gh api "repos/$repo/compare/$before...$head" 2>/dev/null)" || refuse 'PR ancestry lookup failed'
jq -e --arg base "$before" '
  .merge_base_commit.sha == $base and .behind_by == 0
  and (.status == "ahead" or .status == "identical")
' <<< "$compare" >/dev/null 2>&1 || refuse 'PR head not verified against merged main parent'

# Verify latest PR event runs for BOTH entire workflows, not merely isolated
# required jobs. Inspect the last run that STARTED before the merge, even if
# it was still running then: a past green must not hide a newer pending or
# failing attempt. Any ambiguity forces original full main validation.
for workflow in ci.yml security.yml; do
  runs="$(gh api "repos/$repo/actions/workflows/$workflow/runs?head_sha=$head&event=pull_request&per_page=100" 2>/dev/null)" || refuse "$workflow PR run lookup failed"
  jq -e --arg repo "$repo" --arg head "$head" --arg branch "$branch" --arg merged "$merged_at" --arg path ".github/workflows/$workflow" '
    [.workflow_runs[]?
     | select(.event == "pull_request" and .head_sha == $head and .head_branch == $branch
         and .head_repository.full_name == $repo and .repository.full_name == $repo
         and .path == $path and (.created_at | type) == "string"
         and .created_at <= $merged)]
    | sort_by(.created_at, .run_attempt) | last
    | (.updated_at | type) == "string" and .updated_at <= $merged
      and .status == "completed" and .conclusion == "success"
  ' <<< "$runs" >/dev/null 2>&1 || refuse "$workflow has no completed successful exact-head PR evidence"
done

printf 'reuse_validated_pr=true\n'
printf 'CI reuse: verified exact code tree, main parent, PR and successful CI/Security runs.\n' >&2
