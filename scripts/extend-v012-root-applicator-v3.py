from pathlib import Path

p = Path('scripts/apply-v012-root-trust-boundary.py')
text = p.read_text()
marker = "# ROOT_TRUST_EXTENSION_V3"
if marker in text:
    raise SystemExit('root trust extension v3 already present')
extension = r"""

# ROOT_TRUST_EXTENSION_V3
# Preserve only real legacy runtime directories and remove a legacy browser
# symlink after the root-owned runtime container has been established.
replace_once(
    "install.sh",
    'secure_state_container "$STATE_DIR/runtime"\nsecure_state_container "$STATE_DIR/jobs"\n# AI_ENVIRONMENT.json',
    '''secure_state_container "$STATE_DIR/runtime"
secure_state_container "$STATE_DIR/jobs"
if [ -L "$STATE_DIR/runtime/browser" ]; then
  warn "Removing unsafe legacy browser-data symlink without following it: $STATE_DIR/runtime/browser"
  rm -f -- "$STATE_DIR/runtime/browser"
fi
if [ -e "$STATE_DIR/runtime/browser" ] && [ ! -d "$STATE_DIR/runtime/browser" ]; then
  die "Browser runtime-data path is not a real directory: $STATE_DIR/runtime/browser"
fi
# Existing job outputs from the older worker-owned container are never trusted
# as symlinks after migration.
find "$STATE_DIR/jobs" -mindepth 1 -maxdepth 1 -type l -delete
# AI_ENVIRONMENT.json''',
)

# Root job-file reads use O_NOFOLLOW and validate regular-file ownership, so
# preserved legacy job entries cannot turn job_output/status into root file reads.
replace_once(
    "internal/executor/executor.go",
    '''func (s *Server) startJob(req Request) Response {
''',
    '''func (s *Server) openJobFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("job path is not a regular file: %s", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && st.Uid != s.workerUID) {
		f.Close()
		return nil, fmt.Errorf("job file has an unexpected owner: %s", path)
	}
	if fi.Mode().Perm()&0002 != 0 {
		f.Close()
		return nil, fmt.Errorf("job file is world-writable: %s", path)
	}
	return f, nil
}

func (s *Server) startJob(req Request) Response {
''',
)
replace_once(
    "internal/executor/executor.go",
    '''	if b, er := os.ReadFile(statusPath); er == nil {
		status := strings.TrimSpace(string(b))
		if status != "" {
			if _, parseErr := strconv.Atoi(status); parseErr == nil {
				return Response{OK: true, Status: "completed", Output: status}
			}
		}
	}
''',
    '''	if f, er := s.openJobFile(statusPath); er == nil {
		b, readErr := io.ReadAll(io.LimitReader(f, 64))
		_ = f.Close()
		if readErr != nil {
			return Response{Error: readErr.Error()}
		}
		status := strings.TrimSpace(string(b))
		if status != "" {
			if _, parseErr := strconv.Atoi(status); parseErr == nil {
				return Response{OK: true, Status: "completed", Output: status}
			}
			return Response{Error: "invalid job status file"}
		}
	} else if !os.IsNotExist(er) {
		return Response{Error: er.Error()}
	}
''',
)
replace_once(
    "internal/executor/executor.go",
    'f, er := os.Open(path)\n',
    'f, er := s.openJobFile(path)\n',
)

with Path("internal/executor/executor_test.go").open("a") as f:
    f.write('''

func TestOpenJobFileRejectsLegacySymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("root-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "123.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s := &Server{workerUID: uint32(os.Geteuid())}
	if f, err := s.openJobFile(link); err == nil {
		_ = f.Close()
		t.Fatal("openJobFile followed a legacy symlink")
	}
}
''')

# Browser setup also fails closed if an unexpected symlink survived migration.
replace_once(
    "internal/browser/browser.go",
    '''install -d -m 0755 -o root -g root "$engine"
install -d -m 0750 -o "$worker" -g "$worker" "$data"
''',
    '''install -d -m 0755 -o root -g root "$engine"
[ ! -L "$data" ] || { echo "Refusing symlinked browser data directory: $data" >&2; exit 2; }
[ ! -e "$data" ] || [ -d "$data" ] || { echo "Browser data path is not a directory: $data" >&2; exit 2; }
install -d -m 0750 -o "$worker" -g "$worker" "$data"
''',
)

# Make the migration test exercise both a container symlink and a nested
# browser-data symlink left from the former worker-owned runtime directory.
replace_once(
    "tests/root_trust_migration.sh",
    '''install -d -m 0755 -o root -g root "$scratch/runtime-target" "$scratch/jobs-target"
printf 'manifest-sentinel\\n' > "$scratch/manifest-target"
printf 'runtime-sentinel\\n' > "$scratch/runtime-target/sentinel"
printf 'jobs-sentinel\\n' > "$scratch/jobs-target/sentinel"
ln -s "$scratch/runtime-target" "$STATE_DIR/runtime"
ln -s "$scratch/jobs-target" "$STATE_DIR/jobs"
''',
    '''install -d -m 0750 -o aiworker -g aiworker "$STATE_DIR/runtime"
install -d -m 0755 -o root -g root "$scratch/browser-target" "$scratch/jobs-target"
printf 'manifest-sentinel\\n' > "$scratch/manifest-target"
printf 'browser-sentinel\\n' > "$scratch/browser-target/sentinel"
printf 'jobs-sentinel\\n' > "$scratch/jobs-target/sentinel"
ln -s "$scratch/browser-target" "$STATE_DIR/runtime/browser"
ln -s "$scratch/jobs-target" "$STATE_DIR/jobs"
''',
)
replace_once(
    "tests/root_trust_migration.sh",
    '''test ! -L "$STATE_DIR/runtime"
test ! -L "$STATE_DIR/jobs"
test ! -L "$STATE_DIR/AI_ENVIRONMENT.json"
test -d "$STATE_DIR/runtime" && test -d "$STATE_DIR/jobs"
''',
    '''test ! -L "$STATE_DIR/runtime"
test ! -L "$STATE_DIR/jobs"
test ! -L "$STATE_DIR/AI_ENVIRONMENT.json"
test -d "$STATE_DIR/runtime" && test -d "$STATE_DIR/jobs"
test ! -e "$STATE_DIR/runtime/browser"
''',
)
replace_once(
    "tests/root_trust_migration.sh",
    '''test "$(stat -c '%a' "$scratch/runtime-target")" = 755
test "$(stat -c '%a' "$scratch/jobs-target")" = 755
grep -qF runtime-sentinel "$scratch/runtime-target/sentinel"
grep -qF jobs-sentinel "$scratch/jobs-target/sentinel"
''',
    '''test "$(stat -c '%a' "$scratch/browser-target")" = 755
test "$(stat -c '%a' "$scratch/jobs-target")" = 755
grep -qF browser-sentinel "$scratch/browser-target/sentinel"
grep -qF jobs-sentinel "$scratch/jobs-target/sentinel"
''',
)

# Keep the documented invariant explicit.
replace_once(
    "docs/ARCHITECTURE.md",
    'writable worker/browser files exist only beneath root-controlled directory entries.\n',
    'writable worker/browser files exist only beneath root-controlled directory entries. Privileged job-file reads reject symlinks/non-regular files, and legacy browser-data symlinks are removed without being followed during upgrade.\n',
)
"""
p.write_text(text + extension)
print('root trust applicator extended for legacy job/browser child symlinks')
