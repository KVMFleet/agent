package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMtlsClient_LoadsValidPair(t *testing.T) {
	dir := t.TempDir()
	writeKeyPair(t, dir, 365*24*time.Hour)
	client, err := mtlsClient(
		filepath.Join(dir, AgentKeyFilename),
		filepath.Join(dir, AgentCertFilename),
	)
	if err != nil {
		t.Fatalf("mtlsClient: %v", err)
	}
	if client == nil || client.Transport == nil {
		t.Fatalf("expected a populated client + transport")
	}
	tr := client.Transport.(*http.Transport)
	if len(tr.TLSClientConfig.Certificates) != 1 {
		t.Errorf("expected exactly one cert in the TLSClientConfig")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion=TLS1.2")
	}
}

func TestMtlsClient_MissingCertFile(t *testing.T) {
	dir := t.TempDir()
	_, err := mtlsClient(
		filepath.Join(dir, "nope-key"),
		filepath.Join(dir, "nope-cert"),
	)
	if err == nil {
		t.Errorf("expected error for missing files")
	}
}

func TestMtlsClient_MalformedCertFile(t *testing.T) {
	dir := t.TempDir()
	// Real key, bogus cert.
	writeKeyPair(t, dir, 365*24*time.Hour)
	os.WriteFile(filepath.Join(dir, AgentCertFilename), []byte("not a pem"), 0o644)
	_, err := mtlsClient(
		filepath.Join(dir, AgentKeyFilename),
		filepath.Join(dir, AgentCertFilename),
	)
	if err == nil {
		t.Errorf("expected error for malformed cert")
	}
}

func TestStartMtlsLoop_ZeroIntervalIsNoop(t *testing.T) {
	// interval=0 must NOT start a goroutine; we just verify the
	// function returns without panicking and the loop never fires.
	cfg := config{}
	st := &state{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	startMtlsLoop(ctx, cfg, st, 0)
	<-ctx.Done()
}

func TestDoMtlsHeartbeat_PostsToPlatform(t *testing.T) {
	// Set up a fake platform that responds 200 to mtls heartbeat.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agent/mtls/heartbeat" {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("content-type", "application/json")
			w.Write([]byte(`{"device_id":"x","org_id":"y","authed_via":"mtls"}`))
			return
		}
		http.Error(w, "wrong path", 404)
	}))
	defer srv.Close()

	// Provide a cert+key so mtlsClient loads — the fake server
	// doesn't actually verify mTLS, it just records the hit.
	dir := t.TempDir()
	writeKeyPair(t, dir, 365*24*time.Hour)
	cfg := config{APIBase: srv.URL, StatePath: filepath.Join(dir, "state.json")}
	st := &state{}

	doMtlsHeartbeat(context.Background(), cfg, st)
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 hit, got %d", hits)
	}
}

func TestDoMtlsHeartbeat_NoCertSkipsGracefully(t *testing.T) {
	// No cert+key on disk → log + return, no crash.
	dir := t.TempDir()
	cfg := config{StatePath: filepath.Join(dir, "state.json")}
	st := &state{}
	// Should not panic.
	doMtlsHeartbeat(context.Background(), cfg, st)
}

func TestStartMtlsLoop_FiresFirstHeartbeatEagerly(t *testing.T) {
	// Set up a fake platform that COUNTS heartbeat hits.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeKeyPair(t, dir, 365*24*time.Hour)
	cfg := config{APIBase: srv.URL, StatePath: filepath.Join(dir, "state.json")}
	st := &state{}
	ctx, cancel := context.WithCancel(context.Background())

	startMtlsLoop(ctx, cfg, st, 1*time.Hour) // very long interval — only first heartbeat fires
	// Allow the eager-fire goroutine a moment.
	time.Sleep(200 * time.Millisecond)
	cancel()
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected exactly 1 eager heartbeat, got %d", hits)
	}
}
