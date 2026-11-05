package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func writeResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

func TestVerifyFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	h := handler{cfg: Config{DisksRoot: root}}
	tests := []struct {
		name  string
		files []file
		bad   bool
	}{
		{"complete", []file{{Name: "dir/file", Size: new(int64(3)), Progress: 1}}, false},
		{"missing", []file{{Name: "dir/missing", Size: new(int64(3)), Progress: 1}}, true},
		{"wrong size", []file{{Name: "dir/file", Size: new(int64(2)), Progress: 1}}, true},
		{"partial", []file{{Name: "dir/file", Size: new(int64(3)), Progress: 0.5}}, true},
		{"parent traversal", []file{{Name: "../file", Size: new(int64(3)), Progress: 1}}, true},
		{"symlink", []file{{Name: "link/file", Size: new(int64(3)), Progress: 1}}, true},
		{"empty list", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := h.verifyFiles(root, root, tt.files, func(string) error { return nil })
			if (err != nil) != tt.bad {
				t.Fatalf("verifyFiles error = %v, want error %v", err, tt.bad)
			}
		})
	}
	if err := h.verifyFiles(root, root, []file{{Name: "dir/file", Size: new(int64(3)), Progress: 1}}, func(string) error { return errors.New("wrong filesystem") }); err == nil {
		t.Fatal("wrong filesystem accepted")
	}
}

func TestTranslate(t *testing.T) {
	t.Parallel()
	h := handler{cfg: Config{VirtualPrefix: "/data", DisksRoot: "/mnt/disk4/data"}}
	for _, tt := range []struct {
		in, want string
		bad      bool
	}{{"/data/tv/a", "/mnt/disk4/data/tv/a", false}, {"/data", "/mnt/disk4/data", false}, {"/data2/file", "", true}, {"/data/../secret", "", true}, {"relative", "", true}} {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := h.translate(tt.in, "/mnt/disk4/data")
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("translate = %q, %v", got, err)
			}
		})
	}
}

func TestAllowedEndpoints(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	hash := strings.Repeat("a", 40)
	h, err := New(Config{Listen: ":0", Upstream: upstream.URL, VirtualPrefix: "/data", DisksRoot: "/mnt/disk4/data"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/api/v2/auth/login", 204}, {"GET", "/api/v2/app/version", 204},
		{"POST", "/api/v2/torrents/delete", 403}, {"GET", "/api/v2/torrents/delete", 403},
		{"POST", "/api/v2/torrents/info", 403}, {"GET", "/api/v2/torrents/files?hash=" + strings.Repeat("b", 40), 204},
		{"GET", "/api/v2/torrents/files?hash=invalid", 403},
		{"GET", "/api/v2/torrents/files?hash=" + hash + "&hash=" + strings.Repeat("b", 40), 403},
		{"GET", "/api/v2/torrents/properties?hash=" + hash + "&bad=%zz", 403},
		{"GET", "/api/v2/torrents/files?hash=" + hash, 204},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("got status %d, want %d", w.Code, tt.status)
			}
		})
	}
}

func TestRewriteEndpointsRejectNonOK(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		writeResponse(t, w, "/data/private")
	}))
	t.Cleanup(upstream.Close)
	hash := strings.Repeat("a", 40)
	h, err := New(Config{Listen: ":0", Upstream: upstream.URL, VirtualPrefix: "/data", DisksRoot: "/mnt/disk4/data"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, path string }{
		{"info", "/api/v2/torrents/info"}, {"sync", "/api/v2/sync/maindata"}, {"properties", "/api/v2/torrents/properties?hash=" + hash},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "/data/private") {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSyncIncrementalAndLogin(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 40)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test", Path: "/"})
			writeResponse(t, w, "Ok.")
		case "/api/v2/torrents/info":
			if _, err := r.Cookie("SID"); err != nil {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			writeResponse(t, w, `[{"hash":"`+hash+`","save_path":"/data/tv","progress":1}]`)
		case "/api/v2/torrents/files":
			writeResponse(t, w, `[{"name":"movie","size":3,"progress":1}]`)
		case "/api/v2/sync/maindata":
			writeResponse(t, w, `{"rid":42,"full_update":true,"torrents":{"`+hash+`":{"save_path":"/data/tv","state":"uploading"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	raw, err := New(Config{Listen: ":0", Upstream: upstream.URL, VirtualPrefix: "/data", DisksRoot: "/mnt/disk4/data"})
	if err != nil {
		t.Fatal(err)
	}
	h := raw.(*handler)
	h.verifyTorrent = func(string, []file) (string, string, error) { return "/mnt/disk4/data", "/disk4", nil }
	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest("POST", "/api/v2/auth/login", nil))
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("login cookie not forwarded")
	}
	req := httptest.NewRequest("GET", "/api/v2/sync/maindata?rid=41", nil)
	req.AddCookie(cookies[0])
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"full_update":true`) || !strings.Contains(w.Body.String(), `"save_path":"/disk4/tv"`) {
		t.Fatalf("unexpected sync response: %d %s", w.Code, w.Body.String())
	}
	h.verifyTorrent = func(string, []file) (string, string, error) { return "", "", errors.New("disk missing") }
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), hash) {
		t.Fatalf("verification failure returned %d %s", w.Code, w.Body.String())
	}
}

func TestSelectDisk(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		first, second []string
		want          string
		bad           bool
	}{
		{name: "first disk", first: []string{"one", "two"}, want: "/disk1"},
		{name: "second disk", second: []string{"one", "two"}, want: "/disk20"},
		{name: "missing", bad: true},
		{name: "spread", first: []string{"one"}, second: []string{"two"}, bad: true},
		{name: "duplicate", first: []string{"one", "two"}, second: []string{"one", "two"}, bad: true},
		{name: "partial duplicate", first: []string{"one", "two"}, second: []string{"one"}, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first, second := t.TempDir(), t.TempDir()
			for _, d := range []struct {
				root  string
				names []string
			}{{first, tc.first}, {second, tc.second}} {
				for _, name := range d.names {
					if err := os.WriteFile(filepath.Join(d.root, name), []byte("x"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			h := handler{cfg: Config{VirtualPrefix: "/data"}}
			files := []file{{Name: "one", Size: new(int64(1)), Progress: 1}, {Name: "two", Size: new(int64(1)), Progress: 1}}
			_, got, err := h.selectDisk("/data", files, []disk{{first, "/disk1"}, {second, "/disk20"}}, func(root, physical string, files []file) error {
				return h.verifyFiles(root, physical, files, func(string) error { return nil })
			})
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("selectDisk = %q, %v, want %q, error %v", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestDiskName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{{"disk1", true}, {"disk10", true}, {"disk20", true}, {"disk0", false}, {"disk01", false}, {"disk1x", false}, {"cache", false}, {"disk", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := diskName(tc.name); got != tc.want {
				t.Fatalf("diskName = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEligibleCapacity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sectors string
		want, bad     bool
	}{
		{"below", "19531249999", false, false}, {"exactly ten tb", "19531250000", false, false},
		{"above", "19531250001\n", true, false}, {"unknown", "invalid", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := eligibleCapacity(tc.sectors)
			if got != tc.want || (err != nil) != tc.bad {
				t.Fatalf("eligibleCapacity = %v, %v", got, err)
			}
		})
	}
}

func TestPresentRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := present(root, "link/missing"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestMultipleTorrents(t *testing.T) {
	t.Parallel()
	hashA, hashB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("SID"); err != nil {
			http.Error(w, "missing cookie", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			switch r.URL.Query().Get("hashes") {
			case hashA:
				writeResponse(t, w, `[{"hash":"`+hashA+`","save_path":"/data/first","progress":1}]`)
			case hashB:
				writeResponse(t, w, `[{"hash":"`+hashB+`","save_path":"/data/second","progress":1}]`)
			default:
				writeResponse(t, w, `[{"hash":"`+hashA+`","save_path":"/data/first","progress":1},{"hash":"`+hashB+`","save_path":"/data/second","progress":1}]`)
			}
		case "/api/v2/torrents/files":
			writeResponse(t, w, `[{"name":"file","size":1,"progress":1}]`)
		case "/api/v2/torrents/properties":
			writeResponse(t, w, `{"save_path":"/data/second"}`)
		case "/api/v2/sync/maindata":
			writeResponse(t, w, `{"rid":23,"full_update":true,"torrents":{"`+hashA+`":{"save_path":"/data/first"},"`+hashB+`":{"save_path":"/data/second"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	for _, tc := range []struct {
		name, path string
		fail       bool
		want       []string
		status     int
	}{
		{"info", "/api/v2/torrents/info", false, []string{`"save_path":"/disk1/first"`, `"save_path":"/disk20/second"`}, http.StatusOK},
		{"incremental sync", "/api/v2/sync/maindata?rid=22", false, []string{`"full_update":true`, `"save_path":"/disk1/first"`, `"save_path":"/disk20/second"`}, http.StatusOK},
		{"second properties", "/api/v2/torrents/properties?hash=" + hashB, false, []string{`"save_path":"/disk20/second"`}, http.StatusOK},
		{"one disk missing", "/api/v2/torrents/info", true, []string{`"save_path":"/disk1/first"`}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := New(Config{Listen: ":0", Upstream: upstream.URL, VirtualPrefix: "/data", DisksRoot: "/disks"})
			if err != nil {
				t.Fatal(err)
			}
			h := raw.(*handler)
			h.verifyTorrent = func(save string, _ []file) (string, string, error) {
				if save == "/data/second" {
					if tc.fail {
						return "", "", errors.New("disk missing")
					}
					return "/disks/disk20/data", "/disk20", nil
				}
				if save == "/data/first" {
					return "/disks/disk1/data", "/disk1", nil
				}
				return "", "", errors.New("unknown torrent")
			}
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.AddCookie(&http.Cookie{Name: "SID", Value: "test"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(w.Body.String(), want) {
					t.Fatalf("missing %s in %s", want, w.Body.String())
				}
			}
			if tc.fail && (strings.Contains(w.Body.String(), "/data/") || strings.Contains(w.Body.String(), "/disk20/") || strings.Contains(w.Body.String(), hashB)) {
				t.Fatalf("verification failure leaked paths: %s", w.Body.String())
			}
		})
	}
}

func TestSyncVisibilityLifecycle(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 40)
	var safe atomic.Bool
	safe.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/sync/maindata":
			if r.URL.Query().Get("rid") != "0" {
				t.Errorf("upstream received rid %q", r.URL.Query().Get("rid"))
			}
			writeResponse(t, w, `{"rid":4,"full_update":true,"categories":{"tv":{"name":"tv"}},"tags":["tag"],"trackers":{"tracker":["`+hash+`"]},"torrents":{"`+hash+`":{"save_path":"/data/tv","name":"test torrent","state":"uploading","progress":1}}}`)
		case "/api/v2/torrents/info":
			writeResponse(t, w, `[{"hash":"`+hash+`","save_path":"/data/tv","progress":1}]`)
		case "/api/v2/torrents/files":
			writeResponse(t, w, `[{"name":"file","size":1,"progress":1}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	raw, err := New(Config{Listen: ":0", Upstream: upstream.URL, VirtualPrefix: "/data", DisksRoot: "/disks"})
	if err != nil {
		t.Fatal(err)
	}
	h := raw.(*handler)
	h.verifyTorrent = func(string, []file) (string, string, error) {
		if !safe.Load() {
			return "", "", errors.New("file missing")
		}
		return "/disks/disk4/data", "/disk4", nil
	}
	request := func(rid int, sid string) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v2/sync/maindata?rid=%d", rid), nil)
		req.AddCookie(&http.Cookie{Name: "SID", Value: sid})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("sync returned %d: %s", w.Code, w.Body.String())
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	readRID := func(data map[string]json.RawMessage) int {
		t.Helper()
		var rid int
		if err := json.Unmarshal(data["rid"], &rid); err != nil || rid == 0 {
			t.Fatalf("invalid rid %s: %v", data["rid"], err)
		}
		return rid
	}
	initial := request(0, "one")
	firstRID := readRID(initial)
	if string(initial["full_update"]) != "true" || !strings.Contains(string(initial["torrents"]), `"save_path":"/disk4/tv"`) {
		t.Fatalf("initial: %s", initial)
	}
	safe.Store(false)
	missing := request(firstRID, "one")
	missingRID := readRID(missing)
	if string(missing["full_update"]) != "false" || !strings.Contains(string(missing["torrents_removed"]), hash) || strings.Contains(string(missing["torrents"]), hash) || strings.Contains(string(missing["trackers"]), hash) || !strings.Contains(string(missing["trackers_removed"]), "tracker") {
		t.Fatalf("missing: %s", missing)
	}
	retry := request(firstRID, "one")
	if !strings.Contains(string(retry["torrents_removed"]), hash) {
		t.Fatalf("retry lost removal: %s", retry)
	}
	other := request(firstRID, "different")
	if string(other["full_update"]) != "true" {
		t.Fatalf("other session did not receive full update: %s", other)
	}
	safe.Store(true)
	restored := request(missingRID, "one")
	if string(restored["full_update"]) != "false" || !strings.Contains(string(restored["torrents"]), `"name":"test torrent"`) || !strings.Contains(string(restored["torrents"]), `"state":"uploading"`) || !strings.Contains(string(restored["torrents"]), `"save_path":"/disk4/tv"`) {
		t.Fatalf("restored: %s", restored)
	}
}

func TestSyncMetadataRemovals(t *testing.T) {
	t.Parallel()
	h := &handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/sync/maindata", nil)
	req = req.WithContext(context.WithValue(req.Context(), syncRIDKey{}, "0"))
	first, err := h.syncResponse(req, []byte(`{"full_update":true,"categories":{"tv":{}},"tags":["tag"],"trackers":{"tracker":["hash"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(first, &data); err != nil {
		t.Fatal(err)
	}
	var rid int
	if err := json.Unmarshal(data["rid"], &rid); err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(context.WithValue(req.Context(), syncRIDKey{}, fmt.Sprint(rid)))
	second, err := h.syncResponse(req, []byte(`{"full_update":true,"categories":{},"tags":[],"trackers":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &data); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"categories_removed": `["tv"]`, "tags_removed": `["tag"]`} {
		if string(data[key]) != want {
			t.Fatalf("%s = %s, want %s", key, data[key], want)
		}
	}
}
