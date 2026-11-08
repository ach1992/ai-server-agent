package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ach1992/ai-server-agent/internal/managedincus"
	"github.com/ach1992/ai-server-agent/internal/secretoutput"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Keep the internal test fixture surface while sharing one streaming
// implementation with native Incus and private root command execution.
type credentialOutputRedactor struct {
	mu         sync.Mutex
	to         io.Writer
	secret     []byte
	additional [][]byte
	impl       *secretoutput.Redactor
}

func (r *credentialOutputRedactor) inner() *secretoutput.Redactor {
	if r.impl == nil {
		r.impl = secretoutput.New(r.to, r.secret, r.additional...)
	}
	return r.impl
}
func (r *credentialOutputRedactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner().Write(p)
}
func (r *credentialOutputRedactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner().Flush()
}

func (s *Server) serviceUseAction(parent context.Context, req Request) Response {
	if req.Root || req.PrincipalID == "" || req.PrincipalClass == "" {
		return fileError("authentication_required", "authorization", errors.New("authenticated MCP principal required"))
	}
	if s.cfg.InstanceID == "" {
		return fileError("host_identity_unavailable", "state", errors.New("Agent instance identity required"))
	}
	if req.CredentialRef == "" {
		return fileError("credential_ref_required", "validation", errors.New("registered credential_ref required"))
	}
	st := serviceaccess.New(s.cfg.StateDir)
	rec, err := st.Resolve(req.CredentialRef, s.cfg.InstanceID)
	if err != nil {
		return managedErr(err)
	}
	if rec.Endpoint == "" || serviceaccess.ValidServiceTarget(rec.Endpoint) != nil {
		return fileError("service_target_unavailable", "validation", errors.New("registered network endpoint required"))
	}
	target, err := url.Parse(rec.Endpoint)
	if err != nil {
		return fileError("invalid_service_target", "validation", err)
	}
	if rec.Guest != "" {
		if rec.GuestUUID == "" || rec.Project == "" {
			return fileError("native_guest_identity_mismatch", "authorization", errors.New("guest identity incomplete"))
		}
		current, err := s.managedBackend().Run(parent, managedincus.Request{Operation: "inspect", Project: rec.Project, Instance: rec.Guest}, false, "")
		if err != nil || !nativeGuestIdentityMatches(current.Instance, rec.GuestUUID, rec.Project, rec.Guest, s.cfg.InstanceID) {
			return fileError("native_guest_identity_mismatch", "authorization", errors.New("registered guest UUID could not be verified"))
		}
	}
	in := req.ServiceUse
	// Respect the existing Executor IPC frame, not a new client/provider
	// payload allowlist. A small margin covers JSON metadata and escaping;
	// ClientCallContext also checks the serialized request before dialling.
	if len(in.Body) > maxExecutorRequestBytes-8192 || len(in.Query) > maxExecutorRequestBytes-8192 || len(in.Command) > maxCommandBytes {
		return fileError("input_too_large", "validation", errors.New("service operation exceeds the bounded Executor request budget"))
	}
	scheme := strings.ToLower(target.Scheme)
	started := time.Now()
	if blocked := s.beginActionAudit(req, "service_use", "root", "service_use "+scheme, "managed"); blocked != nil {
		return *blocked
	}
	ctx, cancel := context.WithTimeout(parent, requestRunTimeout(req))
	defer cancel()
	var resp Response
	// A payload credential is usable only in a JSON field of this authenticated
	// service's own registered origin. The requester sees neither secret.
	var bodySecret string
	if in.BodySecretRef != "" || in.BodySecretJSONPointer != "" {
		if in.BodySecretRef == "" || in.BodySecretJSONPointer == "" ||
			(scheme != "http" && scheme != "https") {
			return s.finishActionAudit(req, "service_use", "root", "service_use "+scheme, "managed", started,
				fileError("invalid_private_body_binding", "validation", errors.New("both JSON pointer and body_secret_ref are required for an HTTP service")))
		}
		bodyRec, secretErr := st.Resolve(in.BodySecretRef, s.cfg.InstanceID)
		if secretErr != nil {
			return s.finishActionAudit(req, "service_use", "root", "service_use "+scheme, "managed", started,
				fileError("private_body_credential_unavailable", "authorization", secretErr))
		}
		if bodyRec.HostID != rec.HostID || bodyRec.Project != rec.Project ||
			bodyRec.Guest != rec.Guest || bodyRec.GuestUUID != rec.GuestUUID ||
			!strings.EqualFold(bodyRec.Service, rec.Service) ||
			bodyRec.Endpoint == "" || serviceaccess.ValidEndpoint(bodyRec.Endpoint) != nil ||
			serviceaccess.Origin(bodyRec.Endpoint) != serviceaccess.Origin(rec.Endpoint) {
			return s.finishActionAudit(req, "service_use", "root", "service_use "+scheme, "managed", started,
				fileError("private_body_target_mismatch", "authorization",
					errors.New("private body credential must be registered for the exact same service origin and native guest")))
		}
		bodySecret = bodyRec.Secret
	}
	switch scheme {
	case "http", "https":
		release, ok := s.runs.acquire(false)
		if !ok {
			resp = runCapacityResponse(false)
		} else {
			resp = s.useHTTPService(ctx, rec, target, in, bodySecret)
			release()
		}
	case "ssh":
		resp = s.useSSHService(ctx, st, rec, target, in)
	case "postgres", "postgresql", "mysql", "mariadb":
		resp = s.useDatabaseService(ctx, st, rec, target, in)
	default:
		resp = fileError("service_protocol_unsupported", "validation", errors.New("supported consumers: HTTP(S), SSH, PostgreSQL and MySQL/MariaDB"))
	}
	if bodySecret != "" && (resp.OK || strings.HasPrefix(resp.Status, "http_")) {
		_ = st.MarkUsed(in.BodySecretRef)
	}
	resp.DurationMS = time.Since(started).Milliseconds()
	if resp.OK || strings.HasPrefix(resp.Status, "http_") || resp.Status == "native_completed" {
		_ = st.MarkUsed(rec.ID)
	}
	return s.finishActionAudit(req, "service_use", "root", "service_use "+scheme, "managed", started, resp)
}

func (s *Server) serviceResult(c *syncOutputCapture, r *credentialOutputRedactor, resp Response) Response {
	if err := r.Flush(); err != nil {
		resp.OK = false
		resp.Error = "private output filter failed: " + err.Error()
		resp.ErrorCode, resp.ReasonCode, resp.ErrorClass = "output_capture_unavailable", "output_capture_unavailable", "data"
	}
	applyOutputResult(&resp, c.preview.Result())
	if err := c.Finish(&resp, s.cfg.EffectiveRuntime().TextFallbackBytes); err != nil {
		resp.OK = false
		resp.Status = "output_incomplete"
		resp.ErrorCode, resp.ReasonCode, resp.ErrorClass = "output_capture_unavailable", "output_capture_unavailable", "data"
		if errors.Is(err, errSyncOutputLimit) {
			resp.ErrorCode, resp.ReasonCode = "output_limit_exceeded", "output_limit_exceeded"
		}
		resp.Error = "operation may have run, but its complete output is unrecoverable: " + err.Error()
	}
	return resp
}

func validServiceHeader(name string) bool {
	if len(name) == 0 || len(name) > 80 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	switch http.CanonicalHeaderKey(name) {
	case "Host", "Authorization", "Proxy-Authorization", "Connection", "Proxy-Connection", "Transfer-Encoding", "Content-Length":
		return false
	}
	return true
}

// bindPrivateJSONSecret assigns an existing JSON string value without putting
// the credential in MCP arguments. This is a field substitution, not an API
// account-creation adapter or a promise about what a remote service will do.
func bindPrivateJSONSecret(raw, pointer, secret string) (string, error) {
	if len(pointer) < 2 || len(pointer) > 512 || pointer[0] != '/' || secret == "" {
		return "", errors.New("non-empty JSON pointer and enrolled private secret required")
	}
	var document any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return "", fmt.Errorf("invalid JSON body: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", errors.New("JSON body must contain exactly one document")
	}
	keys := strings.Split(pointer[1:], "/")
	var current any = document
	for index, segment := range keys {
		for i := 0; i < len(segment); i++ {
			if segment[i] == '~' {
				if i+1 >= len(segment) || (segment[i+1] != '0' && segment[i+1] != '1') {
					return "", errors.New("invalid JSON pointer escape")
				}
				i++
			}
		}
		key := strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		last := index == len(keys)-1
		switch value := current.(type) {
		case map[string]any:
			child, found := value[key]
			if !found {
				return "", errors.New("JSON pointer must reference an existing field")
			}
			if last {
				if _, ok := child.(string); !ok {
					return "", errors.New("JSON secret destination must be a string")
				}
				value[key] = secret
			} else {
				current = child
			}
		case []any:
			pos, err := strconv.Atoi(key)
			if err != nil || pos < 0 || pos >= len(value) || strconv.Itoa(pos) != key {
				return "", errors.New("JSON pointer array index invalid")
			}
			if last {
				if _, ok := value[pos].(string); !ok {
					return "", errors.New("JSON secret destination must be a string")
				}
				value[pos] = secret
			} else {
				current = value[pos]
			}
		default:
			return "", errors.New("JSON pointer traverses a scalar value")
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (s *Server) useHTTPService(ctx context.Context, rec serviceaccess.Record, target *url.URL, in ServiceUseSpec, bodySecret string) Response {
	if err := serviceaccess.ValidEndpoint(rec.Endpoint); err != nil {
		return fileError("invalid_http_target", "validation", err)
	}
	if in.Command != "" || in.Query != "" {
		return fileError("invalid_service_operation", "validation", errors.New("HTTP uses method/path/body/headers, not command/query"))
	}
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = http.MethodGet
	}
	// Let net/http validate the standard method token; an arbitrary short
	// verb allowlist would wrongly prohibit authorized OPTIONS/PROPFIND APIs.
	if in.Path != "" {
		if !strings.HasPrefix(in.Path, "/") || strings.HasPrefix(in.Path, "//") || strings.ContainsAny(in.Path, "\r\n\x00#") {
			return fileError("invalid_http_path", "validation", errors.New("HTTP path must start with a single slash on the registered origin"))
		}
		relative, err := url.Parse(in.Path)
		if err != nil || relative.Host != "" || relative.Scheme != "" || relative.User != nil || relative.Fragment != "" {
			return fileError("invalid_http_path", "validation", errors.New("HTTP path cannot select another origin"))
		}
		target.Path, target.RawPath, target.RawQuery = relative.Path, relative.RawPath, relative.RawQuery
	}
	body := in.Body
	if bodySecret != "" {
		contentType := in.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		mediaType, _, mediaErr := mime.ParseMediaType(contentType)
		if mediaErr != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return fileError("private_body_requires_json", "validation", errors.New("private body injection requires an application/json compatible Content-Type"))
		}
		var bindErr error
		body, bindErr = bindPrivateJSONSecret(in.Body, in.BodySecretJSONPointer, bodySecret)
		if bindErr != nil {
			return fileError("invalid_private_body_json", "validation", bindErr)
		}
		if len(body) > maxExecutorRequestBytes-8192 {
			return fileError("private_body_too_large", "validation", errors.New("JSON body exceeds bounded private request size"))
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), strings.NewReader(body))
	if err != nil {
		return fileError("invalid_http_request", "validation", err)
	}
	for name, value := range in.Headers {
		if !validServiceHeader(name) || strings.ContainsAny(value, "\r\n\x00") {
			return fileError("invalid_http_header", "validation", errors.New("invalid or reserved HTTP header"))
		}
		request.Header.Set(name, value)
	}
	if in.ContentType != "" {
		if len(in.ContentType) > 120 || strings.ContainsAny(in.ContentType, "\r\n\x00") {
			return fileError("invalid_http_content_type", "validation", errors.New("invalid HTTP content type"))
		}
		request.Header.Set("Content-Type", in.ContentType)
	} else if in.Body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	mode := in.AuthMode
	if mode == "" {
		switch rec.Kind {
		case "password":
			mode = "basic"
		case "api_key":
			mode = "header"
		default:
			mode = "bearer"
		}
	}
	if in.AuthHeader != "" && mode != "header" {
		return fileError("invalid_service_auth", "validation", errors.New("auth_header requires auth_mode=header"))
	}
	switch mode {
	case "bearer":
		request.Header.Set("Authorization", "Bearer "+rec.Secret)
	case "header":
		header := in.AuthHeader
		if header == "" {
			header = "X-API-Key"
		}
		if !validServiceHeader(header) {
			return fileError("invalid_service_auth", "validation", errors.New("invalid API key header"))
		}
		request.Header.Set(header, rec.Secret)
	case "basic":
		if rec.Username == "" {
			return fileError("invalid_service_auth", "validation", errors.New("Basic auth username required"))
		}
		request.SetBasicAuth(rec.Username, rec.Secret)
	default:
		return fileError("invalid_service_auth", "validation", errors.New("auth_mode must be bearer, basic or header"))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // no ambient intermediary for private credentials
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result, err := client.Do(request)
	if err != nil {
		return fileError("service_http_failed", "network", fmt.Errorf("HTTP request failed: %w", err))
	}
	defer result.Body.Close()
	c := newSyncOutputCapture(s)
	filter := &credentialOutputRedactor{to: c, secret: []byte(rec.Secret)}
	if bodySecret != "" {
		filter.additional = append(filter.additional, []byte(bodySecret))
		if js, err := json.Marshal(bodySecret); err == nil && len(js) > 2 {
			filter.additional = append(filter.additional, js[1:len(js)-1])
		}
	}
	if mode == "basic" {
		// Exact reversible Basic header generated by request.SetBasicAuth.
		// Filter both the full header and its encoded value if reflected.
		encoded := base64.StdEncoding.EncodeToString([]byte(rec.Username + ":" + rec.Secret))
		filter.additional = append(filter.additional, []byte("Basic "+encoded), []byte(encoded))
	}
	_, readErr := io.Copy(filter, result.Body)
	resp := Response{OK: result.StatusCode >= 200 && result.StatusCode < 300 && readErr == nil, Status: fmt.Sprintf("http_%d", result.StatusCode)}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		resp.OK = false
		resp.ExitCode = 1
		resp.Error = fmt.Sprintf("HTTP %d from registered service (body retained; redirects are not automatically followed)", result.StatusCode)
		resp.ErrorCode, resp.ReasonCode, resp.ErrorClass = "service_http_status", "service_http_status", "http"
	}
	if readErr != nil {
		resp.OK = false
		resp.Error = "HTTP response read incomplete: " + readErr.Error()
		resp.ErrorCode, resp.ReasonCode, resp.ErrorClass = "service_http_read_failed", "service_http_read_failed", "network"
	}
	return s.serviceResult(c, filter, resp)
}

func validServicePort(target *url.URL, def string) (string, error) {
	port := target.Port()
	if port == "" {
		return def, nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("registered port must be 1..65535")
	}
	return strconv.Itoa(n), nil
}

// Only optional, known native client executables are launched. These private
// fixture overrides are never configurable by an MCP request.
func (s *Server) nativeServiceBinary(which string) (string, error) {
	var path string
	switch which {
	case "ssh":
		path = "/usr/bin/ssh"
		if s.serviceSSHBinary != "" {
			path = s.serviceSSHBinary
		}
	case "sshpass":
		path = "/usr/bin/sshpass"
		if s.serviceSSHPassBinary != "" {
			path = s.serviceSSHPassBinary
		}
	case "psql":
		path = "/usr/bin/psql"
		if s.servicePSQLBinary != "" {
			path = s.servicePSQLBinary
		}
	case "mysql":
		path = "/usr/bin/mysql"
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			path = "/usr/bin/mariadb"
		}
		if s.serviceMySQLBinary != "" {
			path = s.serviceMySQLBinary
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Mode()&0111 == 0 {
		return "", errors.New(which + " optional native executable unavailable")
	}
	return path, nil
}
func newNativeServiceCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = sanitizedCommandEnv("/root")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_, err := terminateProcessGroup(cmd.Process.Pid)
		return err
	}
	cmd.WaitDelay = processGroupTerminateGrace
	return cmd
}
func (s *Server) runNativeService(ctx context.Context, rec serviceaccess.Record, cmd *exec.Cmd) Response {
	release, ok := s.runs.acquire(true)
	if !ok {
		return runCapacityResponse(true)
	}
	defer release()
	c := newSyncOutputCapture(s)
	filter := &credentialOutputRedactor{to: c, secret: []byte(rec.Secret)}
	cmd.Stdout, cmd.Stderr = filter, filter
	err := cmd.Run()
	resp := Response{OK: err == nil, Status: "native_completed"}
	if err != nil {
		resp.OK = false
		resp.Error = "native service client failed: " + err.Error()
		resp.ErrorCode, resp.ReasonCode, resp.ErrorClass = "service_client_failed", "service_client_failed", "process"
		resp.ExitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			resp.ExitCode = exitErr.ExitCode()
		}
	}
	return s.serviceResult(c, filter, resp)
}

func privateServiceTempFile(st *serviceaccess.Store, prefix, content string) (string, func(), error) {
	dir := st.Path()
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0077 != 0 || fi.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("protected credential directory is unavailable")
	}
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := io.WriteString(f, content); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func (s *Server) useSSHService(ctx context.Context, st *serviceaccess.Store, rec serviceaccess.Record, target *url.URL, in ServiceUseSpec) Response {
	if in.Command == "" || in.Query != "" || in.Method != "" || in.Path != "" || in.Body != "" || len(in.Headers) != 0 {
		return fileError("invalid_service_operation", "validation", errors.New("SSH requires command and no HTTP/SQL arguments"))
	}
	if rec.Username == "" {
		return fileError("invalid_service_auth", "validation", errors.New("SSH username required"))
	}
	port, err := validServicePort(target, "22")
	if err != nil {
		return fileError("invalid_service_port", "validation", err)
	}
	path, err := s.nativeServiceBinary("ssh")
	if err != nil {
		return fileError("service_client_unavailable", "dependency", err)
	}
	if rec.Kind == "private_key" || rec.Kind == "ssh_private_key" {
		keyPath, cleanup, err := privateServiceTempFile(st, ".asa-ssh-key-", rec.Secret+"\n")
		if err != nil {
			return fileError("service_key_unavailable", "state", err)
		}
		defer cleanup()
		cmd := newNativeServiceCommand(ctx, path, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
			"-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=10", "-i", keyPath, "-p", port, "-l", rec.Username, "--", target.Hostname(), in.Command)
		return s.runNativeService(ctx, rec, cmd)
	}
	// Textual passwords, API tokens and other provider-issued login codes
	// can all be offered to a password-capable SSH service without forcing
	// an arbitrary stored kind taxonomy. sshpass is an optional native CLI.
	passBinary, err := s.nativeServiceBinary("sshpass")
	if err != nil {
		return fileError("service_client_unavailable", "dependency", err)
	}
	cmd := newNativeServiceCommand(ctx, passBinary, "-e", path,
		"-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10",
		"-o", "PreferredAuthentications=password,keyboard-interactive", "-o", "PubkeyAuthentication=no",
		"-p", port, "-l", rec.Username, "--", target.Hostname(), in.Command)
	cmd.Env = append(cmd.Env, "SSHPASS="+rec.Secret)
	return s.runNativeService(ctx, rec, cmd)
}

func mysqlEscape(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\r", "\\r", "\n", "\\n").Replace(v)
}
func (s *Server) useDatabaseService(ctx context.Context, st *serviceaccess.Store, rec serviceaccess.Record, target *url.URL, in ServiceUseSpec) Response {
	if in.Query == "" || in.Command != "" || in.Method != "" || in.Path != "" || in.Body != "" || in.AuthHeader != "" || len(in.Headers) != 0 {
		return fileError("invalid_service_operation", "validation", errors.New("database requires query, not HTTP/SSH arguments"))
	}
	if rec.Username == "" {
		return fileError("invalid_service_auth", "validation", errors.New("native database client requires enrolled credential and username"))
	}
	db := strings.TrimPrefix(target.Path, "/")
	if db == "" || strings.Contains(db, "/") {
		return fileError("invalid_service_database", "validation", errors.New("registered URI must include a single database name"))
	}
	scheme := strings.ToLower(target.Scheme)
	switch scheme {
	case "postgres", "postgresql":
		port, err := validServicePort(target, "5432")
		if err != nil {
			return fileError("invalid_service_port", "validation", err)
		}
		path, err := s.nativeServiceBinary("psql")
		if err != nil {
			return fileError("service_client_unavailable", "dependency", err)
		}
		host := target.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		dsn := "postgresql://" + host + ":" + port + "/" + url.PathEscape(db)
		// -c sends the entire string to PostgreSQL as SQL, rather than
		// interpreting interactive psql backslash commands from stdin.
		// A leading empty SQL statement prevents a request beginning with
		// \! or another backslash from being treated as a single psql
		// meta-command. SQL is an operator-supplied query, never the stored
		// credential; the latter stays in the private process environment.
		cmd := newNativeServiceCommand(ctx, path, "--no-psqlrc", "--no-password", "--set", "ON_ERROR_STOP=1", "--tuples-only", "--no-align", "--dbname="+dsn, "--command=;\n"+in.Query)
		cmd.Env = append(cmd.Env, "PGUSER="+rec.Username, "PGPASSWORD="+rec.Secret)
		return s.runNativeService(ctx, rec, cmd)
	case "mysql", "mariadb":
		port, err := validServicePort(target, "3306")
		if err != nil {
			return fileError("invalid_service_port", "validation", err)
		}
		path, err := s.nativeServiceBinary("mysql")
		if err != nil {
			return fileError("service_client_unavailable", "dependency", err)
		}
		opts := "[client]\nuser=\"" + mysqlEscape(rec.Username) + "\"\npassword=\"" + mysqlEscape(rec.Secret) + "\"\nhost=\"" + mysqlEscape(target.Hostname()) + "\"\nport=" + port + "\nprotocol=tcp\n"
		optionsPath, cleanup, err := privateServiceTempFile(st, ".asa-mysql-client-", opts)
		if err != nil {
			return fileError("service_client_config_unavailable", "state", err)
		}
		defer cleanup()
		// Noninteractive --binary-mode disables local mysql/mariadb commands
		// (including system/source) while allowing SQL on stdin.
		cmd := newNativeServiceCommand(ctx, path, "--defaults-extra-file="+optionsPath, "--binary-mode", "--skip-reconnect", "--batch", "--raw", "--skip-column-names", "--database="+db)
		cmd.Stdin = strings.NewReader(in.Query + "\n")
		return s.runNativeService(ctx, rec, cmd)
	}
	return fileError("service_protocol_unsupported", "validation", errors.New("unsupported database protocol"))
}
