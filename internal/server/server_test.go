package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/config"
	"github.com/Soli0222/navidrome-curator/internal/cover"
	"github.com/Soli0222/navidrome-curator/internal/index"
	"github.com/Soli0222/navidrome-curator/internal/jobs"
	"github.com/Soli0222/navidrome-curator/internal/library"
	"github.com/Soli0222/navidrome-curator/internal/metrics"
	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/trash"
)

type testServer struct {
	*httptest.Server
	srv    *Server
	source string
	lib    string
}

func newTestServer(t *testing.T, auth config.Auth) *testServer {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	root := filepath.Join(base, "library")
	lib := filepath.Join(root, "shared")
	for _, p := range []string{filepath.Join(source, "Artist", "Album"), lib} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "Artist", "Album", "01.flac"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Sources:     []config.Source{{Name: "music", Path: source, DefaultLibraries: []string{"shared"}}},
		LibraryRoot: root,
		Libraries:   []config.Library{{Name: "shared", Path: lib}},
		WorkDir:     filepath.Join(root, ".curator"),
		Auth:        auth,
	}
	cfg.Trash.Retention = config.Duration(time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rules, _ := scan.NewRules([]string{"flac"}, []string{"._*"}, true)
	tr, err := trash.New(cfg.TrashDir(), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	covers, err := cover.New(rules, filepath.Join(cfg.WorkDir, "cache", "covers"), 0o755, log)
	if err != nil {
		t.Fatal(err)
	}
	idx := index.New([]index.Source{{Name: "music", Path: source}}, rules, log, index.Hooks{})
	lm := library.New([]library.Library{{Name: "shared", Path: lib}}, rules, tr, 0o755, log)
	jm := jobs.New(jobs.Options{
		Sources: map[string]string{"music": source}, LibraryRoot: root, StagingDir: cfg.StagingDir(),
		FileMode: 0o644, DirMode: 0o755,
	}, rules, lm, nil, log, jobs.Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := jm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Config: cfg, Rules: rules, Covers: covers, Index: idx, Libs: lm, Jobs: jm, Trash: tr, Metrics: metrics.New(), Log: log,
		BaseContext: ctx,
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		cancel()
		jm.Wait()
	})
	return &testServer{Server: hs, srv: s, source: source, lib: lib}
}

func (ts *testServer) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp, out
}

func TestBasicAuth(t *testing.T) {
	ts := newTestServer(t, config.Auth{Mode: config.AuthBasic, Basic: config.BasicAuth{Username: "admin", Password: "pw"}})
	resp, _ := ts.do(t, "GET", "/api/status", "", nil)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no credentials: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.SetBasicAuth("admin", "wrong")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	req.SetBasicAuth("admin", "pw")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusOK {
		t.Fatalf("right password: %d", r.StatusCode)
	}
	// ヘルスチェックとメトリクスは認証なしで返す。
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if r, _ := ts.do(t, "GET", p, "", nil); r.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
}

func TestHeaderAuth(t *testing.T) {
	trusted := config.Auth{Mode: config.AuthHeader, Header: config.HeaderAuth{
		Name: "X-Forwarded-User", Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
	}}
	ts := newTestServer(t, trusted)
	if r, _ := ts.do(t, "GET", "/api/status", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing header: %d", r.StatusCode)
	}
	if r, body := ts.do(t, "GET", "/api/status", "", map[string]string{"X-Forwarded-User": "alice"}); r.StatusCode != http.StatusOK || body["user"] != "alice" {
		t.Fatalf("with header: %d %v", r.StatusCode, body)
	}

	untrusted := trusted
	untrusted.Header.Trusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}
	ts2 := newTestServer(t, untrusted)
	if r, _ := ts2.do(t, "GET", "/api/status", "", map[string]string{"X-Forwarded-User": "alice"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted proxy: %d", r.StatusCode)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	ts := newTestServer(t, config.Auth{Mode: config.AuthNone})
	body := `{"items":[{"source":"music","path":"Artist/Album"}]}`
	r, _ := ts.do(t, "POST", "/api/jobs", body, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", r.StatusCode)
	}
	r, out := ts.do(t, "POST", "/api/jobs", body, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if r.StatusCode != http.StatusOK || out["created"].(float64) != 1 {
		t.Fatalf("same-origin POST: %d %v", r.StatusCode, out)
	}
}

func TestBrowseRejectsTraversal(t *testing.T) {
	ts := newTestServer(t, config.Auth{Mode: config.AuthNone})
	for _, p := range []string{"..", "../library", "/etc", "Artist/../../x"} {
		r, out := ts.do(t, "GET", "/api/browse?source=music&path="+p, "", nil)
		if r.StatusCode != http.StatusBadRequest || out["error"] != "invalid_path" {
			t.Errorf("browse %q: %d %v", p, r.StatusCode, out)
		}
	}
	r, out := ts.do(t, "GET", "/api/browse?source=music&path=Artist", "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("browse: %d %v", r.StatusCode, out)
	}
	dirs := out["dirs"].([]any)
	if len(dirs) != 1 || !dirs[0].(map[string]any)["isAlbum"].(bool) {
		t.Fatalf("dirs = %v", dirs)
	}
}

func TestBulkCheck(t *testing.T) {
	ts := newTestServer(t, config.Auth{Mode: config.AuthNone})
	text := "music\tArtist/Album\nmusic\tArtist/Missing\nnope\tArtist/Album\nmusic\t../x\nmusic\tArtist/Album\tother\n# comment\n\n"
	b, _ := json.Marshal(map[string]string{"text": text})
	r, out := ts.do(t, "POST", "/api/bulk/check", string(b), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("bulk: %d %v", r.StatusCode, out)
	}
	rows := out["rows"].([]any)
	want := []struct{ status, err string }{
		{"found", ""}, {"not_found", ""}, {"invalid", "unknown_source"}, {"invalid", "invalid_path"}, {"invalid", "unknown_library"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v", rows)
	}
	for i, w := range want {
		row := rows[i].(map[string]any)
		gotErr, _ := row["error"].(string)
		if row["status"] != w.status || gotErr != w.err {
			t.Errorf("row %d = %v, want %v", i, row, w)
		}
	}
}

func TestCoverAndGroupedAlbums(t *testing.T) {
	ts := newTestServer(t, config.Auth{Mode: config.AuthNone})
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 500, 500))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts.source, "Artist", "Album", "cover.png"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"Disc 1", "Disc 2"} {
		p := filepath.Join(ts.source, "Artist", "Box", d)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "01.flac"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := http.Get(ts.URL + "/api/cover?source=music&path=Artist/Album&size=200")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("ETag") == "" {
		t.Fatalf("cover: %d %v", resp.StatusCode, resp.Header)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/cover?source=music&path=Artist/Album&size=200", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional request: %d", resp2.StatusCode)
	}
	if r, out := ts.do(t, "GET", "/api/cover?source=music&path=Artist/Box/Disc%201", "", nil); r.StatusCode != http.StatusNotFound || out["error"] != "no_cover" {
		t.Fatalf("no cover: %d %v", r.StatusCode, out)
	}
	if r, _ := ts.do(t, "GET", "/api/cover?source=music&path=../x", "", nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("traversal: %d", r.StatusCode)
	}

	_, out := ts.do(t, "GET", "/api/browse?source=music&path=Artist", "", nil)
	var box map[string]any
	for _, d := range out["dirs"].([]any) {
		if d.(map[string]any)["name"] == "Box" {
			box = d.(map[string]any)
		}
	}
	if box == nil || len(box["albums"].([]any)) != 2 || box["albums"].([]any)[0].(map[string]any)["name"] != "Disc 1" {
		t.Fatalf("grouped albums: %v", box)
	}
}
