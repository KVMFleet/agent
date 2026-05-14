// ISO mount/unmount handlers — invoked by the platform's
// /v1/devices/{id}/iso:{mount,unmount} routes through the existing
// HTTP tunnel.
//
// Mount flow:
//  1. Receive {name, source_url, sha256, size_bytes, media_type}
//  2. Stream-download source_url to a tmp file, computing sha256 as we go
//  3. Reject on size or sha256 mismatch
//  4. Multipart-upload to kvmd POST /api/msd/write
//  5. POST /api/msd/connect?type=<media_type>
//
// Unmount flow:
//  1. POST /api/msd/disconnect
//
// Errors are surfaced as HTTP status codes; the platform forwards the
// message verbatim to the operator dashboard.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type isoMountReq struct {
	Name      string `json:"name"`
	SourceURL string `json:"source_url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

func makeIsoMountHandler(cfg config, kvmdBasicAuth string, cookies []*http.Cookie, transport http.RoundTripper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req isoMountReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.SourceURL == "" || req.SHA256 == "" {
			http.Error(w, "name, source_url, and sha256 are required", http.StatusBadRequest)
			return
		}
		if req.MediaType == "" {
			req.MediaType = "cdrom"
		}

		// Stage 1 — download to a tmp file with a streaming sha256.
		tmp, err := os.CreateTemp("", "kvmfleet-iso-*.bin")
		if err != nil {
			http.Error(w, "cannot create tmp file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()

		log.Printf("iso.mount: downloading %s (%s) sha256=%s", req.Name, req.SourceURL, req.SHA256)

		dlReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, req.SourceURL, nil)
		dlClient := &http.Client{}
		dlResp, err := dlClient.Do(dlReq)
		if err != nil {
			http.Error(w, "download failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer dlResp.Body.Close()
		if dlResp.StatusCode >= 400 {
			http.Error(w, fmt.Sprintf("source URL returned %d", dlResp.StatusCode), http.StatusBadGateway)
			return
		}

		hasher := sha256.New()
		written, err := io.Copy(io.MultiWriter(tmp, hasher), dlResp.Body)
		if err != nil {
			http.Error(w, "download streaming failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		gotSum := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(gotSum, req.SHA256) {
			http.Error(w, fmt.Sprintf("sha256 mismatch (expected %s got %s)", req.SHA256, gotSum), http.StatusBadRequest)
			return
		}
		if req.SizeBytes > 0 && written != req.SizeBytes {
			http.Error(w, fmt.Sprintf("size mismatch (expected %d got %d)", req.SizeBytes, written), http.StatusBadRequest)
			return
		}
		log.Printf("iso.mount: download ok, %d bytes, sha256 verified", written)

		// Stage 2 — disconnect any current mount + remove staged image with same name.
		// Best-effort: ignore failures.
		_ = isoCallKvmd(cfg, kvmdBasicAuth, cookies, transport, http.MethodPost, "/api/msd/set_connected?connected=0", "", nil)
		_ = isoCallKvmd(cfg, kvmdBasicAuth, cookies, transport, http.MethodPost, "/api/msd/remove?image="+url.QueryEscape(req.Name), "", nil)

		// Stage 3 — multipart upload to kvmd MSD.
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "tmp rewind failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		uploadPath := "/api/msd/write?image=" + url.QueryEscape(req.Name)
		if err := isoUpload(cfg, kvmdBasicAuth, cookies, transport, uploadPath, req.Name, tmp); err != nil {
			http.Error(w, "kvmd upload failed: "+err.Error(), http.StatusBadGateway)
			return
		}

		// Stage 4 — connect as the requested media type.
		// kvmd 4.x: POST /api/msd/set_params?image=<name>&cdrom=1; POST /api/msd/set_connected?connected=1
		paramsQS := "image=" + url.QueryEscape(req.Name)
		if req.MediaType == "cdrom" {
			paramsQS += "&cdrom=1"
		}
		if err := isoCallKvmd(cfg, kvmdBasicAuth, cookies, transport, http.MethodPost, "/api/msd/set_params?"+paramsQS, "", nil); err != nil {
			http.Error(w, "kvmd set_params failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := isoCallKvmd(cfg, kvmdBasicAuth, cookies, transport, http.MethodPost, "/api/msd/set_connected?connected=1", "", nil); err != nil {
			http.Error(w, "kvmd set_connected failed: "+err.Error(), http.StatusBadGateway)
			return
		}

		log.Printf("iso.mount: %s mounted as %s on kvmd", req.Name, req.MediaType)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","name":%q,"sha256":%q,"size_bytes":%d,"media_type":%q}`,
			req.Name, gotSum, written, req.MediaType)
	}
}

func makeIsoUnmountHandler(cfg config, kvmdBasicAuth string, cookies []*http.Cookie, transport http.RoundTripper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := isoCallKvmd(cfg, kvmdBasicAuth, cookies, transport, http.MethodPost, "/api/msd/set_connected?connected=0", "", nil); err != nil {
			http.Error(w, "kvmd disconnect failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}
}

// --- low-level kvmd helpers ----------------------------------------------

func isoCallKvmd(cfg config, basicAuth string, cookies []*http.Cookie, transport http.RoundTripper, method, path, contentType string, body io.Reader) error {
	target := strings.TrimRight(cfg.KvmdURL, "/") + path
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", basicAuth)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kvmd %s %s -> %d: %s", method, path, resp.StatusCode, string(buf[:min(200, len(buf))]))
	}
	return nil
}

func isoUpload(cfg config, basicAuth string, cookies []*http.Cookie, transport http.RoundTripper, path, filename string, file io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mw.Close()
		part, err := mw.CreateFormFile("image", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	target := strings.TrimRight(cfg.KvmdURL, "/") + path
	req, err := http.NewRequest(http.MethodPost, target, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", basicAuth)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kvmd upload -> %d: %s", resp.StatusCode, string(buf[:min(500, len(buf))]))
	}
	return nil
}

