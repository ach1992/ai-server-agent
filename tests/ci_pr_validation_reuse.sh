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
      if [ "${MOCK_CASE:-}" = ambiguous_pr ];then jq '. + .';else cat;fi
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
  "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs?"*)
    if [ "${MOCK_CASE:-}" = missing_ci ];then echo '{"workflow_runs":[]}';exit 0;fi
    conclusion=success
    if [ "${MOCK_CASE:-}" = failed_ci ];then conclusion=failure;fi
    jq -n --arg sha "$TEST_HEAD" --arg repo "$GITHUB_REPOSITORY" --arg result "$conclusion" \
      '{workflow_runs:[{event:"pull_request",head_sha:$sha,head_branch:"candidate",head_repository:{full_name:$repo},repository:{full_name:$repo},path:".github/workflows/ci.yml",created_at:"2026-10-09T05:16:57Z",updated_at:"2026-10-09T05:20:51Z",status:"completed",conclusion:$result,run_attempt:1}]}' |
      if [ "${MOCK_CASE:-}" = latest_ci_failure ]; then
        jq '.workflow_runs += [(.workflow_runs[0]|.created_at="2026-10-09T05:23:00Z"|.updated_at="2026-10-09T05:25:51Z"|.conclusion="failure") ]'
      elif [ "${MOCK_CASE:-}" = latest_ci_pending ]; then
        jq '.workflow_runs += [(.workflow_runs[0]|.created_at="2026-10-09T05:23:00Z"|.updated_at="2026-10-09T05:35:51Z"|.status="in_progress"|.conclusion=null) ]'
      elif [ "${MOCK_CASE:-}" = missing_ci_timestamp ]; then
        jq '.workflow_runs[0] |= del(.updated_at)'
      else cat;fi
    ;;
  "repos/$GITHUB_REPOSITORY/actions/workflows/security.yml/runs?"*)
    [ "${MOCK_CASE:-}" != failed_security_lookup ] || exit 6
    if [ "${MOCK_CASE:-}" = missing_security ];then echo '{"workflow_runs":[]}';exit 0;fi
    conclusion=success
    if [ "${MOCK_CASE:-}" = failed_security ];then conclusion=failure;fi
    jq -n --arg sha "$TEST_HEAD" --arg repo "$GITHUB_REPOSITORY" --arg result "$conclusion" \
      '{workflow_runs:[{event:"pull_request",head_sha:$sha,head_branch:"candidate",head_repository:{full_name:$repo},repository:{full_name:$repo},path:".github/workflows/security.yml",created_at:"2026-10-09T05:16:57Z",updated_at:"2026-10-09T05:20:51Z",status:"completed",conclusion:$result,run_attempt:1}]}'
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

check(){
  local what="$1" want="$2" got
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
for scenario in no_pr api_error fork ambiguous_pr stale_base changed_tree failed_tree_lookup bad_ancestry missing_ci failed_ci latest_ci_failure latest_ci_pending missing_ci_timestamp missing_security failed_security failed_security_lookup;do
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
echo 'CI PR evidence reuse tests passed (valid squash and fail-closed cases).'
