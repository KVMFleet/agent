// WebRTC console — Option C: MJPEG over DataChannel.
//
// The agent serves a `video` DataChannel that streams JPEG frames
// pulled from kvmd's existing MJPEG endpoint (/streamer/stream). The
// browser renders them to a <canvas> via createImageBitmap.
//
// Why this and not a real WebRTC video track:
//   - Universal across KVM hardware (every KVM-over-IP product emits
//     MJPEG; H.264 / VP8 sources are vendor-specific).
//   - Per-vendor integration becomes a 50-line "give me JPEGs" adapter.
//   - End-to-end DTLS encryption preserved (SCTP-over-DTLS instead of
//     SRTP — same trust model).
//   - Bandwidth is higher than H.264 but irrelevant on LAN, and the
//     "upgrade to real H.264 per-vendor" path is on the TODO if the
//     numbers ever matter.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

func makeWebrtcOfferHandler(cfg config, kvmdBasicAuth string, cookies []*http.Cookie, transport http.RoundTripper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			SDP  string `json:"sdp"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Type != "offer" || req.SDP == "" {
			http.Error(w, "type must be 'offer' and sdp must be non-empty", http.StatusBadRequest)
			return
		}

		iceServers := []webrtc.ICEServer{
			{URLs: []string{"stun:stun.cloudflare.com:3478"}},
		}
		if turnURL := os.Getenv("KVMFLEET_TURN_URL"); turnURL != "" {
			iceServers = append(iceServers, webrtc.ICEServer{
				URLs:           []string{turnURL},
				Username:       os.Getenv("KVMFLEET_TURN_USERNAME"),
				Credential:     os.Getenv("KVMFLEET_TURN_PASSWORD"),
				CredentialType: webrtc.ICECredentialTypePassword,
			})
		}

		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
		if err != nil {
			http.Error(w, "newPeerConnection: "+err.Error(), http.StatusInternalServerError)
			return
		}

		streamCtx, streamCancel := context.WithCancel(context.Background())

		pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
			log.Printf("webrtc: ICE state = %s", s.String())
			if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
				streamCancel()
				_ = pc.Close()
			}
		})
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			log.Printf("webrtc: connection state = %s", s.String())
			if s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateFailed {
				streamCancel()
			}
		})

		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			log.Printf("webrtc: data channel '%s' opened (id=%d)", dc.Label(), dc.ID())
			switch dc.Label() {
			case "hid":
				dc.OnMessage(func(msg webrtc.DataChannelMessage) {
					if !msg.IsString {
						return
					}
					forwardHIDEvent(cfg, kvmdBasicAuth, cookies, transport, msg.Data)
				})
			case "video":
				dc.OnOpen(func() {
					log.Printf("webrtc: video data channel open; starting MJPEG pump")
					go pumpKvmdMJPEG(streamCtx, cfg, kvmdBasicAuth, cookies, transport, dc)
				})
			default:
				log.Printf("webrtc: ignoring unknown channel %q", dc.Label())
			}
		})

		if err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  req.SDP,
		}); err != nil {
			streamCancel()
			_ = pc.Close()
			http.Error(w, "setRemoteDescription: "+err.Error(), http.StatusBadRequest)
			return
		}

		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			streamCancel()
			_ = pc.Close()
			http.Error(w, "createAnswer: "+err.Error(), http.StatusInternalServerError)
			return
		}

		gatherComplete := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(answer); err != nil {
			streamCancel()
			_ = pc.Close()
			http.Error(w, "setLocalDescription: "+err.Error(), http.StatusInternalServerError)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		select {
		case <-gatherComplete:
		case <-ctx.Done():
			log.Printf("webrtc: ICE gathering timeout, returning partial SDP")
		}

		local := pc.LocalDescription()
		if local == nil {
			streamCancel()
			_ = pc.Close()
			http.Error(w, "no local description", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sdp":  local.SDP,
			"type": local.Type.String(),
		})
	}
}

// pumpKvmdMJPEG pulls the multipart MJPEG stream from kvmd and forwards
// each JPEG frame as one binary message on the data channel. Uses the
// same Basic-auth + session-cookie scheme as the existing iframe
// console proxy, which is the known-good auth path on kvmd.
//
// Frame skipping: if the SCTP send buffer backs up above 1 MB we drop
// the current frame rather than queueing — a stale frame is useless
// for real-time display and the next one is ~33ms away anyway.
func pumpKvmdMJPEG(ctx context.Context, cfg config, basicAuth string, cookies []*http.Cookie, transport http.RoundTripper, dc *webrtc.DataChannel) {
	streamURL := strings.TrimRight(cfg.KvmdURL, "/") + "/streamer/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		log.Printf("webrtc video: new request: %v", err)
		return
	}
	req.Header.Set("Authorization", basicAuth)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	// Long-running stream; no client-side timeout.
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("webrtc video: GET %s: %v", streamURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		log.Printf("webrtc video: kvmd %s returned %d: %s",
			streamURL, resp.StatusCode, string(buf))
		return
	}

	ct := resp.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		log.Printf("webrtc video: unexpected Content-Type %q from %s", ct, streamURL)
		return
	}
	boundary, ok := params["boundary"]
	if !ok {
		log.Printf("webrtc video: no boundary in Content-Type %q", ct)
		return
	}
	log.Printf("webrtc video: MJPEG stream open from %s (boundary=%s)", streamURL, boundary)

	mr := multipart.NewReader(resp.Body, boundary)
	var frames uint64
	var dropped uint64
	logger := newRateLimitedLogger(15 * time.Second)
	defer logger.flush()

	for {
		if ctx.Err() != nil {
			return
		}
		part, err := mr.NextPart()
		if err == io.EOF {
			log.Printf("webrtc video: stream ended after %d frames (%d dropped)", frames, dropped)
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("webrtc video: read part: %v", err)
			}
			return
		}
		jpeg, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil || len(jpeg) == 0 {
			continue
		}

		// Backpressure: if the data channel is backed up, drop the frame.
		// 1 MB is enough for ~10 frames at 1080p quality without bloating
		// the operator's apparent latency.
		if dc.BufferedAmount() > 1024*1024 {
			dropped++
			continue
		}

		if err := dc.Send(jpeg); err != nil {
			if ctx.Err() == nil {
				log.Printf("webrtc video: dc.Send error: %v", err)
			}
			return
		}
		frames++
		logger.maybeLog("webrtc video: %d frames sent, %d dropped, %d bytes buffered",
			frames, dropped, dc.BufferedAmount())
	}
}

// --- utilities ------------------------------------------------------------

type rateLimitedLogger struct {
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
	pending  string
}

func newRateLimitedLogger(interval time.Duration) *rateLimitedLogger {
	return &rateLimitedLogger{interval: interval}
}

func (l *rateLimitedLogger) maybeLog(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) < l.interval {
		l.pending = fmt.Sprintf(format, args...)
		return
	}
	l.last = now
	log.Printf(format, args...)
	l.pending = ""
}

func (l *rateLimitedLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != "" {
		log.Print(l.pending)
		l.pending = ""
	}
}

// --- HID forwarder (unchanged from phase 1) -------------------------------

func forwardHIDEvent(cfg config, basicAuth string, cookies []*http.Cookie, transport http.RoundTripper, raw []byte) {
	var ev struct {
		Type   string `json:"type"`
		Code   string `json:"code,omitempty"`
		State  bool   `json:"state,omitempty"`
		DX     int    `json:"dx,omitempty"`
		DY     int    `json:"dy,omitempty"`
		Button string `json:"button,omitempty"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}

	var path string
	switch ev.Type {
	case "key":
		path = fmt.Sprintf(
			"/api/hid/events/send_key?key=%s&state=%v",
			url.QueryEscape(ev.Code),
			ev.State,
		)
	case "mouse-button":
		path = fmt.Sprintf(
			"/api/hid/events/send_mouse_button?button=%s&state=%v",
			url.QueryEscape(ev.Button),
			ev.State,
		)
	case "mouse-move":
		path = fmt.Sprintf("/api/hid/events/send_mouse_move?to_x=%d&to_y=%d", ev.DX, ev.DY)
	case "mouse-wheel":
		path = fmt.Sprintf("/api/hid/events/send_mouse_wheel?delta_x=%d&delta_y=%d", ev.DX, ev.DY)
	default:
		return
	}

	target := strings.TrimRight(cfg.KvmdURL, "/") + path
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", basicAuth)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("webrtc HID forward error (%s): %v", ev.Type, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		log.Printf("webrtc HID forward kvmd %d: %s", resp.StatusCode, string(buf[:min(200, len(buf))]))
	}
}
