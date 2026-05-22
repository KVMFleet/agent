package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Helper: write a (key, cert) pair to disk for tests that need them
// pre-existing.
func writeKeyPair(t *testing.T, dir string, validFor time.Duration) (ed25519.PrivateKey, *x509.Certificate) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(validFor),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsecert: %v", err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	if err := os.WriteFile(filepath.Join(dir, AgentKeyFilename), keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, AgentCertFilename), certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return priv, cert
}

func TestGenerateEd25519PEM_RoundTrip(t *testing.T) {
	pubPEM, privPEM, err := generateEd25519PEM()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if !bytes.Contains(pubPEM, []byte("PUBLIC KEY")) {
		t.Errorf("pubPEM missing header")
	}
	if !bytes.Contains(privPEM, []byte("PRIVATE KEY")) {
		t.Errorf("privPEM missing header")
	}
	// Round-trip parse.
	priv, err := parsePEMPrivateKey(privPEM)
	if err != nil {
		t.Fatalf("parse priv: %v", err)
	}
	block, _ := pem.Decode(pubPEM)
	parsedPub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	pub, ok := parsedPub.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("not ed25519 pubkey")
	}
	if !bytes.Equal(pub, priv.Public().(ed25519.PublicKey)) {
		t.Errorf("pubkey/privkey mismatch")
	}
}

func TestCertIsUsable_NoFiles(t *testing.T) {
	dir := t.TempDir()
	ok, reason := certIsUsable(filepath.Join(dir, "k"), filepath.Join(dir, "c"))
	if ok {
		t.Errorf("expected ok=false for missing files")
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

func TestCertIsUsable_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeKeyPair(t, dir, 365*24*time.Hour)
	ok, reason := certIsUsable(
		filepath.Join(dir, AgentKeyFilename),
		filepath.Join(dir, AgentCertFilename),
	)
	if !ok {
		t.Errorf("expected usable; reason=%s", reason)
	}
}

func TestCertIsUsable_NearExpiryTriggersRefresh(t *testing.T) {
	dir := t.TempDir()
	// Cert valid for 10 days — well under the 30-day renewal threshold.
	writeKeyPair(t, dir, 10*24*time.Hour)
	ok, reason := certIsUsable(
		filepath.Join(dir, AgentKeyFilename),
		filepath.Join(dir, AgentCertFilename),
	)
	if ok {
		t.Errorf("expected near-expiry to trigger refresh")
	}
	if reason == "" {
		t.Errorf("expected a reason string")
	}
}

func TestCertIsUsable_ExpiredCert(t *testing.T) {
	dir := t.TempDir()
	writeKeyPair(t, dir, -time.Hour)
	ok, _ := certIsUsable(
		filepath.Join(dir, AgentKeyFilename),
		filepath.Join(dir, AgentCertFilename),
	)
	if ok {
		t.Errorf("expected expired cert to be unusable")
	}
}

func TestCertIsUsable_DetectsKeyCertDrift(t *testing.T) {
	dir := t.TempDir()
	// Write a valid pair.
	writeKeyPair(t, dir, 365*24*time.Hour)
	// Overwrite the KEY with a fresh independent one; cert pubkey no
	// longer matches.
	_, freshPriv, _ := ed25519.GenerateKey(rand.Reader)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(freshPriv)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	os.WriteFile(filepath.Join(dir, AgentKeyFilename), pemBytes, 0o600)

	ok, reason := certIsUsable(
		filepath.Join(dir, AgentKeyFilename),
		filepath.Join(dir, AgentCertFilename),
	)
	if ok {
		t.Errorf("expected drift to be detected")
	}
	if reason == "" || reason == "cert expired" {
		t.Errorf("expected reason mentioning the drift, got: %s", reason)
	}
}

func TestAtomicWrite_LeavesNoTmpFileOnSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out")
	if err := atomicWrite(target, []byte("hello"), 0o600); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "hello" {
		t.Errorf("unexpected content: %s", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected exactly one file in dir, got %d", len(entries))
	}
}

func TestPersistKeyAndCert_BothWritten(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, AgentKeyFilename)
	certPath := filepath.Join(dir, AgentCertFilename)
	if err := persistKeyAndCert(keyPath, certPath, []byte("KEY"), []byte("CERT")); err != nil {
		t.Fatalf("persist: %v", err)
	}
	k, _ := os.ReadFile(keyPath)
	c, _ := os.ReadFile(certPath)
	if string(k) != "KEY" || string(c) != "CERT" {
		t.Errorf("contents mismatch: key=%s cert=%s", k, c)
	}
	// Key must be 0600.
	st, _ := os.Stat(keyPath)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key perm = %o, want 0600", st.Mode().Perm())
	}
}

func TestRequestCertFromPlatform_HappyPath(t *testing.T) {
	// Fake the platform endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/cert/issue" {
			http.Error(w, "wrong path", 404)
			return
		}
		if r.Header.Get("authorization") != "Bearer test-token" {
			http.Error(w, "missing auth", 401)
			return
		}
		var body struct {
			PublicKeyPEM string `json:"public_key_pem"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.PublicKeyPEM == "" {
			http.Error(w, "empty pubkey", 400)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"cert_pem":"FAKE-CERT-PEM"}`)
	}))
	defer srv.Close()

	cfg := config{APIBase: srv.URL}
	st := &state{AuthToken: "test-token"}
	cert, err := requestCertFromPlatform(cfg, st, []byte("dummy pubkey pem"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if string(cert) != "FAKE-CERT-PEM" {
		t.Errorf("unexpected cert: %s", cert)
	}
}

func TestRequestCertFromPlatform_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	cfg := config{APIBase: srv.URL}
	st := &state{AuthToken: "t"}
	_, err := requestCertFromPlatform(cfg, st, []byte("p"))
	if err == nil {
		t.Errorf("expected an error on HTTP 500")
	}
}

func TestRequestCertFromPlatform_RejectsEmptyCertField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"cert_pem":""}`)
	}))
	defer srv.Close()
	cfg := config{APIBase: srv.URL}
	st := &state{AuthToken: "t"}
	_, err := requestCertFromPlatform(cfg, st, []byte("p"))
	if err == nil {
		t.Errorf("expected error on empty cert_pem")
	}
}
