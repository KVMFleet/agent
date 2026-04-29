// EuroKVM device agent.
//
// MVP responsibilities:
//   1. On first start, exchange an enrollment token for a per-device auth token.
//   2. Persist device_id + auth token to a state file.
//   3. Open a WebSocket to the platform and send periodic heartbeats with
//      synthetic-or-real health metrics.
//
// Real PiKVM integration (read /sys/class/thermal/thermal_zone0/temp, etc.)
// will replace the simulated metrics in V1.0. The control surface above is
// stable.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"nhooyr.io/websocket"
)

// agentMux is the HTTP mux used for *both* the standalone local HTTP server
// (startConsoleServer) and the HTTP-over-WS multiplex path. It must be set up
// before connect() starts so incoming http.request frames can be dispatched.
var (
	agentMux     *http.ServeMux
	agentMuxOnce sync.Once
)

func buildAgentMux(cfg config, st *state) *http.ServeMux {
	mux := http.NewServeMux()

	// /health always returns agent-level health, regardless of mode.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"hw_kind":%q,"device_id":%q,"mode":%q}`,
			st.Name, cfg.HWKind, st.DeviceID, consoleMode(cfg))
	})

	if cfg.KvmdURL != "" {
		// Real mode: reverse-proxy every request to the local kvmd web UI.
		// kvmd uses a self-signed cert, so we skip TLS verification.
		target, err := url.Parse(cfg.KvmdURL)
		if err != nil {
			log.Fatalf("bad --kvmd-url: %v", err)
		}
		kvmdBasicAuth := "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(cfg.KvmdUser+":"+cfg.KvmdPass),
		)
		proxy := &httputil.ReverseProxy{
			Director: func(r *http.Request) {
				r.URL.Scheme = target.Scheme
				r.URL.Host = target.Host
				r.Host = target.Host
				r.Header.Set("Authorization", kvmdBasicAuth)
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, fmt.Sprintf("kvmd unreachable: %v", err), http.StatusBadGateway)
			},
		}
		mux.Handle("/", proxy)
		log.Printf("console mode: kvmd reverse-proxy → %s", cfg.KvmdURL)
	} else {
		// Simulate mode: serve the fake BIOS HTML.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/html; charset=utf-8")
			fmt.Fprintf(w, fakeConsoleHTML, st.Name, cfg.HWKind, st.DeviceID, st.Name)
		})
		log.Printf("console mode: fake HTML (simulate)")
	}

	return mux
}

func consoleMode(cfg config) string {
	if cfg.KvmdURL != "" {
		return "kvmd-proxy"
	}
	return "simulate"
}

var version = "dev"

const (
	heartbeatInterval = 5 * time.Second
	reconnectMin      = 1 * time.Second
	reconnectMax      = 30 * time.Second
)

type config struct {
	APIBase     string
	StatePath   string
	TokenFile   string // path to a file containing the enrollment token (single-use)
	Name        string
	Tags        []string
	HWKind      string
	HWID        string
	Simulate    bool
	ConsoleAddr string // bind addr for standalone HTTP server (":8080")
	ConsoleHost string // hostname the platform would use for direct routing
	KvmdURL     string // upstream kvmd (e.g. "https://127.0.0.1/"); empty = fake HTML
	KvmdUser    string // kvmd Basic auth username (default: admin)
	KvmdPass    string // kvmd Basic auth password (default: admin)
}

type state struct {
	DeviceID  string `json:"device_id"`
	OrgID     string `json:"org_id"`
	AuthToken string `json:"auth_token"`
	Name      string `json:"name"`
}

func main() {
	cmd := "run"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	switch cmd {
	case "run":
		runAgent()
	case "version":
		fmt.Println("eurokvm-agent", version)
	default:
		log.Fatalf("unknown command: %s", cmd)
	}
}

func runAgent() {
	cfg := loadConfig()
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o755); err != nil {
		log.Fatalf("mkdir state dir: %v", err)
	}

	st, err := loadState(cfg.StatePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	if st == nil {
		st, err = enroll(cfg)
		if err != nil {
			log.Fatalf("enroll: %v", err)
		}
		if err := saveState(cfg.StatePath, st); err != nil {
			log.Fatalf("save state: %v", err)
		}
		log.Printf("enrolled as device_id=%s name=%s", st.DeviceID, st.Name)
	} else {
		log.Printf("resuming device_id=%s name=%s", st.DeviceID, st.Name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agentMuxOnce.Do(func() { agentMux = buildAgentMux(cfg, st) })

	go startConsoleServer(ctx, cfg, st)
	runLoop(ctx, cfg, st)
}

// startConsoleServer runs the local fake-PiKVM HTTP UI on cfg.ConsoleAddr.
// In production this won't run on a public interface — the real entry point
// for platform→agent HTTP traffic is the http.request frame handled over the
// outbound WS in connect(). This local server is kept only for native dev
// (curl http://localhost:8080/) and for the docker-compose direct-routing
// fallback; you can disable it by setting EUROKVM_CONSOLE_ADDR=off.
func startConsoleServer(ctx context.Context, cfg config, st *state) {
	if cfg.ConsoleAddr == "off" || cfg.ConsoleAddr == "" {
		return
	}
	srv := &http.Server{Addr: cfg.ConsoleAddr, Handler: agentMux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	log.Printf("fake-PiKVM console serving on %s", cfg.ConsoleAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("console server: %v", err)
	}
}

const fakeConsoleHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>%s — KVM</title>
<style>
  html,body{margin:0;background:#050913;color:#a7f3d0;font:14px ui-monospace,monospace;height:100%%}
  .bar{background:#0f172a;border-bottom:1px solid #1f2a44;padding:8px 14px;color:#e5e7eb;font-family:Inter,sans-serif;display:flex;gap:14px;align-items:center}
  .chip{font-size:11px;padding:2px 8px;border:1px solid #1f2a44;border-radius:9999px;color:#94a3b8}
  .scr{padding:24px;line-height:1.5;background:
       radial-gradient(ellipse at center,rgba(34,211,238,.05),transparent 60%%),
       repeating-linear-gradient(0deg,rgba(255,255,255,.02) 0,rgba(255,255,255,.02) 1px,transparent 1px,transparent 3px),
       #050913;min-height:calc(100%% - 38px)}
  .blink{animation:bl 1.1s steps(2,start) infinite}
  @keyframes bl{to{visibility:hidden}}
</style></head><body>
<div class="bar">
  <strong>%s</strong>
  <span class="chip">device_id %s</span>
  <span class="chip" style="margin-left:auto">simulated PiKVM web UI</span>
</div>
<div class="scr"><pre>
                          GNU GRUB  version 2.06

 +----------------------------------------------------------------------------+
 | *Ubuntu Server 24.04 LTS                                                   |
 |  Advanced options for Ubuntu Server 24.04 LTS                              |
 |  Memory test (memtest86+, serial console)                                  |
 |  System setup                                                              |
 +----------------------------------------------------------------------------+

      Use the up and down keys to select which entry is highlighted.
      Press enter to boot the selected OS.

   The highlighted entry will be executed automatically in 4s_<span class="blink">▌</span>

  -- This screen is rendered by the fake-PiKVM container --
  -- The platform reverse-proxies it through /v1/devices/%s/console/ --
</pre></div>
</body></html>
`

func loadConfig() config {
	api := flag.String("api", envOr("EUROKVM_API", "http://localhost:8000"), "platform base URL")
	statePath := flag.String("state", envOr("EUROKVM_STATE", "/var/lib/eurokvm/state.json"), "state file path")
	tokenFile := flag.String("token-file", os.Getenv("EUROKVM_TOKEN_FILE"), "file containing enrollment token (single use)")
	name := flag.String("name", os.Getenv("EUROKVM_DEVICE_NAME"), "suggested device name")
	tags := flag.String("tags", os.Getenv("EUROKVM_DEVICE_TAGS"), "comma-separated tags")
	hwKind := flag.String("hw-kind", envOr("EUROKVM_HW_KIND", "pikvm-v4"), "hardware kind")
	hwID := flag.String("hw-id", os.Getenv("EUROKVM_HW_ID"), "stable hardware id (defaults to hostname)")
	simulate := flag.Bool("simulate", true, "simulate metrics rather than read hardware")
	consoleAddr := flag.String("console-addr", envOr("EUROKVM_CONSOLE_ADDR", ":8080"), "embedded fake-PiKVM HTTP bind address")
	consoleHost := flag.String("console-host", envOr("EUROKVM_CONSOLE_HOST", ""), "hostname platform uses to reach us (defaults to hostname)")
	kvmdURL := flag.String("kvmd-url", envOr("EUROKVM_KVMD_URL", ""), "upstream kvmd URL (e.g. https://127.0.0.1/); empty = fake HTML mode")
	kvmdUser := flag.String("kvmd-user", envOr("EUROKVM_KVMD_USER", "admin"), "kvmd Basic auth username")
	kvmdPass := flag.String("kvmd-pass", envOr("EUROKVM_KVMD_PASS", "admin"), "kvmd Basic auth password")
	flag.Parse()

	id := *hwID
	if id == "" {
		h, _ := os.Hostname()
		id = h
		if id == "" {
			id = fmt.Sprintf("simhw-%d", rand.Int63())
		}
	}

	var tagList []string
	for _, t := range strings.Split(*tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tagList = append(tagList, t)
		}
	}

	host := *consoleHost
	if host == "" {
		host, _ = os.Hostname()
	}

	kurl := *kvmdURL
	if kurl == "" && !*simulate {
		// On a real PiKVM, kvmd listens on https://127.0.0.1/ with a self-signed cert.
		kurl = "https://127.0.0.1/"
	}

	return config{
		APIBase:     strings.TrimRight(*api, "/"),
		StatePath:   *statePath,
		TokenFile:   *tokenFile,
		Name:        *name,
		Tags:        tagList,
		HWKind:      *hwKind,
		HWID:        id,
		Simulate:    *simulate,
		ConsoleAddr: *consoleAddr,
		ConsoleHost: host,
		KvmdURL:     kurl,
		KvmdUser:    *kvmdUser,
		KvmdPass:    *kvmdPass,
	}
}

func consoleURL(cfg config) string {
	port := cfg.ConsoleAddr
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	}
	return fmt.Sprintf("http://%s:%s", cfg.ConsoleHost, port)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func loadState(path string) (*state, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveState(path string, s *state) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func enroll(cfg config) (*state, error) {
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("no enrollment token (set EUROKVM_TOKEN_FILE or --token-file)")
	}
	// poll for token file (seed script may not have run yet)
	var tokenBytes []byte
	deadline := time.Now().Add(120 * time.Second)
	for {
		b, err := os.ReadFile(cfg.TokenFile)
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			tokenBytes = bytes.TrimSpace(b)
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("token file %s never appeared: %v", cfg.TokenFile, err)
		}
		log.Printf("waiting for enrollment token at %s…", cfg.TokenFile)
		time.Sleep(2 * time.Second)
	}

	body, _ := json.Marshal(map[string]any{
		"enrollment_token":   string(tokenBytes),
		"hw_id":              cfg.HWID,
		"hw_kind":            cfg.HWKind,
		"name":               cfg.Name,
		"tags":               cfg.Tags,
		"agent_version":      version,
		"local_console_url":  consoleURL(cfg),
	})

	req, _ := http.NewRequest("POST", cfg.APIBase+"/v1/agent/register", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")

	// retry the platform until it's up
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		log.Printf("platform not ready yet (%v), retrying…", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register: HTTP %d: %s", resp.StatusCode, string(b))
	}

	var out struct {
		DeviceID  string `json:"device_id"`
		AuthToken string `json:"auth_token"`
		OrgID     string `json:"org_id"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	return &state{
		DeviceID:  out.DeviceID,
		OrgID:     out.OrgID,
		AuthToken: out.AuthToken,
		Name:      out.Name,
	}, nil
}

func runLoop(ctx context.Context, cfg config, st *state) {
	backoff := reconnectMin
	for ctx.Err() == nil {
		err := connect(ctx, cfg, st)
		if ctx.Err() != nil {
			return
		}
		log.Printf("ws disconnected: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

func connect(ctx context.Context, cfg config, st *state) error {
	u, err := url.Parse(cfg.APIBase)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/v1/agent/ws"
	q := u.Query()
	q.Set("token", st.AuthToken)
	u.RawQuery = q.Encode()

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, u.String(), nil)
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	log.Printf("ws connected to %s", u.String())

	// Serialize all writes — WebSocket connections can't be written concurrently.
	writes := make(chan []byte, 32)
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	// Writer goroutine.
	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case data := <-writes:
				wctx, wc := context.WithTimeout(connCtx, 10*time.Second)
				err := c.Write(wctx, websocket.MessageText, data)
				wc()
				if err != nil {
					log.Printf("ws write: %v", err)
					cancelConn()
					return
				}
			}
		}
	}()

	// Reader goroutine — surfaces errors via readErr.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := c.Read(connCtx)
			if err != nil {
				readErr <- err
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg["type"] {
			case "http.request":
				go handleHTTPRequest(connCtx, writes, msg)
			case "ws.open":
				go handleWSOpen(connCtx, writes, msg, cfg)
			case "ws.frame":
				handleWSFrame(msg)
			case "ws.close":
				handleWSClose(msg)
			default:
				// ignore unknown frames
			}
		}
	}()

	startedAt := time.Now()
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()

	// initial heartbeat immediately
	if err := enqueueHeartbeat(connCtx, writes, cfg, startedAt); err != nil {
		return err
	}

	for {
		select {
		case <-connCtx.Done():
			return connCtx.Err()
		case err := <-readErr:
			return err
		case <-tick.C:
			if err := enqueueHeartbeat(connCtx, writes, cfg, startedAt); err != nil {
				return err
			}
		}
	}
}

func enqueueHeartbeat(ctx context.Context, writes chan<- []byte, cfg config, startedAt time.Time) error {
	payload := map[string]any{
		"type":          "heartbeat",
		"cpu_temp_c":    readTempC(cfg),
		"uptime_s":      int(time.Since(startedAt).Seconds()),
		"agent_version": version,
		"at":            time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(payload)
	select {
	case writes <- b:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleHTTPRequest dispatches a platform-originated HTTP request to the local
// mux (agentMux) and publishes the response back through the write queue.
func handleHTTPRequest(ctx context.Context, writes chan<- []byte, msg map[string]any) {
	reqID, _ := msg["id"].(string)
	method, _ := msg["method"].(string)
	path, _ := msg["path"].(string)
	bodyB64, _ := msg["body_b64"].(string)
	body, _ := base64.StdEncoding.DecodeString(bodyB64)

	if method == "" {
		method = "GET"
	}
	if path == "" {
		path = "/"
	}

	req := httptest.NewRequest(method, "http://agent.local"+path, bytes.NewReader(body))
	if raw, ok := msg["headers"].([]any); ok {
		for _, h := range raw {
			pair, ok := h.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			k, _ := pair[0].(string)
			v, _ := pair[1].(string)
			if k != "" {
				req.Header.Add(k, v)
			}
		}
	}

	rec := httptest.NewRecorder()
	// Defensive: if mux isn't ready (shouldn't happen post-init), return 503.
	if agentMux == nil {
		rec.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rec.Write([]byte("agent mux not initialized"))
	} else {
		agentMux.ServeHTTP(rec, req)
	}
	res := rec.Result()

	outHeaders := make([][2]string, 0, len(res.Header))
	for k, vs := range res.Header {
		for _, v := range vs {
			outHeaders = append(outHeaders, [2]string{k, v})
		}
	}

	respBody, _ := io.ReadAll(res.Body)
	respMsg := map[string]any{
		"type":     "http.response",
		"id":       reqID,
		"status":   res.StatusCode,
		"headers":  outHeaders,
		"body_b64": base64.StdEncoding.EncodeToString(respBody),
	}
	data, _ := json.Marshal(respMsg)
	select {
	case writes <- data:
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		log.Printf("dropped http.response for %s (write queue full)", reqID)
	}
}

// --- WebSocket channel tunneling ---
//
// The platform can ask us to open a local WebSocket to kvmd and relay
// frames bidirectionally. This is how the browser gets live video and
// keyboard/mouse from PiKVM through the tunnel.

var (
	wsChannels   = make(map[string]*wsChannel)
	wsChannelsMu sync.Mutex
)

type wsChannel struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

func handleWSOpen(ctx context.Context, writes chan<- []byte, msg map[string]any, cfg config) {
	channelID, _ := msg["channel"].(string)
	path, _ := msg["path"].(string)
	if channelID == "" || path == "" {
		return
	}

	// Route to the right local service.
	// Janus has a Unix socket at /run/kvmd/janus-ws.sock — connect directly
	// to bypass nginx auth issues. Everything else goes through nginx (kvmd URL).
	var useUnixSocket string
	cleanPath := path
	if strings.HasPrefix(path, "/janus/") || strings.HasPrefix(path, "janus/") {
		useUnixSocket = "/run/kvmd/janus-ws.sock"
		// Janus expects / not /janus/ws — strip the prefix
		cleanPath = strings.TrimPrefix(path, "/janus")
		cleanPath = strings.TrimPrefix(cleanPath, "janus")
		if cleanPath == "" || cleanPath == "/ws" {
			cleanPath = "/"
		}
	}

	var u *url.URL
	if useUnixSocket != "" {
		// For Unix socket, we'll use a custom dialer — URL is just for the path
		u = &url.URL{Scheme: "ws", Host: "localhost", Path: cleanPath}
	} else {
		kvmdBase := cfg.KvmdURL
		if kvmdBase == "" {
			kvmdBase = "http://127.0.0.1:80"
		}
		var err error
		u, err = url.Parse(kvmdBase)
		if err != nil {
			log.Printf("ws.open: bad URL: %v", err)
			sendWSClose(writes, channelID)
			return
		}
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else if u.Scheme == "http" {
			u.Scheme = "ws"
		}
		u.Path = path
	}
	if qi := strings.IndexByte(u.Path, '?'); qi >= 0 {
		u.RawQuery = u.Path[qi+1:]
		u.Path = u.Path[:qi]
	}

	dialCtx, cancel := context.WithCancel(ctx)

	kvmdAuth := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(cfg.KvmdUser+":"+cfg.KvmdPass),
	)
	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {kvmdAuth},
		},
	}

	if useUnixSocket != "" {
		// Connect directly to Unix socket (bypasses nginx auth)
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", useUnixSocket)
				},
			},
		}
		opts.HTTPHeader = http.Header{} // no auth needed for Unix socket
		opts.Subprotocols = []string{"janus-protocol"}
	} else if u.Scheme == "wss" {
		// Skip TLS verify for self-signed kvmd certs
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}

	c, _, err := websocket.Dial(dialCtx, u.String(), opts)
	if err != nil {
		log.Printf("ws.open: failed to connect to %s: %v", u.String(), err)
		cancel()
		sendWSClose(writes, channelID)
		return
	}
	// Increase read limit for video frames
	c.SetReadLimit(4 * 1024 * 1024) // 4 MB

	ch := &wsChannel{conn: c, cancel: cancel}
	wsChannelsMu.Lock()
	wsChannels[channelID] = ch
	wsChannelsMu.Unlock()

	// Notify platform that channel is open
	opened, _ := json.Marshal(map[string]any{"type": "ws.opened", "channel": channelID})
	select {
	case writes <- opened:
	case <-ctx.Done():
		c.Close(websocket.StatusGoingAway, "")
		return
	}

	log.Printf("ws channel %s opened → %s", channelID[:8], u.String())

	// Read loop: kvmd → platform (→ browser)
	go func() {
		defer func() {
			c.Close(websocket.StatusNormalClosure, "")
			cancel()
			wsChannelsMu.Lock()
			delete(wsChannels, channelID)
			wsChannelsMu.Unlock()
			sendWSClose(writes, channelID)
			log.Printf("ws channel %s closed", channelID[:8])
		}()

		for {
			typ, data, err := c.Read(dialCtx)
			if err != nil {
				return
			}

			frame := map[string]any{
				"type":    "ws.frame",
				"channel": channelID,
			}
			if typ == websocket.MessageBinary {
				frame["binary"] = true
				frame["data_b64"] = base64.StdEncoding.EncodeToString(data)
			} else {
				frame["binary"] = false
				frame["data"] = string(data)
			}

			b, _ := json.Marshal(frame)
			select {
			case writes <- b:
			case <-dialCtx.Done():
				return
			case <-time.After(5 * time.Second):
				return // write queue full, close channel
			}
		}
	}()
}

func handleWSFrame(msg map[string]any) {
	channelID, _ := msg["channel"].(string)
	wsChannelsMu.Lock()
	ch := wsChannels[channelID]
	wsChannelsMu.Unlock()
	if ch == nil {
		return
	}

	isBinary, _ := msg["binary"].(bool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if isBinary {
		dataB64, _ := msg["data_b64"].(string)
		data, _ := base64.StdEncoding.DecodeString(dataB64)
		ch.conn.Write(ctx, websocket.MessageBinary, data)
	} else {
		data, _ := msg["data"].(string)
		ch.conn.Write(ctx, websocket.MessageText, []byte(data))
	}
}

func handleWSClose(msg map[string]any) {
	channelID, _ := msg["channel"].(string)
	wsChannelsMu.Lock()
	ch := wsChannels[channelID]
	delete(wsChannels, channelID)
	wsChannelsMu.Unlock()
	if ch != nil {
		ch.conn.Close(websocket.StatusNormalClosure, "")
		ch.cancel()
	}
}

func sendWSClose(writes chan<- []byte, channelID string) {
	frame, _ := json.Marshal(map[string]any{"type": "ws.close", "channel": channelID})
	select {
	case writes <- frame:
	default:
	}
}

func readTempC(cfg config) float64 {
	if !cfg.Simulate {
		// /sys/class/thermal/thermal_zone0/temp returns millidegrees C
		if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
			var v int
			fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &v)
			if v > 0 {
				return float64(v) / 1000.0
			}
		}
	}
	// simulate around 38°C ± 4
	return 38.0 + (rand.Float64()-0.5)*8.0
}
