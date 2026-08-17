from pathlib import Path


def replace_once(path, old, new):
    p = Path(path)
    text = p.read_text()
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{path}: expected one match, got {count}")
    p.write_text(text.replace(old, new, 1))

replace_once(
    "internal/executor/executor.go",
    '''\thome := s.cfg.WorkspaceDir
\tif req.Root {
\t\thome = "/root"
\t}
\tcmd := newShellCommand(req.Command, home, s.cfg.WorkspaceDir)
''',
    '''\thome := s.cfg.WorkspaceDir
\tdir := s.cfg.WorkspaceDir
\tif req.Root {
\t\thome = "/root"
\t\tdir = "/root"
\t}
\tcmd := newShellCommand(req.Command, home, dir)
''')

replace_once(
    "internal/executor/executor.go",
    '''\thome := s.cfg.WorkspaceDir
\targs := []string{"--unit", unit, "--collect", "--property=WorkingDirectory=" + s.cfg.WorkspaceDir}
\tif req.Root {
\t\thome = "/root"
\t} else {
\t\targs = append(args, "--uid="+s.cfg.WorkerUser)
\t}
''',
    '''\thome := s.cfg.WorkspaceDir
\tworkDir := s.cfg.WorkspaceDir
\tif req.Root {
\t\thome = "/root"
\t\tworkDir = "/root"
\t}
\targs := []string{"--unit", unit, "--collect", "--property=WorkingDirectory=" + workDir}
\tif !req.Root {
\t\targs = append(args, "--uid="+s.cfg.WorkerUser)
\t}
''')

replace_once(
    "internal/executor/executor_test.go",
    '''\tif strings.Contains(env, "HOME="+workspace) {
\t\tt.Fatalf("root command inherited worker HOME: %q", env)
\t}
}
''',
    '''\tif strings.Contains(env, "HOME="+workspace) {
\t\tt.Fatalf("root command inherited worker HOME: %q", env)
\t}
\tif rootCmd.Dir != "/root" {
\t\tt.Fatalf("root command working directory = %q, want /root", rootCmd.Dir)
\t}
\tworkerCmd, _ := s.command(Request{Command: "printf safe", Root: false})
\tif workerCmd.Dir != workspace {
\t\tt.Fatalf("worker command working directory = %q, want workspace", workerCmd.Dir)
\t}
}
''')

replace_once(
    "internal/executor/executor_test.go",
    '''\tt.Setenv("BASH_ENV", bashEnv)
\tt.Setenv("ENV", bashEnv)

\ts := &Server{
''',
    '''\tt.Setenv("BASH_ENV", bashEnv)
\tt.Setenv("ENV", bashEnv)
\tambientName := ".ai-worker-ambient-" + filepath.Base(workspace)
\tif err := os.WriteFile(filepath.Join(workspace, ambientName), []byte("worker-controlled"), 0644); err != nil {
\t\tt.Fatal(err)
\t}
\tbenignRootCommand := "test ! -e " + shellQuote(ambientName) + " && printf safe"

\ts := &Server{
''')

replace_once(
    "internal/executor/executor_test.go",
    '''\tresp := s.run(Request{Command: "printf safe", Root: true, Approval: true})
''',
    '''\tresp := s.run(Request{Command: benignRootCommand, Root: true, Approval: true})
''')

replace_once(
    "internal/executor/executor_test.go",
    '''\tfakeScript := `#!/bin/sh
set -e
while [ "$#" -gt 0 ]; do
  case "$1" in
    --unit) shift 2 ;;
    --collect|--property=*|--uid=*) shift ;;
    /usr/bin/env) exec "$@" ;;
    *) echo "unexpected systemd-run test arg: $1" >&2; exit 64 ;;
  esac
done
exit 65
`
''',
    '''\tfakeScript := `#!/bin/sh
set -e
workdir=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --unit) shift 2 ;;
    --property=WorkingDirectory=*) workdir="${1#--property=WorkingDirectory=}"; shift ;;
    --collect|--uid=*) shift ;;
    /usr/bin/env) [ -n "$workdir" ] && cd "$workdir"; exec "$@" ;;
    *) echo "unexpected systemd-run test arg: $1" >&2; exit 64 ;;
  esac
done
exit 65
`
''')

replace_once(
    "internal/executor/executor_test.go",
    '''\tjob := s.startJob(Request{Command: "printf safe", Root: true, Approval: true})
''',
    '''\tjob := s.startJob(Request{Command: benignRootCommand, Root: true, Approval: true})
''')

replace_once(
    "internal/executor/executor_test.go",
    '''\tfor _, want := range []string{"/usr/bin/env", `"-i"`, `"HOME="+home`, `"/bin/bash", "--noprofile", "--norc", "-c"`} {
''',
    '''\tfor _, want := range []string{"/usr/bin/env", `"-i"`, `"HOME="+home`, `workDir = "/root"`, `"--property=WorkingDirectory=" + workDir`, `"/bin/bash", "--noprofile", "--norc", "-c"`} {
''')

print("root working-directory hardening applied")
