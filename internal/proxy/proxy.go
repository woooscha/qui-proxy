// Package proxy implements a deliberately restricted, single-torrent proof of concept.
package proxy

import (
	"bufio"
	"bytes"
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
	"slices"
	"strings"
	"syscall"
	"time"
)

const maxResponse = 32 << 20
const xfsMagic = 0x58465342

type Config struct {
	Listen        string `json:"listen"`
	Upstream      string `json:"upstream"`
	TorrentHash   string `json:"torrent_hash"`
	VirtualPrefix string `json:"virtual_prefix"`
	PhysicalData  string `json:"physical_data"`
}

type handler struct {
	cfg           Config
	target        *url.URL
	client        *http.Client
	proxy         *httputil.ReverseProxy
	verifyTorrent func(string, []file) error
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
	c.TorrentHash = strings.ToLower(c.TorrentHash)
	if len(c.TorrentHash) != 40 && len(c.TorrentHash) != 64 {
		return nil, errors.New("torrent_hash must be a 40 or 64 character hex hash")
	}
	if _, err := hex.DecodeString(c.TorrentHash); err != nil {
		return nil, fmt.Errorf("invalid torrent hash: %w", err)
	}
	for _, p := range []string{c.VirtualPrefix, c.PhysicalData} {
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
		if err != nil || len(query["hash"]) != 1 || !strings.EqualFold(query["hash"][0], h.cfg.TorrentHash) {
			http.Error(w, "torrent not enabled in proof of concept", http.StatusForbidden)
			return
		}
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
	// Resolve independently on every response, including incremental sync responses.
	torrent, err := h.fetchTorrent(resp.Request)
	if err != nil {
		return err
	}
	switch p {
	case "/api/v2/torrents/info":
		var list []object
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("decoding torrent list: %w", err)
		}
		for _, item := range list {
			var hash string
			if err := json.Unmarshal(item["hash"], &hash); err != nil {
				return fmt.Errorf("decoding torrent hash: %w", err)
			}
			if strings.EqualFold(hash, h.cfg.TorrentHash) {
				if err := h.rewrite(item, torrent); err != nil {
					return err
				}
			}
		}
		b, err = json.Marshal(list)
	case "/api/v2/sync/maindata":
		var data object
		if err := json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("decoding sync: %w", err)
		}
		var torrents map[string]object
		if v, ok := data["torrents"]; ok {
			if err := json.Unmarshal(v, &torrents); err != nil {
				return fmt.Errorf("decoding sync torrents: %w", err)
			}
		}
		for hash, item := range torrents {
			if strings.EqualFold(hash, h.cfg.TorrentHash) {
				if err := h.rewrite(item, torrent); err != nil {
					return err
				}
			}
		}
		if torrents != nil {
			data["torrents"], err = json.Marshal(torrents)
			if err != nil {
				return err
			}
		}
		b, err = json.Marshal(data)
	default:
		var data object
		if err := json.Unmarshal(b, &data); err != nil {
			return fmt.Errorf("decoding properties: %w", err)
		}
		if err := h.rewrite(data, torrent); err != nil {
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

func (h *handler) fetchTorrent(r *http.Request) (object, error) {
	var list []object
	if err := h.get(r, "/api/v2/torrents/info", url.Values{"hashes": {h.cfg.TorrentHash}}, &list); err != nil {
		return nil, err
	}
	if len(list) != 1 {
		return nil, errors.New("configured torrent missing or ambiguous")
	}
	var hash, save string
	var progress float64
	if json.Unmarshal(list[0]["hash"], &hash) != nil || !strings.EqualFold(hash, h.cfg.TorrentHash) || json.Unmarshal(list[0]["save_path"], &save) != nil || json.Unmarshal(list[0]["progress"], &progress) != nil || progress != 1 {
		return nil, errors.New("torrent identity, save path or completion could not be verified")
	}
	var files []file
	if err := h.get(r, "/api/v2/torrents/files", url.Values{"hash": {h.cfg.TorrentHash}}, &files); err != nil {
		return nil, err
	}
	if err := h.verifyTorrent(save, files); err != nil {
		return nil, fmt.Errorf("verifying torrent files: %w", err)
	}
	return list[0], nil
}

func (h *handler) translate(p string) (string, error) {
	rel, err := filepath.Rel(h.cfg.VirtualPrefix, p)
	if err != nil || !filepath.IsAbs(p) || filepath.Clean(p) != p || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path outside virtual data root")
	}
	return filepath.Join(h.cfg.PhysicalData, rel), nil
}

func (h *handler) verify(save string, files []file) error {
	if err := readOnlyMount(h.cfg.PhysicalData); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(h.cfg.PhysicalData)
	if err != nil || root != h.cfg.PhysicalData {
		return errors.New("physical data root has symlink or is unavailable")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(h.cfg.PhysicalData, &fs); err != nil {
		return fmt.Errorf("checking filesystem: %w", err)
	}
	if uint64(fs.Type) != xfsMagic {
		return errors.New("physical data is not on xfs")
	}
	physical, err := h.translate(save)
	if err != nil {
		return err
	}
	return h.verifyFiles(physical, files, func(target string) error {
		var filefs syscall.Statfs_t
		if err := syscall.Statfs(target, &filefs); err != nil || filefs.Fsid != fs.Fsid || uint64(filefs.Type) != xfsMagic {
			return errors.New("torrent file is not on the configured xfs filesystem")
		}
		return nil
	})
}

func (h *handler) verifyFiles(physical string, files []file, filesystemCheck func(string) error) error {
	if len(files) == 0 {
		return errors.New("empty torrent file list")
	}
	for _, f := range files {
		if f.Name == "" || filepath.IsAbs(f.Name) || filepath.Clean(f.Name) != f.Name || f.Name == ".." || strings.HasPrefix(f.Name, "../") || f.Size == nil || *f.Size < 0 || f.Progress != 1 {
			return errors.New("invalid or incomplete torrent file")
		}
		target := filepath.Join(physical, f.Name)
		rel, err := filepath.Rel(h.cfg.PhysicalData, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return errors.New("file escapes physical data root")
		}
		current := h.cfg.PhysicalData
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

// Require the exact data directory as a read-only XFS bind mount. This avoids
// accepting an ordinary directory left behind after a disk is unmounted.
func readOnlyMount(root string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("opening mount information: %w", err)
	}
	defer closeWithLog(f)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := -1
		for i, v := range fields {
			if v == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 6 || sep < 6 || sep+1 >= len(fields) {
			continue
		}
		mountpoint := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if mountpoint == root && fields[sep+1] == "xfs" {
			if slices.Contains(strings.Split(fields[5], ","), "ro") {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading mount information: %w", err)
	}
	return errors.New("physical data must be an exact read-only xfs mount")
}

func (h *handler) rewrite(item, verified object) error {
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
		translated, err := h.translate(value)
		if err != nil {
			return fmt.Errorf("translating %s: %w", key, err)
		}
		// Non-save paths must exist without symlinks; their meaning depends on qBittorrent's settings.
		if key != "save_path" {
			resolved, err := filepath.EvalSymlinks(translated)
			if err != nil || resolved != translated {
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
