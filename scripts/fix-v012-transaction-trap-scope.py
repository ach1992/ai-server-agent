from pathlib import Path

path = Path("manage.sh")
text = path.read_text()
old = '''  local old_host old_zone old_dns old_dns_owned old_origin_ruleset old_origin_rule old_ssl_ruleset old_ssl_rule old_cert old_port previous_zone previous_cert
  local host port zone_pair zone_id zone_name ip stage backup cert_id dns_id dns_owned dns_action dns_old_content dns_old_proxied dns_old_ttl
  local origin_ruleset_id origin_rule_id origin_action ssl_ruleset_id ssl_rule_id ssl_action config_backup managed_backup managed_existed=0
  local owned_dns_id="" owned_origin_ruleset="" owned_origin_rule="" owned_ssl_ruleset="" owned_ssl_rule="" local_mutation_started=0 managed_mutation_started=0 transaction_committed=0'''
new = '''  old_host=""; old_zone=""; old_dns=""; old_dns_owned=""; old_origin_ruleset=""; old_origin_rule=""; old_ssl_ruleset=""; old_ssl_rule=""; old_cert=""; old_port=""; previous_zone=""; previous_cert=""
  host=""; port=""; zone_pair=""; zone_id=""; zone_name=""; ip=""; stage=""; backup=""; cert_id=""; dns_id=""; dns_owned=false; dns_action=""; dns_old_content=""; dns_old_proxied=""; dns_old_ttl=""
  origin_ruleset_id=""; origin_rule_id=""; origin_action=""; ssl_ruleset_id=""; ssl_rule_id=""; ssl_action=""; config_backup=""; managed_backup=""; managed_existed=0
  owned_dns_id=""; owned_origin_ruleset=""; owned_origin_rule=""; owned_ssl_ruleset=""; owned_ssl_rule=""; local_mutation_started=0; managed_mutation_started=0; transaction_committed=0'''
if text.count(old) != 1:
    raise SystemExit(f"expected one transaction scope marker, got {text.count(old)}")
path.write_text(text.replace(old, new, 1))
print("transaction trap scope fixed")
