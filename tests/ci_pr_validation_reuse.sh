#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/ci-reuse-pr-validation.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/repo" "$tmp/fakebin"
cd "$tmp/repo"
git init -q
git config user.name 'ci-evidence-fixture'
git config user.email 'ci-evidence@example.invalid'
printf 'baseline\n' > source.txt
git add source.txt
git commit -qm baseline
git branch -M main
base="$(git rev-parse HEAD)"
git switch -qc candidate
printf 'validated PR content\n' > source.txt
git add source.txt
git commit -qm candidate
candidate="$(git rev-parse HEAD)"
candidate_tree="$(git rev-parse HEAD^{tree})"
git switch -q main
git checkout -q "$candidate" -- source.txt
git add source.txt
git commit -qm 'squash merged candidate'
merged="$(git rev-parse HEAD)"
[ "$(git rev-parse HEAD^{tree})" = "$candidate_tree" ]

cat > "$tmp/fakebin/gh" <<'MOCK_GH'
#!/usr/bin/env bash
set -Eeuo pipefail
[ "${1:-}" = api ] || exit 3
path="${2:-}"
case "$path" in
  "repos/$GITHUB_REPOSITORY/commits/$TEST_MAIN/pulls")
    [ "${MOCK_CASE:-}" != api_error ] || exit 7
    if [ "${MOCK_CASE:-}" = no_pr ]; then echo '[]';exit 0;fi
    repo="$GITHUB_REPOSITORY"
    if [ "${MOCK_CASE:-}" = fork ];then repo='other/fork';fi
    base="$TEST_BASE"
    if [ "${MOCK_CASE:-}" = stale_base ];then base="$TEST_HEAD";fi
    jq -n --arg main "$TEST_MAIN" --arg head "$TEST_HEAD" --arg base "$base" --arg repo "$repo" \
      '[{number:91,state:"closed",merged_at:"2026-10-09T05:33:15Z",merge_commit_sha:$main,
          base:{ref:"main",sha:$base},head:{sha:$head,ref:"candidate",repo:{full_name:$repo}}}]' |
      if [ "${MOCK_CASE:-}" = ambiguous_pr ]; then jq '. + .'
      elif [ "${MOCK_CASE:-}" = second_same_merge_wrong_base ]; then
        jq '. + [ (.[0] | .number=92 | .base.sha="0000000000000000000000000000000000000000") ]'
      elif [ "${MOCK_CASE:-}" = missing_pr_number ]; then jq '.[0] |= del(.number)'
      else cat;fi
    ;;
  "repos/$GITHUB_REPOSITORY/git/commits/$TEST_HEAD")
    [ "${MOCK_CASE:-}" != failed_tree_lookup ] || exit 6
    tree="$TEST_HEAD_TREE"
    if [ "${MOCK_CASE:-}" = changed_tree ];then tree="$TEST_BASE_TREE";fi
    if [ "${3:-}" = --jq ];then echo "$tree";else jq -n --arg tree "$tree" '{tree:{sha:$tree}}';fi
    ;;
  "repos/$GITHUB_REPOSITORY/compare/$TEST_BASE...$TEST_HEAD")
    if [ "${MOCK_CASE:-}" = bad_ancestry ];then
      jq -n --arg head "$TEST_HEAD" '{status:"diverged",behind_by:1,merge_base_commit:{sha:$head}}'
    else
      jq -n --arg base "$TEST_BASE" '{status:"ahead",behind_by:0,merge_base_commit:{sha:$base}}'
    fi
    ;;
  "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs?"*|"repos/$GITHUB_REPOSITORY/actions/workflows/security.yml/runs?"*)
    case "$path" in
      */ci.yml/*) kind=CI; key=ci;;
      */security.yml/*) kind=Security; key=security;;
    esac
    [ "${MOCK_CASE:-}" != "failed_${key}_lookup" ] || exit 7
    if [ "${MOCK_CASE:-}" = "missing_${key}" ]; then echo '{"total_count":0,"workflow_runs":[]}';exit 0;fi
    conclusion=success
    [ "${MOCK_CASE:-}" != "failed_${key}" ] || conclusion=failure
    jq -n --arg sha "$TEST_HEAD" --arg base "$TEST_BASE" --arg repo "$GITHUB_REPOSITORY" --arg result "$conclusion" \
      --arg kind "$kind" --arg key "$key" \
      '{total_count:1,workflow_runs:[{
        event:"pull_request",head_sha:$sha,head_branch:"candidate",
        head_repository:{full_name:$repo},repository:{full_name:$repo},
        path:(".github/workflows/"+$key+".yml"),
        display_title:($kind+" pull_request PR:91 REF:main BASE:"+$base+" HEAD:"+$sha),
        pull_requests:[{number:91,head:{sha:$sha,ref:"candidate"},base:{sha:$base,ref:"main"}}],
        created_at:"2026-10-09T05:16:57Z",updated_at:"2026-10-09T05:20:51Z",
        status:"completed",conclusion:$result,run_attempt:1
      }]}' |
      case "${MOCK_CASE:-}" in
        latest_ci_failure)
          jq '.workflow_runs += [(.workflow_runs[0]|.created_at="2026-10-09T05:23:00Z"|.updated_at="2026-10-09T05:25:51Z"|.conclusion="failure")] | .total_count=2';;
        latest_ci_pending)
          jq '.workflow_runs += [(.workflow_runs[0]|.created_at="2026-10-09T05:23:00Z"|.updated_at="2026-10-09T05:35:51Z"|.status="in_progress"|.conclusion=null)] | .total_count=2';;
        missing_ci_timestamp)
          jq '.workflow_runs[0] |= del(.updated_at)';;
        missing_ci_link|missing_security_link)
          jq '.workflow_runs[0] |= del(.pull_requests,.display_title)';;
        empty_ci_link_without_name|empty_security_link_without_name)
          jq '.workflow_runs[0].pull_requests=[] | .workflow_runs[0].display_title="PR title without canonical identity"';;
        empty_ci_link_valid_title|empty_security_link_valid_title|both_cleared)
          jq '.workflow_runs[0].pull_requests=[]';;
        ambiguous_ci_link|ambiguous_security_link)
          jq '.workflow_runs[0].pull_requests += [(.workflow_runs[0].pull_requests[0]|.number=92)]';;
        wrong_number_ci|wrong_number_security)
          jq '.workflow_runs[0].pull_requests[0].number=92';;
        wrong_base_ci|wrong_base_security)
          jq --arg sha "$TEST_HEAD" '.workflow_runs[0].pull_requests[0].base.sha=$sha';;
        wrong_base_ref_ci|wrong_base_ref_security)
          jq '.workflow_runs[0].pull_requests[0].base.ref="not-main"';;
        wrong_head_ci|wrong_head_security)
          jq --arg sha "$TEST_BASE" '.workflow_runs[0].pull_requests[0].head.sha=$sha';;
        foreign_ci_only|foreign_security_only)
          jq '.workflow_runs[0].pull_requests[0].number=92 | .workflow_runs[0].display_title |= sub("PR:91";"PR:92")';;
        foreign_ci_masks_failure|foreign_security_masks_failure)
          jq '.workflow_runs[0].conclusion="failure" | .workflow_runs += [(.workflow_runs[0]|.pull_requests[0].number=92|.display_title |= sub("PR:91";"PR:92")|.created_at="2026-10-09T05:23:00Z"|.updated_at="2026-10-09T05:25:51Z"|.conclusion="success")] | .total_count=2';;
        foreign_ci_newer|foreign_security_newer)
          jq '.workflow_runs += [(.workflow_runs[0]|.pull_requests[0].number=92|.display_title |= sub("PR:91";"PR:92")|.created_at="2026-10-09T05:23:00Z"|.updated_at="2026-10-09T05:25:51Z")] | .total_count=2';;
        foreign_ci_cleared|foreign_security_cleared)
          jq '.workflow_runs[0].pull_requests=[] | .workflow_runs[0].display_title |= sub("PR:91";"PR:92")';;
        ambiguous_ci_cleared|ambiguous_security_cleared)
          jq '.workflow_runs[0].pull_requests=[] | .workflow_runs[0].display_title |= sub("BASE:[0-9a-f]+";"BASE:other")';;
        wrong_ref_ci_cleared|wrong_ref_security_cleared)
          jq '.workflow_runs[0].pull_requests=[] | .workflow_runs[0].display_title |= sub("REF:main";"REF:develop")';;
        # All these cases begin with a real completed/successful merged-PR
        # run. A newer same-head run has unresolved evidence. The older green
        # MUST NOT win merely because the uncertainty got filtered out.
        newer_ci_missing_link|newer_security_missing_link)
          jq '.workflow_runs += [(.workflow_runs[0] | del(.pull_requests,.display_title)
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_malformed_link|newer_security_malformed_link)
          jq '.workflow_runs += [(.workflow_runs[0] | .pull_requests="not-an-array"
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_ambiguous_link|newer_security_ambiguous_link)
          jq '.workflow_runs += [(.workflow_runs[0] | .pull_requests += [(.pull_requests[0]|.number=92)]
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_noncanonical_title|newer_security_noncanonical_title)
          jq '.workflow_runs += [(.workflow_runs[0] | .pull_requests=[] | .display_title="untrusted free-form title"
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_contradictory_title|newer_security_contradictory_title)
          jq '.workflow_runs += [(.workflow_runs[0] | .pull_requests[0].number=92
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_mismatched_base|newer_security_mismatched_base)
          jq '.workflow_runs += [(.workflow_runs[0] | .pull_requests[0].base.sha="0000000000000000000000000000000000000000"
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_missing_creation|newer_security_missing_creation)
          jq '.workflow_runs += [(.workflow_runs[0] | del(.created_at)
            | .updated_at="2026-10-09T05:26:00Z")] | .total_count=2';;
        newer_ci_bad_attempt|newer_security_bad_attempt)
          jq '.workflow_runs += [(.workflow_runs[0] | .run_attempt="bad"
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_exact_failed_attempt|newer_security_exact_failed_attempt)
          jq '.workflow_runs += [(.workflow_runs[0] | .run_attempt=2 | .conclusion="failure"
            | .created_at="2026-10-09T05:16:57Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_missing_branch|newer_security_missing_branch)
          jq '.workflow_runs += [(.workflow_runs[0] | del(.head_branch)
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_missing_repo|newer_security_missing_repo)
          jq '.workflow_runs += [(.workflow_runs[0] | del(.head_repository)
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_missing_path|newer_security_missing_path)
          jq '.workflow_runs += [(.workflow_runs[0] | del(.path)
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_same_timestamp_tie|newer_security_same_timestamp_tie)
          jq '.workflow_runs = [(.workflow_runs[0]|.conclusion="failure"), .workflow_runs[0]]
            | .total_count=2';;
        newer_ci_other_branch|newer_security_other_branch)
          jq '.workflow_runs += [(.workflow_runs[0] | .head_branch="different-source-branch"
            | .created_at="2026-10-09T05:23:00Z" | .updated_at="2026-10-09T05:26:00Z")]
            | .total_count=2';;
        newer_ci_postmerge|newer_security_postmerge)
          jq '.workflow_runs += [(.workflow_runs[0] | .created_at="2026-10-09T05:40:00Z"
            | .updated_at="2026-10-09T05:45:00Z" | .conclusion="failure")]
            | .total_count=2';;
        truncated_ci_list|truncated_security_list)
          jq '.total_count=101';;
        *) cat;;
      esac
    ;;
  *) echo "unexpected gh mock request $path" >&2; exit 3;;
esac
MOCK_GH
chmod 700 "$tmp/fakebin/gh"

export GITHUB_EVENT_NAME=push GITHUB_REF=refs/heads/main GITHUB_REPOSITORY=test-owner/test-repo
export GITHUB_SHA="$merged" GH_TOKEN=offline-test TEST_MAIN="$merged" TEST_BASE="$base" TEST_HEAD="$candidate"
export TEST_HEAD_TREE="$candidate_tree" TEST_BASE_TREE="$(git rev-parse "$base^{tree}")"
export PATH="$tmp/fakebin:$PATH" GITHUB_EVENT_PATH="$tmp/push.json"
jq -n --arg before "$base" --arg after "$merged" --arg repo "$GITHUB_REPOSITORY" \
  '{before:$before,after:$after,ref:"refs/heads/main",repository:{full_name:$repo},forced:false,deleted:false,created:false}' > "$GITHUB_EVENT_PATH"

cases_checked=0
check(){
  local what="$1" want="$2" got
  ((cases_checked += 1))
  export MOCK_CASE="$what"
  got="$(bash "$SCRIPT" 2>"$tmp/last-stderr")" || { echo "reuse proof unexpectedly failed: $what" >&2;cat "$tmp/last-stderr" >&2;exit 1; }
  [ "$got" = "reuse_validated_pr=$want" ] || {
    echo "bad evidence selection: case=$what got=$got want=$want" >&2
    cat "$tmp/last-stderr" >&2
    exit 1
  }
}

check matching_squash true
# The required jobs, protected PR validations and exact-SHA main artifact are
# separate paths; ensure the proof output is consumed by both workflows.
grep -qF 'reuse_validated_pr: ${{ steps.reuse.outputs.reuse_validated_pr }}' "$ROOT/.github/workflows/ci.yml"
grep -qF 'reuse_validated_pr: ${{ steps.reuse.outputs.reuse_validated_pr }}' "$ROOT/.github/workflows/security.yml"
grep -qF "needs.change-scope.outputs.reuse_validated_pr != 'true'" "$ROOT/.github/workflows/ci.yml"
grep -qF "needs.change-scope.outputs.reuse_validated_pr != 'true'" "$ROOT/.github/workflows/security.yml"
grep -qF "needs.change-scope.outputs.reuse_validated_pr == 'true'" "$ROOT/.github/workflows/ci.yml"
grep -qF 'gh run download "$run_id"' "$ROOT/.github/workflows/release.yml"
# Ensure the fallback's trusted GitHub-event run-name contract remains in both
# workflows. No editable PR title, free-form workflow input, or user text.
grep -qF 'run-name: "CI ${{ github.event_name }} PR:${{ github.event.pull_request.number || '"'"'none'"'"' }} REF:${{ github.event.pull_request.base.ref || '"'"'none'"'"' }} BASE:${{ github.event.pull_request.base.sha || '"'"'none'"'"' }} HEAD:${{ github.event.pull_request.head.sha || github.sha }}"' "$ROOT/.github/workflows/ci.yml"
grep -qF 'run-name: "Security ${{ github.event_name }} PR:${{ github.event.pull_request.number || '"'"'none'"'"' }} REF:${{ github.event.pull_request.base.ref || '"'"'none'"'"' }} BASE:${{ github.event.pull_request.base.sha || '"'"'none'"'"' }} HEAD:${{ github.event.pull_request.head.sha || github.sha }}"' "$ROOT/.github/workflows/security.yml"
# Distinguish foreign PRs sharing the SAME head SHA/branch/workflow from the
# actual merged PR. Later foreign green must not mask failed/missing matching.
check empty_ci_link_valid_title true
check empty_security_link_valid_title true
check both_cleared true
check foreign_ci_newer true
check foreign_security_newer true
check newer_ci_postmerge true
check newer_security_postmerge true
check newer_ci_other_branch true
check newer_security_other_branch true
for scenario in no_pr api_error fork ambiguous_pr second_same_merge_wrong_base missing_pr_number stale_base changed_tree failed_tree_lookup bad_ancestry \
  missing_ci failed_ci latest_ci_failure latest_ci_pending missing_ci_timestamp \
  missing_security failed_security failed_security_lookup \
  foreign_ci_only foreign_security_only foreign_ci_masks_failure foreign_security_masks_failure \
  missing_ci_link missing_security_link empty_ci_link_without_name empty_security_link_without_name \
  ambiguous_ci_link ambiguous_security_link wrong_number_ci wrong_number_security \
  wrong_base_ci wrong_base_security wrong_base_ref_ci wrong_base_ref_security \
  wrong_head_ci wrong_head_security foreign_ci_cleared foreign_security_cleared \
  ambiguous_ci_cleared ambiguous_security_cleared wrong_ref_ci_cleared wrong_ref_security_cleared \
  truncated_ci_list truncated_security_list \
  newer_ci_missing_link newer_security_missing_link \
  newer_ci_malformed_link newer_security_malformed_link \
  newer_ci_ambiguous_link newer_security_ambiguous_link \
  newer_ci_noncanonical_title newer_security_noncanonical_title \
  newer_ci_contradictory_title newer_security_contradictory_title \
  newer_ci_mismatched_base newer_security_mismatched_base \
  newer_ci_missing_creation newer_security_missing_creation \
  newer_ci_bad_attempt newer_security_bad_attempt \
  newer_ci_exact_failed_attempt newer_security_exact_failed_attempt \
  newer_ci_missing_branch newer_security_missing_branch \
  newer_ci_missing_repo newer_security_missing_repo \
  newer_ci_missing_path newer_security_missing_path \
  newer_ci_same_timestamp_tie newer_security_same_timestamp_tie;do
  check "$scenario" false
done

jq '.forced=true' "$GITHUB_EVENT_PATH" > "$tmp/changed.json"
cp "$tmp/changed.json" "$GITHUB_EVENT_PATH"
check matching_squash false
jq 'del(.forced)' "$GITHUB_EVENT_PATH" > "$tmp/changed.json"
cp "$tmp/changed.json" "$GITHUB_EVENT_PATH"
check matching_squash false
jq '.forced=false | .before=.after' "$GITHUB_EVENT_PATH" > "$tmp/changed.json"
cp "$tmp/changed.json" "$GITHUB_EVENT_PATH"
check matching_squash false
jq --arg before "$base" '.before=$before | .after=.before' "$GITHUB_EVENT_PATH" > "$tmp/changed.json"
cp "$tmp/changed.json" "$GITHUB_EVENT_PATH"
check matching_squash false
jq --arg before "$base" --arg after "$merged" '.before=$before | .after=$after' "$GITHUB_EVENT_PATH" > "$tmp/changed.json"
cp "$tmp/changed.json" "$GITHUB_EVENT_PATH"
GITHUB_EVENT_NAME=pull_request check matching_squash false
GITHUB_REF=refs/heads/other check matching_squash false
GITHUB_SHA="$base" check matching_squash false
GH_TOKEN= check matching_squash false
GITHUB_REPOSITORY=wrong/repo check matching_squash false

# A historical valid squash, checked from the actual public GitHub API in
# developer smoke checks, provides a non-mock confirmation of API semantics.
echo "CI PR evidence reuse tests passed ($cases_checked valid, adversarial and fail-closed scenarios)."
