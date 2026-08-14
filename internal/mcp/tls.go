package mcp

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/manifest"
)

func ServeTLS(cfg config.Config) error {
	if !cfg.TLSConfigured() {
		return fmt.Errorf("native TLS requires both tls_cert_file and tls_key_file")
	}
	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return err
	}
	return serveTLS(cfg, ln)
}

func serveTLS(cfg config.Config, ln net.Listener) error {
	defer ln.Close()
	s, err := New(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0750); err != nil {
		return err
	}
	if err := manifest.Write(filepath.Join(cfg.StateDir, "AI_ENVIRONMENT.json"), manifest.Build(cfg)); err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	fmt.Printf("ai-server-agent listening with TLS on %s%s\n", ln.Addr().String(), cfg.MCPPath)
	return httpServer.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
}
