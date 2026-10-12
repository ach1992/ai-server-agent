package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateEndpointRejectsUnsafeTransport(t *testing.T) {
	for _, test := range []struct {
		name, endpoint string
		allowed        bool
	}{
		{"tls", "https://agent.example/mcp", true},
		{"loopback-http", "http://127.0.0.1:41234/mcp", true},
		{"ipv6-loopback", "http://[::1]:41234/mcp", true},
		{"remote-http", "http://agent.example/mcp", false},
		{"credentials-in-url", "https://mytoken@agent.example/mcp", false},
		{"no-scheme", "agent.example/mcp", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateEndpoint(test.endpoint)
			if (err == nil) != test.allowed {
				t.Fatalf("validateEndpoint(%q) error=%v allowed=%t", test.endpoint, err, test.allowed)
			}
		})
	}
}

func TestBearerTransportDoesNotFollowRedirect(t *testing.T) {
	redirected := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("authorization header leaked to redirected destination")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer temp-only-token" {
			t.Errorf("initial authorized request lost bearer: %q", got)
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := &http.Client{
		Transport:     bearerTransport{base: http.DefaultTransport, token: "temp-only-token"},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	reply, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusTemporaryRedirect || redirected {
		t.Fatalf("unexpected redirect response status=%d followed=%t", reply.StatusCode, redirected)
	}
}
