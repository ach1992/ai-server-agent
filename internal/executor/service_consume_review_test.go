package executor

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func readServiceTranscript(t *testing.T, s *Server, id string) []byte {
	t.Helper()
	var all []byte
	var offset int64
	for {
		part := s.jobOutputBounded(Request{JobID: id, Offset: offset, Limit: 4096})
		if !part.OK || part.NextOffset == nil || *part.NextOffset <= offset {
			t.Fatalf("bad continuation at %d: %+v", offset, part)
		}
		raw := []byte(part.Output)
		if part.OutputEncoding == "base64" {
			var err error
			raw, err = base64.StdEncoding.DecodeString(part.Output)
			if err != nil {
				t.Fatal(err)
			}
		}
		all = append(all, raw...)
		offset = *part.NextOffset
		if part.EOF != nil && *part.EOF {
			return all
		}
	}
}

func TestServiceBasicAuthorizationReflectionInlineAndRetained(t *testing.T) {
	const secret = "basic-stored-secret"
	const user = "operator"
	token := base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
	value := "Basic " + token
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != value {
			t.Error("Basic Authorization on wire changed")
		}
		if r.URL.Path == "/large" {
			_, _ = w.Write(bytes.Repeat([]byte("A"), 1100000))
		}
		_, _ = fmt.Fprintf(w, "header=%s;encoded=%s;secret=%s;tail", r.Header.Get("Authorization"), token, secret)
	}))
	defer srv.Close()
	s, ref := serviceFixture(t, srv.URL, "password", user, secret)
	for _, path := range []string{"/small", "/large"} {
		got := useService(s, ref, ServiceUseSpec{Path: path})
		if !got.OK {
			t.Fatalf("%s unauthorized: %s", path, got.ErrorCode)
		}
		expect := "header=[REDACTED];encoded=[REDACTED];secret=[REDACTED];tail"
		if path == "/small" {
			if got.JobID != "" || got.Output != expect {
				t.Fatalf("inline Basic leak: %q", got.Output)
			}
		} else {
			if got.JobID == "" {
				t.Fatal("large Basic response missing job_id")
			}
			all := readServiceTranscript(t, s, got.JobID)
			want := append(bytes.Repeat([]byte("A"), 1100000), []byte(expect)...)
			if !bytes.Equal(all, want) {
				t.Fatalf("retained response mismatched got=%d want=%d", len(all), len(want))
			}
		}
		if strings.Contains(got.Output, token) || strings.Contains(got.Output, secret) {
			t.Fatal("Basic secret visible")
		}
	}
}

func TestServiceBasicRedactionAcrossEveryWriteBoundary(t *testing.T) {
	const secret = "basic-streaming-secret"
	encoded := base64.StdEncoding.EncodeToString([]byte("user:" + secret))
	input := "auth Basic " + encoded + " token " + encoded + " raw " + secret + " done"
	var output bytes.Buffer
	filter := &credentialOutputRedactor{to: &output, secret: []byte(secret),
		additional: [][]byte{[]byte("Basic " + encoded), []byte(encoded)}}
	for i := 0; i < len(input); i++ {
		if _, err := filter.Write([]byte{input[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := filter.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "auth [REDACTED] token [REDACTED] raw [REDACTED] done"; got != want {
		t.Fatalf("split secret leaked: %q", got)
	}
}

func TestServiceNativeSQLModesNeverFeedPSQLMetaCommands(t *testing.T) {
	s, ref := serviceFixture(t, "postgresql://db.example/app", "password", "dboperator", "postgres-secret")
	// A fixture that would interpret local commands only from stdin. In the
	// corrected route, no query is sent to that interactive interpreter.
	s.servicePSQLBinary = nativeScript(t, "sql=''\nfor a in \"$@\"; do case \"$a\" in --command=*) sql=\"${a#*=}\" ;; esac; done\n[ -n \"$sql\" ]\nprintf 'sql:%s secret:%s\\n' \"$sql\" \"$PGPASSWORD\"")
	for _, query := range []string{"SELECT 1;", "CREATE TEMP TABLE a(id INT); INSERT INTO a VALUES(3); SELECT * FROM a;", "\\! id"} {
		got := useService(s, ref, ServiceUseSpec{Query: query})
		if !got.OK || !strings.Contains(got.Output, "sql:;\n"+query) || !strings.Contains(got.Output, "[REDACTED]") {
			t.Fatalf("PostgreSQL command not forced into -c SQL mode: %+v", got)
		}
		if strings.Contains(got.Output, "uid=") {
			t.Fatal("local shell command ran")
		}
	}
	m, mref := serviceFixture(t, "mariadb://db.example/app", "password", "dboperator", "mysql-secret")
	m.serviceMySQLBinary = nativeScript(t, "[ \"$2\" = '--binary-mode' ]\nfound=0\nfor a in \"$@\"; do if [ \"$a\" = '--skip-reconnect' ]; then found=1; fi; done\n[ \"$found\" = 1 ]\nIFS= read -r query\nprintf 'sql:%s\\n' \"$query\"")
	for _, query := range []string{"SELECT 1;", "INSERT INTO a VALUES(7);", "\\! id"} {
		got := useService(m, mref, ServiceUseSpec{Query: query})
		if !got.OK || got.Output != "sql:"+query+"\n" {
			t.Fatalf("MySQL noninteractive binary mode missing: %+v", got)
		}
	}
	noServiceTemps(t, m)
}
