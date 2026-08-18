#!/usr/bin/env python3
from pathlib import Path

path = Path("manage.sh")
text = path.read_text()
old = '''    result="$(jq -ce --arg phase "$phase" '.result | select(type=="object" and (.id|type)=="string" and (.id|length)>0 and .kind=="zone" and .phase==$phase and (.rules|type)=="array")' <<<"$res")" || return 2
'''
new = '''    result="$(jq -ce --arg phase "$phase" '
      .result
      | select(
          type=="object" and
          (.id|type)=="string" and (.id|length)>0 and
          .kind=="zone" and
          .phase==$phase and
          (.rules|type)=="array" and
          all(.rules[];
            type=="object" and
            (.id|type)=="string" and (.id|length)>0 and
            ((has("ref")|not) or .ref==null or ((.ref|type)=="string" and (.ref|length)>0)) and
            ((has("description")|not) or .description==null or (.description|type)=="string")
          )
        )
    ' <<<"$res")" || return 2
'''
if text.count(old) != 1:
    raise SystemExit(f"expected one phase entrypoint validator, found {text.count(old)}")
text = text.replace(old, new, 1)
path.write_text(text)
