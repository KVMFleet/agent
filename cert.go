// Per-device mTLS cert lifecycle (Phase B M2).
//
// On each boot the agent calls `ensureCert`. Behaviour:
//
//   - If both `agent-key.pem` and `agent-cert.pem` exist under the
//     state dir AND the cert is not within
//     CertRenewalThresholdDays of expiry AND the cert's public key
//     matches the on-disk private key, do nothing.
//   - Otherwise, generate a fresh Ed25519 keypair, POST the public
//     key to `/v1/agent/cert/issue` with the existing bearer token,
//     persist the cert + key atomically (tmp-file + rename).
//
// Fail-soft: any error in the issuance path is LOGGED but does NOT
// abort agent startup. The agent worked yesterday without a cert
// (M1 wasn't shipped); it should keep working today while the cert
// layer is being rolled out. M3 will tighten this once mTLS-dial is
// in place.
//
// Fail-loud: a key-generation failure (which would mean the host's
// CSPRNG is broken) exits the process with status 2, because there
// is no honest way to continue past a broken CSPRNG.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// issuanceHTTPTimeout caps any individual issuance HTTP attempt.
// Three attempts × 10s + backoff is well within the M2 product-loop
// budget of 60s for the whole issuance loop.
const issuanceHTTPTimeout = 10 * time.Second

// CertRenewalThresholdDays — when the active cert has fewer days
// than this remaining we request a new one on the next boot. v1
// keeps it loose (matches the platform's default-30 setting).
const CertRenewalThresholdDays = 30

// AgentKeyFilename / AgentCertFilename live under the same dir as
// the state.json file. They share a 0700-ish ancestor in production
// because the state file already has 0600 perms — we mirror that.
const (
	AgentKeyFilename  = "agent-key.pem"
	AgentCertFilename = "agent-cert.pem"
)

// ensureCert is called at agent startup after state is loaded.
// Returns nil on success OR on a tolerated failure (logged + agent
// continues). The agent gates NO functionality on the cert in M2 —
// M3 will start using it for outbound mTLS.
func ensureCert(cfg config, st *state) {
	keyPath, certPath := certPaths(cfg)

	// Happy fast-path: existing valid cert + matching key.
	if ok, why := certIsUsable(keyPath, certPath); ok {
		log.Printf("agent cert ok: %s", why)
		return
	} else if why != "" {
		log.Printf("agent cert needs refresh: %s", why)
	}

	// Generate a fresh keypair. Failure here is unrecoverable —
	// crypto/rand should never fail on a sane host.
	pubPEM, privPEM, err := generateEd25519PEM()
	if err != nil {
		log.Printf("agent: key generation failed: %v — refusing to start without a working CSPRNG", err)
		os.Exit(2)
	}

	certPEM, err := requestCertWithRetry(cfg, st, pubPEM)
	if err != nil {
		log.Printf("agent: cert issuance failed (continuing without cert): %v", err)
		return
	}

	if err := persistKeyAndCert(keyPath, certPath, privPEM, certPEM); err != nil {
		log.Printf("agent: persist cert/key failed (continuing): %v", err)
		return
	}
	log.Printf("agent: cert issued + persisted under %s", filepath.Dir(keyPath))
}

// requestCertWithRetry wraps requestCertFromPlatform with a tiny
// retry loop. We back off 1s then 5s (total ~6s ceiling, well under
// the 60s M2 budget). The retry is INTENTIONALLY narrow — we don't
// want a flaky network to block agent startup indefinitely.
func requestCertWithRetry(cfg config, st *state, pubPEM []byte) ([]byte, error) {
	delays := []time.Duration{0, 1 * time.Second, 5 * time.Second}
	var lastErr error
	for i, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}
		cert, err := requestCertFromPlatform(cfg, st, pubPEM)
		if err == nil {
			return cert, nil
		}
		lastErr = err
		log.Printf("agent: cert issuance attempt %d/%d failed: %v", i+1, len(delays), err)
	}
	return nil, lastErr
}

func certPaths(cfg config) (keyPath, certPath string) {
	dir := filepath.Dir(cfg.StatePath)
	return filepath.Join(dir, AgentKeyFilename), filepath.Join(dir, AgentCertFilename)
}

// certIsUsable reports whether the on-disk cert + key form a valid,
// non-near-expiry pair. The (ok, reason) return is logged so an
// operator can see WHY a refresh was triggered.
func certIsUsable(keyPath, certPath string) (ok bool, reason string) {
	keyBytes, keyErr := os.ReadFile(keyPath)
	certBytes, certErr := os.ReadFile(certPath)
	if os.IsNotExist(keyErr) || os.IsNotExist(certErr) {
		return false, "no on-disk cert+key"
	}
	if keyErr != nil {
		return false, fmt.Sprintf("read key: %v", keyErr)
	}
	if certErr != nil {
		return false, fmt.Sprintf("read cert: %v", certErr)
	}

	priv, err := parsePEMPrivateKey(keyBytes)
	if err != nil {
		return false, fmt.Sprintf("parse key: %v", err)
	}
	cert, err := parsePEMCert(certBytes)
	if err != nil {
		return false, fmt.Sprintf("parse cert: %v", err)
	}

	// Pubkey-match check: catches the "cert and key drifted" case
	// where a previous renewal wrote one but not the other.
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return false, "cert has non-Ed25519 public key"
	}
	if !bytes.Equal(pub, priv.Public().(ed25519.PublicKey)) {
		return false, "cert pubkey does not match on-disk private key"
	}

	remaining := time.Until(cert.NotAfter)
	if remaining <= 0 {
		return false, "cert expired"
	}
	if remaining < CertRenewalThresholdDays*24*time.Hour {
		return false, fmt.Sprintf("cert in renewal window (%s remaining)", remaining.Round(time.Hour))
	}
	return true, fmt.Sprintf("valid until %s", cert.NotAfter.UTC().Format(time.RFC3339))
}

// generateEd25519PEM produces a fresh keypair and PEM-encodes both
// halves. PKCS#8 for the private key (matches the platform's
// expectation); SPKI for the public key.
func generateEd25519PEM() (pubPEM, privPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ed25519 keygen: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal pkcs8: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal spki: %w", err)
	}
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})
	privPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	return pubPEM, privPEM, nil
}

// requestCertFromPlatform POSTs the public key PEM to the platform's
// /v1/agent/cert/issue endpoint, authenticated by the device's
// existing bearer auth-token. Returns the cert PEM verbatim on success.
func requestCertFromPlatform(cfg config, st *state, pubPEM []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), issuanceHTTPTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"public_key_pem": string(pubPEM)})
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.APIBase+"/v1/agent/cert/issue", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+st.AuthToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post issue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("issue HTTP %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if out.CertPEM == "" {
		return nil, fmt.Errorf("empty cert_pem in response")
	}
	return []byte(out.CertPEM), nil
}

// persistKeyAndCert writes both files atomically (tmp + rename) so a
// crash mid-write leaves either the OLD pair intact or no files
// (next boot regenerates). The KEY gets 0600, the CERT gets 0644
// (cert is public-by-design).
//
// Order: write key first, then cert. If we crash after key but
// before cert, certIsUsable() rejects on the next boot (cert missing)
// and we regenerate everything. The orphan key is overwritten.
func persistKeyAndCert(keyPath, certPath string, privPEM, certPEM []byte) error {
	if err := atomicWrite(keyPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	if err := atomicWrite(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		// If we didn't successfully rename, clean up the orphan tmp.
		if !renamed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	renamed = true
	return nil
}

func parsePEMPrivateKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an Ed25519 private key")
	}
	return priv, nil
}

func parsePEMCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}
