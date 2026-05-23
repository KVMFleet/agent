// Phase B M4: agent-side mTLS dial + periodic cert renewal.
//
// Closes the M1+M2+M3 loop: M1 issued the cert, M2 wrote it to
// disk, M3 verified it server-side. M4 makes the agent actually
// USE the cert by:
//
//   1. Building an *http.Client that presents the cert+key during
//      TLS handshake (mtlsClient)
//   2. Running a background loop that periodically calls
//      /v1/agent/mtls/heartbeat as a self-test — every minute by
//      default — so prod operators can see the mTLS layer working
//      in real time via audit events
//   3. Re-running ensureCert every 24h so a long-running agent
//      that has been up for ≥ 60 days self-renews before its 90-day
//      cert expires
//
// Fail-soft throughout: a missing cert means the heartbeat loop
// logs and continues (the agent's primary work via bearer-token WS
// continues unaffected). M5+ will start gating real functionality
// on mTLS, at which point fail-soft will tighten.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// MtlsHeartbeatInterval — how often the agent calls the mTLS
// heartbeat endpoint. Once per minute is a sane default: enough
// to surface failures quickly without flooding audit logs.
// Configurable via KVMFLEET_MTLS_HEARTBEAT_SECONDS; set to 0 to
// disable the loop entirely (still runs ensureCert at startup).
const DefaultMtlsHeartbeatInterval = 60 * time.Second

// MtlsCertRefreshInterval — how often the loop re-checks the
// on-disk cert for near-expiry. 1h is enough — ensureCert is
// cheap when the cert is already valid (just a file read +
// parse), and a tighter interval means an agent whose boot-time
// issuance failed recovers within an hour rather than a day.
const MtlsCertRefreshInterval = 1 * time.Hour

// mtlsHTTPTimeout caps any single heartbeat round-trip. Heartbeats
// that take longer than this are not worth waiting on — the next
// tick will retry.
const mtlsHTTPTimeout = 10 * time.Second

// mtlsClient returns an *http.Client preconfigured to present the
// agent's on-disk cert+key during TLS handshake. Returns nil + an
// error if the files can't be loaded — caller decides what to do.
//
// The platform's TLS root is the system default (Let's Encrypt is
// chained from public CAs already trusted by the OS). Phase B v1
// does NOT pin the platform's leaf cert — that's a future
// hardening item.
func mtlsClient(keyPath, certPath string) (*http.Client, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read cert %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", keyPath, err)
	}
	// Sanity: confirm the cert PEM is a single CERTIFICATE block;
	// guards against an accidentally truncated file producing a
	// confusing TLS error later.
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("cert file %s is not a PEM CERTIFICATE", certPath)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("cert file %s does not parse as x509: %w", certPath, err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("X509KeyPair: %w", err)
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
		},
		// Defaults are otherwise fine; we don't share this transport
		// with bearer-token requests so any connection-pool weirdness
		// is contained.
	}
	return &http.Client{Transport: tr, Timeout: mtlsHTTPTimeout}, nil
}

// startMtlsLoop runs the periodic heartbeat + renewal loop.
// Returns immediately; the goroutine exits when ctx is cancelled.
//
// Disabling: pass interval=0 (the caller does this when the env
// var KVMFLEET_MTLS_HEARTBEAT_SECONDS is 0).
func startMtlsLoop(ctx context.Context, cfg config, st *state, interval time.Duration) {
	if interval <= 0 {
		log.Printf("agent: mtls heartbeat loop disabled (interval=%v)", interval)
		return
	}
	go func() {
		hbTicker := time.NewTicker(interval)
		defer hbTicker.Stop()
		certTicker := time.NewTicker(MtlsCertRefreshInterval)
		defer certTicker.Stop()

		// Fire the first heartbeat eagerly so an operator sees the
		// outcome immediately on agent restart instead of waiting
		// for the first tick.
		doMtlsHeartbeat(ctx, cfg, st)

		for {
			select {
			case <-ctx.Done():
				return
			case <-hbTicker.C:
				doMtlsHeartbeat(ctx, cfg, st)
			case <-certTicker.C:
				ensureCert(cfg, st)
			}
		}
	}()
}

// doMtlsHeartbeat builds the mTLS client and POSTs to
// /v1/agent/mtls/heartbeat. On success, logs the platform's
// response. On failure, logs the error and returns — the next
// tick retries.
func doMtlsHeartbeat(ctx context.Context, cfg config, st *state) {
	keyPath, certPath := certPaths(cfg)
	client, err := mtlsClient(keyPath, certPath)
	if err != nil {
		// Opportunistic recovery: if the cert is missing/malformed,
		// try to issue one. This makes the heartbeat loop self-heal
		// within one tick rather than waiting for the periodic
		// cert-refresh — important for an agent whose boot-time
		// issuance failed because the platform was briefly down.
		log.Printf("agent: mtls heartbeat skipped (no cert): %v — attempting ensureCert", err)
		ensureCert(cfg, st)
		return
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.APIBase+"/v1/agent/mtls/heartbeat", nil)
	if err != nil {
		log.Printf("agent: mtls heartbeat build request: %v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("agent: mtls heartbeat failed: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		log.Printf("agent: mtls heartbeat HTTP %d: %s", resp.StatusCode, string(body))
		return
	}
	log.Printf("agent: mtls heartbeat ok (%d bytes)", len(body))
}
