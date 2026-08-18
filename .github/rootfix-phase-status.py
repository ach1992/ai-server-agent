from pathlib import Path

src = Path('.github/workflows/rootfix-phase-entrypoint-v2.yml').read_text().splitlines()
start = src.index('        run: |') + 1
body = []
for line in src[start:]:
    if line.startswith('          '):
        body.append(line[10:])
    elif not line.strip():
        body.append('')
    else:
        break
script = '\n'.join(body) + '\n'
old = '''  fi
  rc=$?
  [ "$rc" -eq 3 ] && return 3
  return 2
'''
new = '''  else
    rc=$?
    [ "$rc" -eq 3 ] && return 3
    return 2
  fi
'''
if script.count(old) != 1:
    raise SystemExit(f'unexpected phase helper status shape: {script.count(old)}')
Path('/tmp/rootfix-phase.sh').write_text(script.replace(old, new))
