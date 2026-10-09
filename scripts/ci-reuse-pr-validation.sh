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

# GitHub's associated-PR endpoint retains the merged PR linkage after branch
# deletion, but is PAGINATED. Uniqueness is meaningless over just page 1.
# Explicitly bound a single-page result (<100) and prove page 2 is empty.
# A full page, later-page entries, any bad shape or API error => full main CI.
# Even unrelated later-page PRs are conservatively refused; successful reuse
# is an optimization, not a reason to weaken the uniqueness trust anchor.
prs="$(gh api "repos/$repo/commits/$sha/pulls?per_page=100&page=1" 2>/dev/null)" || refuse 'merged PR page 1 lookup failed'
jq -se 'length == 1 and (.[0] | type == "array" and length < 100)' <<< "$prs" >/dev/null 2>&1 || refuse 'merged PR page 1 incomplete or malformed'
more_prs="$(gh api "repos/$repo/commits/$sha/pulls?per_page=100&page=2" 2>/dev/null)" || refuse 'merged PR page 2 completeness lookup failed'
jq -se 'length == 1 and (.[0] | type == "array" and length == 0)' <<< "$more_prs" >/dev/null 2>&1 || refuse 'merged PR inventory has additional pages or invalid evidence'

# We have proved the bounded PR inventory is complete; only NOW may a unique
# exact merge-commit association be selected. Reject conflicting associations.
pr="$(jq -er --arg repo "$repo" --arg sha "$sha" --arg base "$before" '
  select(type == "array")
  | [.[] | select(.merge_commit_sha == $sha)]
  | select(length == 1) | .[0]
  | select(.merged_at != null and .state == "closed"
       and .base.ref == "main" and .base.sha == $base
       and .head.repo.full_name == $repo
       and (.head.sha | type) == "string"
       and (.head.ref | type) == "string")
' <<< "$prs" 2>/dev/null)" || refuse 'no uniquely linked merged PR matching the main parent'
pr_number="$(jq -er '.number | select(type == "number" and . > 0 and . == floor)' <<< "$pr" 2>/dev/null)" || refuse 'missing exact merged PR number'
head="$(jq -r '.head.sha' <<< "$pr")"
branch="$(jq -r '.head.ref' <<< "$pr")"
merged_at="$(jq -r '.merged_at' <<< "$pr")"
[[ "$head" =~ ^[0-9a-f]{40}$ ]] || refuse 'invalid PR head'
[ -n "$branch" ] && [ "$branch" != null ] || refuse 'invalid PR branch'
[[ "$merged_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || refuse 'missing or invalid merge time'

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

# The trust decision has three states, not a Boolean filter:
#   exact      -- a run provably belongs to the uniquely merged PR;
#   foreign    -- a different, positively identified PR using this SHA/ref;
#   unresolved -- missing, malformed, ambiguous or contradictory evidence.
#
# IMPORTANT: classify ALL otherwise-relevant pre-merge runs BEFORE choosing
# the latest exact-PR attempt. Never drop an unresolved newer run: otherwise
# an older green would incorrectly masquerade as latest valid evidence.
# GitHub clears pull_requests metadata on some merged PR runs. In that one
# known case (an explicitly empty array), the canonical GitHub-event run-name
# provides the persisted PR/base/head identity; an absent array does not.
for workflow in ci.yml security.yml; do
  if [ "$workflow" = ci.yml ]; then kind=CI; else kind=Security; fi
  expected_title="$kind pull_request PR:$pr_number REF:main BASE:$before HEAD:$head"
  runs="$(gh api "repos/$repo/actions/workflows/$workflow/runs?head_sha=$head&event=pull_request&per_page=100" 2>/dev/null)" || refuse "$workflow PR run lookup failed"
  jq -e --arg repo "$repo" --arg head "$head" --arg branch "$branch" --arg base "$before" \
    --argjson number "$pr_number" --arg title "$expected_title" --arg kind "$kind" \
    --arg merged "$merged_at" --arg path ".github/workflows/$workflow" '
      def is_sha: type == "string" and test("^[0-9a-f]{40}$");
      def timestamp_ok:
        type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$");
      def canonical_prefix: $kind + " pull_request PR:";
      def valid_attempt:
        (.run_attempt | type) == "number"
        and .run_attempt > 0
        and .run_attempt == (.run_attempt | floor);
      # The workflow/head/event query itself must not yield partially
      # identified records; those cannot be safely excluded as unrelated.
      def api_record_complete:
        .event == "pull_request" and .head_sha == $head
        and .head_repository.full_name == $repo and .repository.full_name == $repo
        and .path == $path and (.head_branch | type) == "string";
      def coarse_run: .head_branch == $branch;
      def classify:
        if (.pull_requests | type) != "array" then "unresolved"
        elif (.pull_requests | length) == 0 then
          if .display_title == $title then "exact" else "unresolved" end
        elif (.pull_requests | length) == 1 then
          .pull_requests[0] as $link
          | if ($link.number | type) != "number"
             or $link.number <= 0 or $link.number != ($link.number | floor)
             or $link.head.sha != $head or $link.head.ref != $branch
             or ($link.base.sha | is_sha | not)
             or ($link.base.ref | type) != "string"
             or ($link.base.ref | length) == 0
            then "unresolved"
            else
              # Any visible canonical title contradicting structured linkage
              # is unresolved. Legacy noncanonical PR titles are permitted
              # only when structured metadata proves the PR identity.
              ($kind + " pull_request PR:" + ($link.number | tostring)
                + " REF:" + $link.base.ref
                + " BASE:" + $link.base.sha
                + " HEAD:" + $link.head.sha) as $linked_title
              | if (.display_title | type) == "string"
                   and (.display_title | startswith(canonical_prefix))
                   and .display_title != $linked_title
                then "unresolved"
                elif $link.number == $number then
                  if $link.base.ref == "main" and $link.base.sha == $base
                  then "exact" else "unresolved" end
                else "foreign" end
            end
        else "unresolved" end;
      (.total_count | type) == "number"
      and (.total_count | floor) == .total_count
      and (.total_count >= 0)
      and (.workflow_runs | type) == "array"
      and (.total_count == (.workflow_runs | length))
      and all(.workflow_runs[]; api_record_complete)
      and (
        (.workflow_runs | map(select(coarse_run))) as $same
        | all($same[]; (.created_at | timestamp_ok) and valid_attempt)
          and (
            ($same | map(select(.created_at <= $merged) | . + {pr_identity: classify})) as $prior
            | ([$prior[] | select(.pr_identity == "unresolved")] | length) == 0
              and (
                ([$prior[] | select(.pr_identity == "exact")]
                | sort_by(.created_at, .run_attempt)) as $exact
                | ($exact | length) > 0
                  and ($exact[-1] as $latest
                    | ([$exact[] | select(.created_at == $latest.created_at
                           and .run_attempt == $latest.run_attempt)] | length) == 1
                      and ($latest.updated_at | timestamp_ok)
                      and $latest.updated_at <= $merged
                      and $latest.status == "completed"
                      and $latest.conclusion == "success")
              )
          )
      )
  ' <<< "$runs" >/dev/null 2>&1 || refuse "$workflow has unresolved or unsuccessful exact-PR workflow evidence"
done

printf 'reuse_validated_pr=true\n'
printf 'CI reuse: verified exact Git tree, merged PR number/base/head, and successful CI/Security runs.\n' >&2
