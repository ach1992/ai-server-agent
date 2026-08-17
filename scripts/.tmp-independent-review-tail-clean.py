# Clean tail for the temporary independent-review remediation applicator.

replace('tests/cloudflare_phase_recovery.sh',
'''      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"\n      host=mcp.example.com; zone_id=zone1; old_port=3210\n      dns_id=dns-new; dns_action=created; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""\n      origin_ruleset_id=origin-set; origin_rule_id=origin-rule; origin_action=created\n      ssl_ruleset_id=ssl-set; ssl_rule_id=ssl-rule; ssl_action=created; cert_id=cert-new''',
'''      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""\n      host=mcp.example.com; zone_id=zone1; old_port=3210\n      dns_id=dns-new; dns_action=created; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""; dns_fingerprint=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n      origin_ruleset_id=origin-set; origin_rule_id=origin-rule; origin_action=created; origin_fingerprint=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n      ssl_ruleset_id=ssl-set; ssl_rule_id=ssl-rule; ssl_action=created; ssl_fingerprint=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc; cert_id=cert-new''')
replace('tests/cloudflare_phase_recovery.sh',
'''          cf_set_commit_intent mcp.example.com 3210 zone1 example.com dns-new true origin-set origin-rule ssl-set ssl-rule cert-new''',
'''          cf_set_commit_intent mcp.example.com 3210 zone1 example.com dns-new true origin-set origin-rule ssl-set ssl-rule cert-new aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc''')
replace('tests/cloudflare_phase_recovery.sh',
'''    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_DELETE_LOG="$DELETE_LOG" \\\n''',
'''    TEST_CONFIG_FILE="$CONFIG_FILE" TEST_MANAGED_STATE="$MANAGED_STATE" TEST_CF_TXN_STATE="$CF_TXN_STATE" TEST_CF_TXN_BACKUP_DIR="$CF_TXN_BACKUP_DIR" TEST_MANAGEMENT_LOCK="$MANAGEMENT_LOCK" TEST_DELETE_LOG="$DELETE_LOG" \\\n''')
replace('tests/cloudflare_phase_recovery.sh',
'''      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"\n      systemctl(){ return 0; }\n      cf_delete_owned(){ printf "%s\\n" "$1" >> "$TEST_DELETE_LOG"; return 0; }''',
'''      STATE_DIR="$TEST_STATE_DIR"; CONFIG_DIR="$TEST_CONFIG_DIR"; CONTROL_DIR="$TEST_CONTROL_DIR"; TLS_DIR="$TEST_TLS_DIR"; CONFIG_FILE="$TEST_CONFIG_FILE"; MANAGED_STATE="$TEST_MANAGED_STATE"; CF_TXN_STATE="$TEST_CF_TXN_STATE"; CF_TXN_BACKUP_DIR="$TEST_CF_TXN_BACKUP_DIR"; MANAGEMENT_LOCK="$TEST_MANAGEMENT_LOCK"; MANAGEMENT_LOCK_FD=""\n      systemctl(){ return 0; }\n      cf_delete_dns_if_expected(){ printf "/zones/%s/dns_records/%s\\n" "$1" "$2" >> "$TEST_DELETE_LOG"; return 0; }\n      cf_delete_rule_if_expected(){ printf "/zones/%s/rulesets/%s/rules/%s\\n" "$1" "$2" "$3" >> "$TEST_DELETE_LOG"; return 0; }\n      cf_delete_owned(){ printf "%s\\n" "$1" >> "$TEST_DELETE_LOG"; return 0; }''')

p = Path('tests/cloudflare_phase_recovery.sh')
t = p.read_text()
marker = "\n# Truncated journal fails closed and remains byte-for-byte untouched.\n"
extra = r'''
# Committed recovery is terminal only when managed state proves the exact commit.
reset_old_local
kill_after_boundary committed
printf '%s\n' "$old_managed" > "$MANAGED_STATE"
chown root:root "$MANAGED_STATE"; chmod 0640 "$MANAGED_STATE"
before="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"; : > "$DELETE_LOG"
if recover_fresh; then echo 'committed journal finalized with mismatched managed state' >&2; exit 1; fi
test "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$before"
test -d "$CF_TXN_BACKUP_DIR"; test ! -s "$DELETE_LOG"

reset_old_local
kill_after_boundary committed
rm -f "$MANAGED_STATE"
before="$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')"; : > "$DELETE_LOG"
if recover_fresh; then echo 'committed journal finalized with missing managed state' >&2; exit 1; fi
test "$(sha256sum "$CF_TXN_STATE" | awk '{print $1}')" = "$before"
test -d "$CF_TXN_BACKUP_DIR"; test ! -s "$DELETE_LOG"
'''
if marker not in t:
    raise SystemExit('phase test marker missing')
p.write_text(t.replace(marker, '\n' + extra + marker, 1))

readme = Path('README.md').read_text()
readme = readme.replace('/var/lib/ai-server-agent/cloudflare-transaction.json', '/etc/ai-server-agent/control/cloudflare-transaction.json')
release_note = '- Stable release publication also requires an active tag ruleset covering `refs/tags/v*` (or all tags) with tag updates and deletions restricted and no bypass actors; the release workflow creates the exact tag only after verifying that protection.\n'
anchor = '- Stable releases rely on GitHub immutable release enforcement plus per-archive `SHA256SUMS` verification; neither stable install nor stable update accepts a local binary override.\n'
if release_note not in readme:
    if anchor not in readme:
        raise SystemExit('README stable-release security anchor missing')
    readme = readme.replace(anchor, anchor + release_note, 1)
Path('README.md').write_text(readme)

arch = Path('docs/ARCHITECTURE.md').read_text()
note = '''\n### Privileged management serialization and remote drift\n\nAll connection-mutating management paths share one root-only advisory lock under `/etc/ai-server-agent/control`. Cloudflare rollback and cleanup store exact Agent-written resource fingerprints and re-read the current remote representation immediately before destructive mutation; observed drift fails closed instead of overwriting or deleting concurrent state. Existing Agent-owned DNS, Origin Rules, and strict-SSL Configuration Rules are reused only when their recorded representation still matches the requested canonical state; otherwise reconfiguration stops for manual resolution.\n'''
if '### Privileged management serialization and remote drift' not in arch:
    arch += note
Path('docs/ARCHITECTURE.md').write_text(arch)

release = Path('.github/workflows/release.yml').read_text()
if 'concurrency:\n  group: stable-release' not in release:
    release = release.replace('permissions:\n  contents: write\n  actions: read\n', 'concurrency:\n  group: stable-release\n  cancel-in-progress: false\npermissions:\n  contents: write\n  actions: read\n', 1)
new_tag = '''      - name: Require an unpublished absent release tag\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: |\n          set -Eeuo pipefail\n          if gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then\n            echo "Release $RELEASE_TAG already exists; refusing to mutate an existing release." >&2\n            exit 1\n          fi\n          if gh api "/repos/$GITHUB_REPOSITORY/git/ref/tags/$RELEASE_TAG" >/dev/null 2>&1; then\n            echo "Tag $RELEASE_TAG already exists. Publication requires an absent tag so create-only tag creation can bind the exact validated SHA." >&2\n            exit 1\n          fi\n'''
release, n = re.subn(r'      - name: Require a new release tag\n.*?(?=      - name: Verify exact main validation and download artifact\n)', new_tag, release, count=1, flags=re.S)
if n != 1:
    raise SystemExit('release old tag gate missing or ambiguous')
new_publish = '''      - name: Verify active immutable release-tag ruleset\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: |\n          set -Eeuo pipefail\n          protected=0\n          while IFS= read -r ruleset_id; do\n            [ -n "$ruleset_id" ] || continue\n            ruleset="$(gh api "/repos/$GITHUB_REPOSITORY/rulesets/$ruleset_id")"\n            if jq -e --arg pattern 'refs/tags/v*' '\n              .target=="tag" and .enforcement=="active" and\n              ((.bypass_actors // []) | length)==0 and\n              ((((.conditions.ref_name.include // []) | index($pattern)) != null) or (((.conditions.ref_name.include // []) | index("~ALL")) != null)) and\n              (((.conditions.ref_name.exclude // []) | length)==0) and\n              (([.rules[]?.type] | index("update")) != null) and\n              (([.rules[]?.type] | index("deletion")) != null)\n            ' >/dev/null <<<"$ruleset"; then\n              protected=1\n              break\n            fi\n          done < <(gh api "/repos/$GITHUB_REPOSITORY/rulesets?targets=tag&includes_parents=true&per_page=100" --jq '.[] | select(.target == "tag" and .enforcement == "active") | .id')\n          test "$protected" -eq 1 || {\n            echo "Publication is blocked. Create an active tag ruleset covering refs/tags/v* (or all tags) with Restrict updates + Restrict deletions and no bypass actors." >&2\n            exit 1\n          }\n      - name: Recheck manual immutability gate before publication\n        run: |\n          set -Eeuo pipefail\n          test "$IMMUTABILITY_CONFIRMATION" = 'IMMUTABLE_RELEASES_ENABLED'\n          echo "Proceeding only on the operator's immediately preceding repository-setting verification."\n      - name: Create and verify exact protected release tag\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: |\n          set -Eeuo pipefail\n          gh api --method POST "/repos/$GITHUB_REPOSITORY/git/refs" \\\n            -f ref="refs/tags/$RELEASE_TAG" \\\n            -f sha="$RELEASE_SHA" >/tmp/release-tag-create.json\n          test "$(jq -r '.object.type' /tmp/release-tag-create.json)" = commit\n          test "$(jq -r '.object.sha' /tmp/release-tag-create.json)" = "$RELEASE_SHA"\n          remote_tag_json="$(gh api "/repos/$GITHUB_REPOSITORY/git/ref/tags/$RELEASE_TAG")"\n          test "$(jq -r '.object.type' <<<"$remote_tag_json")" = commit\n          test "$(jq -r '.object.sha' <<<"$remote_tag_json")" = "$RELEASE_SHA"\n      - name: Publish release payload\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: |\n          set -Eeuo pipefail\n          remote_tag_json="$(gh api "/repos/$GITHUB_REPOSITORY/git/ref/tags/$RELEASE_TAG")"\n          test "$(jq -r '.object.type' <<<"$remote_tag_json")" = commit\n          test "$(jq -r '.object.sha' <<<"$remote_tag_json")" = "$RELEASE_SHA"\n          version="${RELEASE_TAG#v}"\n          gh release create "$RELEASE_TAG" \\\n            "dist/ai-server-agent_${version}_linux_amd64.tar.gz" \\\n            dist/SHA256SUMS \\\n            dist/install.sh \\\n            --repo "$GITHUB_REPOSITORY" \\\n            --verify-tag \\\n            --generate-notes \\\n            --latest \\\n            --fail-on-no-commits\n'''
release, n = re.subn(r'      - name: Recheck manual immutability gate before publication\n.*?(?=      - name: Verify immutable release and attestations\n)', new_publish, release, count=1, flags=re.S)
if n != 1:
    raise SystemExit('release publish block missing or ambiguous')
if '--target "$RELEASE_SHA"' in release or '--verify-tag' not in release:
    raise SystemExit('release tag provenance remediation incomplete')
Path('scripts/.tmp-release.yml').write_text(release)

ci = Path('.github/workflows/ci.yml').read_text()
ci = ci.replace("          grep -qF 'restore_updated_dns_record(){' manage.sh\n", "          grep -qF 'cf_delete_dns_if_expected(){' manage.sh\n          grep -qF 'cf_delete_rule_if_expected(){' manage.sh\n")
ci = ci.replace("          grep -qF 'Recorded ownership state was preserved for a safe retry.' manage.sh\n", "          grep -qF 'resource drift was detected' manage.sh\n")
anchor = "          grep -qF 'CF_TXN_STATE=\"$CONTROL_DIR/cloudflare-transaction.json\"' manage.sh\n"
addition = "          grep -qF 'MANAGEMENT_LOCK=\"$CONTROL_DIR/management.lock\"' manage.sh\n          grep -qF 'flock -n \"$MANAGEMENT_LOCK_FD\"' manage.sh\n          grep -qF 'dns_record_fingerprint' manage.sh\n          grep -qF 'origin_rule_fingerprint' manage.sh\n          grep -qF 'ssl_config_rule_fingerprint' manage.sh\n"
if addition not in ci:
    if anchor not in ci:
        raise SystemExit('CI transaction anchor missing')
    ci = ci.replace(anchor, anchor + addition, 1)
Path('scripts/.tmp-ci.yml').write_text(ci)

security = Path('.github/workflows/security.yml').read_text()
replacement = r'''      - name: Release publication is manually gated and exact-tag protected before publish
        run: |
          set -Eeuo pipefail
          grep -qF 'workflow_dispatch:' .github/workflows/release.yml
          if grep -qF "tags: ['v*']" .github/workflows/release.yml; then
            echo 'release must not publish automatically from a tag push' >&2
            exit 1
          fi
          grep -qF 'immutability_confirmation:' .github/workflows/release.yml
          grep -qF 'IMMUTABLE_RELEASES_ENABLED' .github/workflows/release.yml
          grep -qF 'release_sha:' .github/workflows/release.yml
          grep -qF 'group: stable-release' .github/workflows/release.yml
          if grep -qF -- '--target "$RELEASE_SHA"' .github/workflows/release.yml; then
            echo 'release must not rely on --target for an existing tag' >&2
            exit 1
          fi
          grep -qF -- '--verify-tag' .github/workflows/release.yml
          grep -qF 'rulesets?targets=tag&includes_parents=true' .github/workflows/release.yml
          grep -qF "--arg pattern 'refs/tags/v*'" .github/workflows/release.yml
          grep -qF 'index("update")' .github/workflows/release.yml
          grep -qF 'index("deletion")' .github/workflows/release.yml
          grep -qF '((.bypass_actors // []) | length)==0' .github/workflows/release.yml
          grep -qF 'gh api --method POST "/repos/$GITHUB_REPOSITORY/git/refs"' .github/workflows/release.yml
          grep -qF -- '-f sha="$RELEASE_SHA"' .github/workflows/release.yml
          gate_line="$(grep -nF 'Pre-publication immutable release gate' .github/workflows/release.yml | cut -d: -f1)"
          ruleset_line="$(grep -nF 'Verify active immutable release-tag ruleset' .github/workflows/release.yml | cut -d: -f1)"
          recheck_line="$(grep -nF 'Recheck manual immutability gate before publication' .github/workflows/release.yml | cut -d: -f1)"
          tag_line="$(grep -nF 'Create and verify exact protected release tag' .github/workflows/release.yml | cut -d: -f1)"
          publish_line="$(grep -nF 'Publish release payload' .github/workflows/release.yml | cut -d: -f1)"
          verify_line="$(grep -nF 'Verify immutable release and attestations' .github/workflows/release.yml | cut -d: -f1)"
          test -n "$gate_line" && test -n "$ruleset_line" && test -n "$recheck_line" && test -n "$tag_line" && test -n "$publish_line" && test -n "$verify_line"
          test "$gate_line" -lt "$ruleset_line"
          test "$ruleset_line" -lt "$recheck_line"
          test "$recheck_line" -lt "$tag_line"
          test "$tag_line" -lt "$publish_line"
          test "$publish_line" -lt "$verify_line"
          grep -qF '.immutable == true' .github/workflows/release.yml
          grep -qF 'gh release verify "$RELEASE_TAG"' .github/workflows/release.yml
          grep -qF 'gh release verify-asset' .github/workflows/release.yml
'''
security, n = re.subn(r'      - name: Release publication is manually gated before publish\n        run: \|\n.*\Z', replacement, security, count=1, flags=re.S)
if n != 1:
    raise SystemExit('security release-contract step did not replace')
Path('scripts/.tmp-security.yml').write_text(security)
