// Package server は HTTP の API と画面、ヘルスチェック、メトリクスを提供する。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Soli0222/navidrome-curator/internal/config"
	"github.com/Soli0222/navidrome-curator/internal/index"
	"github.com/Soli0222/navidrome-curator/internal/jobs"
	"github.com/Soli0222/navidrome-curator/internal/library"
	"github.com/Soli0222/navidrome-curator/internal/metrics"
	"github.com/Soli0222/navidrome-curator/internal/pathutil"
	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/trash"
)

// Server は HTTP のハンドラーと、それが使うものをまとめる。
type Server struct {
	Config  *config.Config
	Rules   *scan.Rules
	Index   *index.Manager
	Libs    *library.Manager
	Jobs    *jobs.Manager
	Trash   *trash.Trash
	Metrics *metrics.Metrics
	Log     *slog.Logger
	Version string
	Static  fs.FS
	// BaseContext は索引の作り直しなど、リクエストより長く続く処理に使う。
	BaseContext context.Context
}

type ctxKey int

const userKey ctxKey = 0

// Handler は全体の http.Handler を返す。
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/status", s.handleStatus)
	api.HandleFunc("POST /api/index/refresh", s.handleIndexRefresh)
	api.HandleFunc("GET /api/browse", s.handleBrowse)
	api.HandleFunc("GET /api/compare", s.handleCompare)
	api.HandleFunc("GET /api/search", s.handleSearch)
	api.HandleFunc("GET /api/jobs", s.handleJobs)
	api.HandleFunc("POST /api/jobs", s.handleCreateJobs)
	api.HandleFunc("POST /api/jobs/{id}/cancel", s.handleCancelJob)
	api.HandleFunc("POST /api/jobs/{id}/retry", s.handleRetryJob)
	api.HandleFunc("POST /api/bulk/check", s.handleBulkCheck)
	api.HandleFunc("GET /api/libraries", s.handleLibraries)
	api.HandleFunc("GET /api/libraries/{name}/albums", s.handleLibraryAlbums)
	api.HandleFunc("POST /api/libraries/{name}/delete", s.handleLibraryDelete)
	api.HandleFunc("GET /api/trash", s.handleTrash)
	api.HandleFunc("POST /api/trash/empty", s.handleTrashEmpty)
	api.HandleFunc("POST /api/trash/{id}/restore", s.handleTrashRestore)
	api.HandleFunc("POST /api/trash/{id}/delete", s.handleTrashDelete)
	api.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API")
	}))
	if s.Static != nil {
		api.Handle("/", http.FileServerFS(s.Static))
	}

	// 変更を伴うリクエストは同一オリジンからだけ受け付ける（Sec-Fetch-Site と Origin で判定する）。
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected")
	}))
	protected := s.authenticate(csrf.Handler(securityHeaders(api)))

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	root.HandleFunc("GET /readyz", s.handleReady)
	root.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}))
	root.Handle("/", protected)
	return root
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate は auth.mode に従って利用者を確かめる。
func (s *Server) authenticate(next http.Handler) http.Handler {
	a := s.Config.Auth
	switch a.Mode {
	case config.AuthBasic:
		user := []byte(a.Basic.Username)
		pass := []byte(a.Basic.Password)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			// 長さの違いで早く返らないよう、両方を必ず比べる。
			uok := subtle.ConstantTimeCompare([]byte(u), user) == 1
			pok := subtle.ConstantTimeCompare([]byte(p), pass) == 1
			if !ok || !uok || !pok {
				w.Header().Set("WWW-Authenticate", `Basic realm="navidrome-curator", charset="UTF-8"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
		})
	case config.AuthHeader:
		trusted := a.Header.Trusted
		name := a.Header.Name
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !fromTrusted(r.RemoteAddr, trusted) {
				writeError(w, http.StatusForbidden, "untrusted_proxy", "connection is not from a trusted proxy")
				return
			}
			u := r.Header.Get(name)
			if u == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing "+name+" header")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
		})
	default:
		return next
	}
}

func fromTrusted(remote string, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func userOf(r *http.Request) string {
	u, _ := r.Context().Value(userKey).(string)
	return u
}

// handleReady は、すべての source が読め、workDir に書けるときだけ 200 を返す。
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	var wg sync.WaitGroup
	results := make(chan result, len(s.Config.Sources)+1)
	for _, src := range s.Config.Sources {
		wg.Go(func() {
			results <- result{"source " + src.Name, withTimeout(ctx, func() error { return canRead(src.Path) })}
		})
	}
	wg.Go(func() {
		results <- result{"workDir", withTimeout(ctx, func() error { return canWrite(s.Config.WorkDir) })}
	})
	wg.Wait()
	close(results)
	var failed []string
	for res := range results {
		if res.err != nil {
			failed = append(failed, res.name+": "+res.err.Error())
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(failed) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Join(failed, "\n") + "\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// withTimeout は f を別の goroutine で動かし、ctx が終わったら待たずに戻る。ネットワークのマウントが固まっても応答できるようにする。
func withTimeout(ctx context.Context, f func() error) error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func canRead(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func canWrite(dir string) error {
	f, err := os.CreateTemp(dir, ".readyz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// --- JSON の補助 ---

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

// encodeRef は相対パスのバイト列を、そのまま往復できる文字列にする。
// JSON の文字列は UTF-8 でないバイト列を保てないため、画面からの操作にはこれを使う。
func encodeRef(rel string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(rel))
}

// decodePath は ref（優先）か path から相対パスを取り出し、正規化する。
func decodePath(ref, path string) (string, error) {
	p := path
	if ref != "" {
		b, err := base64.RawURLEncoding.DecodeString(ref)
		if err != nil {
			return "", pathutil.ErrInvalidPath
		}
		p = string(b)
	}
	return pathutil.CleanRel(p)
}
