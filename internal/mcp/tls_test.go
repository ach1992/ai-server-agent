package mcp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCertificate(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	certFile, err := os.OpenFile(certPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		certFile.Close()
		t.Fatal(err)
	}
	if err := certFile.Close(); err != nil {
		t.Fatal(err)
	}
	keyBytes := x509.MarshalPKCS1PrivateKey(key)
	keyFile, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes}); err != nil {
		keyFile.Close()
		t.Fatal(err)
	}
	if err := keyFile.Close(); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestNativeTLSServesHealthEndpoint(t *testing.T) {
	cfg := testConfig(t, "bearer")
	certPath, keyPath := writeTestCertificate(t, cfg.StateDir)
	cfg.TLSCertFile = certPath
	cfg.TLSKeyFile = keyPath

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveTLS(cfg, ln) }()

	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	defer transport.CloseIdleConnections()

	url := "https://" + ln.Addr().String() + cfg.HealthPath
	var resp *http.Response
	for i := 0; i < 20; i++ {
		resp, err = client.Get(url)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		ln.Close()
		t.Fatalf("TLS health request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ln.Close()
		t.Fatalf("got status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		ln.Close()
		t.Fatal("expected TLS 1.2 or newer")
	}

	_ = ln.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TLS server did not stop after listener close")
	}
}
