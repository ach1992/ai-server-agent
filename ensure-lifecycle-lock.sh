#!/usr/bin/env bash
set -Eeuo pipefail

LOCK_DIR=/run/lock/ai-server-agent
LOCK_FILE="$LOCK_DIR/management.lock"

fail(){ printf 'ai-server-agent lifecycle lock setup: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "must run as root"

created_dir=0
if [ -e "$LOCK_DIR" ] || [ -L "$LOCK_DIR" ]; then
  [ -d "$LOCK_DIR" ] && [ ! -L "$LOCK_DIR" ] || fail "lifecycle lock directory is not a real directory: $LOCK_DIR"
else
  # Another root lifecycle process may win this create race. Never replace an
  # existing inode; validate whatever exists after the attempt instead.
  if ( umask 077; mkdir -- "$LOCK_DIR" ) 2>/dev/null; then
    created_dir=1
  fi
fi
if [ "$created_dir" -eq 1 ]; then
  chown root:root -- "$LOCK_DIR" || fail "could not set lifecycle lock directory ownership"
  chmod 0700 -- "$LOCK_DIR" || fail "could not set lifecycle lock directory mode"
fi
[ -d "$LOCK_DIR" ] && [ ! -L "$LOCK_DIR" ] || fail "lifecycle lock directory is not a real directory: $LOCK_DIR"
[ "$(stat -c '%u:%g:%a' "$LOCK_DIR" 2>/dev/null || true)" = "0:0:700" ] || fail "lifecycle lock directory ownership/mode is unsafe"

created_file=0
if [ ! -e "$LOCK_FILE" ] && [ ! -L "$LOCK_FILE" ]; then
  # noclobber makes concurrent creation safe: if another root lifecycle
  # process creates the lock first, this does not truncate or replace it.
  if ( set -C; umask 077; : > "$LOCK_FILE" ) 2>/dev/null; then
    created_file=1
  fi
fi
if [ "$created_file" -eq 1 ]; then
  chown root:root -- "$LOCK_FILE" || fail "could not set lifecycle lock ownership"
  chmod 0600 -- "$LOCK_FILE" || fail "could not set lifecycle lock mode"
fi
[ -f "$LOCK_FILE" ] && [ ! -L "$LOCK_FILE" ] || fail "lifecycle lock is not a regular file"
[ "$(stat -c '%u:%g:%a' "$LOCK_FILE" 2>/dev/null || true)" = "0:0:600" ] || fail "lifecycle lock ownership/mode is unsafe"
