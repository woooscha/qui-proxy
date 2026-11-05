// Package proxy implements a deliberately restricted proof of concept.
package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const maxResponse = 32 << 20
const xfsMagic = 0x58465342

type Config struct {
	Listen        string `json:"listen"`
	Upstream      string `json:"upstream"`
	VirtualPrefix string `json:"virtual_prefix"`
	DisksRoot     string `json:"disks_root"`
}

type handler struct {
	cfg           Config
	target        *url.URL
	client        *http.Client
	proxy         *httputil.ReverseProxy
	verifyTorrent func(string, []file) (string, string, error)
	syncState     syncHistory
}

type file struct {
	Name     string  `json:"name"`
	Size     *int64  `json:"size"`
	Progress float64 `json:"progress"`
}
type object map[string]json.RawMessage

func closeWithLog(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Warn("closing resource", "error", err)
	}
}

func New(c Config) (http.Handler, error) {
	if c.Listen == "" {
		return nil, errors.New("listen is required")
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("upstream must be an http(s) origin without credentials or path")
	}
	for _, p := range []string{c.VirtualPrefix, c.DisksRoot} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return nil, errors.New("data paths must be clean, absolute, and not root")
		}
	}
	h := &handler{cfg: c, target: u, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	h.verifyTorrent = h.verify
	h.proxy = &httputil.ReverseProxy{}
	h.proxy.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 20 * time.Second}
	h.proxy.Rewrite = func(r *httputil.ProxyRequest) {
		r.SetURL(u)
		r.Out.Host = u.Host
		r.Out.Header.Del("Accept-Encoding")
		if r.In.URL.Path == "/api/v2/sync/maindata" {
			r.Out.URL.RawQuery = "rid=0"
		}
	}
	h.proxy.ModifyResponse = h.modifyResponse
	h.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("proxy request failed", "path", r.URL.Path, "error", err)
		http.Error(w, "upstream or verification failed", http.StatusBadGateway)
	}
	return h, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// An explicit allowlist is necessary: qBittorrent has mutation endpoints that accept GET.
	allowed := false
	switch r.URL.Path {
	case "/api/v2/auth/login":
		allowed = r.Method == http.MethodPost
	case "/api/v2/app/version", "/api/v2/app/webapiVersion", "/api/v2/torrents/info", "/api/v2/torrents/files", "/api/v2/torrents/properties", "/api/v2/sync/maindata":
		allowed = r.Method == http.MethodGet
	}
	if !allowed || r.URL.RawPath != "" || r.URL.Path != path.Clean(r.URL.Path) {
		http.Error(w, "endpoint or method not allowed", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/v2/torrents/files" || r.URL.Path == "/api/v2/torrents/properties" {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query["hash"]) != 1 || !validHash(query["hash"][0]) {
			http.Error(w, "invalid torrent hash", http.StatusForbidden)
			return
		}
	}
	if r.URL.Path == "/api/v2/sync/maindata" {
		r = r.WithContext(context.WithValue(r.Context(), syncRIDKey{}, r.URL.Query().Get("rid")))
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *handler) modifyResponse(resp *http.Response) error {
	p := resp.Request.URL.Path
	if p != "/api/v2/torrents/info" && p != "/api/v2/sync/maindata" && p != "/api/v2/torrents/properties" {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned status %d for %s", resp.StatusCode, p)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("reading upstream response: %w", err)
	}
	if len(b) > maxResponse {
		return errors.New("upstream response too large")
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("closing upstream response: %w", err)
	}
	switch p {
	case "/api/v2/torrents/info":
		var list []object
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("decoding torrent list: %w", err)
		}
		visible := make([]object, 0, len(list))
		for _, item := range list {
			var hash string
			if err := json.Unmarshal(item["hash"], &hash); err != nil {
				return fmt.Errorf("decoding torrent hash: %w", err)
			}
			if err := h.rewriteTorrent(resp.Request, hash, item); err != nil {
				slog.Warn("hiding torrent", "hash", hash, "reason", err)
				continue
			}
			visible = append(visible, item)
		}
		list = visible
		b, err = json.Marshal(list)
	case "/api/v2/sync/maindata":
		b, err = h.syncResponse(resp.Request, b)

	default:
		var data object
		if err := json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("decoding properties: %w", err)
		}
		if err := h.rewriteTorrent(resp.Request, resp.Request.URL.Query().Get("hash"), data); err != nil {
			return err
		}
		b, err = json.Marshal(data)
	}
	if err != nil {
		return fmt.Errorf("encoding response: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", fmt.Sprint(len(b)))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("ETag")
	resp.Header.Del("Last-Modified")
	return nil
}

func (h *handler) get(r *http.Request, endpoint string, q url.Values, dst any) error {
	u := *h.target
	u.Path = endpoint
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", r.Header.Get("Cookie"))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", endpoint, err)
	}
	defer closeWithLog(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("requesting %s: status %d", endpoint, resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponse))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decoding %s: %w", endpoint, err)
	}
	return nil
}

func (h *handler) fetchTorrent(r *http.Request, requestedHash string) (object, string, string, error) {
	if !validHash(requestedHash) {
		return nil, "", "", errors.New("invalid torrent hash")
	}
	var list []object
	if err := h.get(r, "/api/v2/torrents/info", url.Values{"hashes": {requestedHash}}, &list); err != nil {
		return nil, "", "", err
	}
	if len(list) != 1 {
		return nil, "", "", errors.New("torrent missing or ambiguous")
	}
	var hash, save string
	var progress float64
	if json.Unmarshal(list[0]["hash"], &hash) != nil || !strings.EqualFold(hash, requestedHash) || json.Unmarshal(list[0]["save_path"], &save) != nil || json.Unmarshal(list[0]["progress"], &progress) != nil || progress != 1 {
		return nil, "", "", errors.New("torrent identity, save path or completion could not be verified")
	}
	var files []file
	if err := h.get(r, "/api/v2/torrents/files", url.Values{"hash": {requestedHash}}, &files); err != nil {
		return nil, "", "", err
	}
	source, destination, err := h.verifyTorrent(save, files)
	if err != nil {
		return nil, "", "", fmt.Errorf("verifying torrent files: %w", err)
	}
	return list[0], source, destination, nil
}

func validHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func (h *handler) rewriteTorrent(r *http.Request, hash string, item object) error {
	torrent, source, destination, err := h.fetchTorrent(r, hash)
	if err != nil {
		return fmt.Errorf("resolving torrent %s: %w", hash, err)
	}
	return h.rewrite(item, torrent, source, destination)
}

func (h *handler) translate(p, root string) (string, error) {
	rel, err := filepath.Rel(h.cfg.VirtualPrefix, p)
	if err != nil || !filepath.IsAbs(p) || filepath.Clean(p) != p || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path outside virtual data root")
	}
	return filepath.Join(root, rel), nil
}

func (h *handler) verifyFiles(root, physical string, files []file, filesystemCheck func(string) error) error {
	if len(files) == 0 {
		return errors.New("empty torrent file list")
	}
	for _, f := range files {
		if f.Name == "" || filepath.IsAbs(f.Name) || filepath.Clean(f.Name) != f.Name || f.Name == ".." || strings.HasPrefix(f.Name, "../") || f.Size == nil || *f.Size < 0 || f.Progress != 1 {
			return errors.New("invalid or incomplete torrent file")
		}
		target := filepath.Join(physical, f.Name)
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return errors.New("file escapes physical data root")
		}
		current := root
		// Reject symlinks at every component, including the configured root.
		st, err := os.Lstat(current)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("physical data root unavailable or unsafe")
		}
		for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			st, err = os.Lstat(current)
			if err != nil {
				return fmt.Errorf("checking torrent file: %w", err)
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return errors.New("symlink in torrent path")
			}
		}
		if !st.Mode().IsRegular() || st.Size() != *f.Size {
			return errors.New("torrent file missing, not regular or wrong size")
		}
		if err := filesystemCheck(target); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) rewrite(item, verified object, source, destination string) error {
	for _, key := range []string{"save_path", "content_path", "download_path"} {
		raw, ok := item[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("invalid %s: %w", key, err)
		}
		if value == "" {
			continue
		}
		// A partial sync update must not introduce values different from the verified snapshot.
		var reference string
		if json.Unmarshal(verified[key], &reference) != nil || reference != value {
			return fmt.Errorf("%s changed during verification", key)
		}
		translated, err := h.translate(value, destination)
		if err != nil {
			return fmt.Errorf("translating %s: %w", key, err)
		}
		// Non-save paths must exist without symlinks; their meaning depends on qBittorrent's settings.
		if key != "save_path" {
			physical, err := h.translate(value, source)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(physical)
			if err != nil || resolved != physical {
				return fmt.Errorf("unverified %s", key)
			}
		}
		item[key], err = json.Marshal(translated)
		if err != nil {
			return err
		}
	}
	return nil
}
