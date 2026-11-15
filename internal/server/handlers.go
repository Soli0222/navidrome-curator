package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/config"
	"github.com/Soli0222/navidrome-curator/internal/index"
	"github.com/Soli0222/navidrome-curator/internal/jobs"
	"github.com/Soli0222/navidrome-curator/internal/library"
	"github.com/Soli0222/navidrome-curator/internal/pathutil"
	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/trash"
)

// --- 状態 ---

type sourceInfo struct {
	index.Status
	DefaultLibraries []string `json:"defaultLibraries"`
	AllowedLibraries []string `json:"allowedLibraries"`
}

type statusResponse struct {
	Version    string         `json:"version"`
	User       string         `json:"user,omitempty"`
	AuthMode   string         `json:"authMode"`
	Sources    []sourceInfo   `json:"sources"`
	Libraries  []string       `json:"libraries"`
	Jobs       map[string]int `json:"jobs"`
	Usage      *scan.Usage    `json:"usage,omitempty"`
	TrashBytes int64          `json:"trashBytes"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := statusResponse{
		Version:  s.Version,
		User:     userOf(r),
		AuthMode: s.Config.Auth.Mode,
		Jobs:     s.Jobs.Counts(),
	}
	statuses := s.Index.Statuses()
	for i, st := range statuses {
		src := &s.Config.Sources[i]
		resp.Sources = append(resp.Sources, sourceInfo{
			Status:           st,
			DefaultLibraries: nonNil(src.DefaultLibraries),
			AllowedLibraries: s.Config.AllowedList(src),
		})
	}
	for _, l := range s.Config.Libraries {
		resp.Libraries = append(resp.Libraries, l.Name)
	}
	if u, err := scan.Statfs(s.Config.LibraryRoot); err == nil {
		resp.Usage = &u
	}
	resp.TrashBytes = s.Trash.Bytes()
	writeJSON(w, http.StatusOK, resp)
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (s *Server) handleIndexRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	if req.Source == "" {
		s.Index.RefreshAll(s.BaseContext)
	} else if !s.Index.Refresh(s.BaseContext, req.Source) {
		writeError(w, http.StatusNotFound, "unknown_source", req.Source)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// --- アルバムの状態 ---

type jobRef struct {
	ID      string `json:"id"`
	Library string `json:"library"`
	Status  string `json:"status"`
	Source  string `json:"source"`
}

// dirState はディレクトリの状態。画面はこれで「未追加」「追加済み」「コピー待ち」などを出す。
type dirState struct {
	// アルバムなら、同じ相対パスのディレクトリがある library。
	Libraries []string `json:"libraries"`
	// 待ちか実行中のジョブ。
	Jobs []jobRef `json:"jobs"`
	// 同じ相対パスを持つ、ほかの source。
	SameIn []string `json:"sameIn"`
	// アルバムでないディレクトリの、下にあるアルバムの数と、そのうちどれかの library にある数。
	// 索引ができていなければ -1。
	AlbumsTotal int `json:"albumsTotal"`
	AlbumsAdded int `json:"albumsAdded"`
}

func (s *Server) albumState(source, rel string, isAlbum bool, active map[jobs.ActiveKey]jobs.Job) dirState {
	st := dirState{Libraries: []string{}, Jobs: []jobRef{}, SameIn: []string{}, AlbumsTotal: -1, AlbumsAdded: -1}
	for _, l := range s.Config.Libraries {
		if j, ok := active[jobs.ActiveKey{Library: l.Name, Rel: rel}]; ok {
			st.Jobs = append(st.Jobs, jobRef{ID: j.ID, Library: j.Library, Status: j.Status, Source: j.Source})
		}
	}
	for _, other := range s.Index.SourcesWith(rel) {
		if other != source {
			st.SameIn = append(st.SameIn, other)
		}
	}
	if isAlbum {
		st.Libraries = nonNil(s.Libs.PresentIn(rel))
		return st
	}
	if albums, ok := s.Index.AlbumsUnder(source, rel); ok {
		st.AlbumsTotal = len(albums)
		st.AlbumsAdded = 0
		for _, a := range albums {
			if len(s.Libs.PresentIn(a)) > 0 {
				st.AlbumsAdded++
			}
		}
	}
	return st
}

// --- 閲覧 ---

type crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Ref  string `json:"ref"`
}

type dirEntry struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Ref     string   `json:"ref"`
	IsAlbum bool     `json:"isAlbum"`
	State   dirState `json:"state"`
}

type fileEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Audio bool   `json:"audio"`
}

type browseResponse struct {
	Source      string      `json:"source"`
	Path        string      `json:"path"`
	Ref         string      `json:"ref"`
	Breadcrumbs []crumb     `json:"breadcrumbs"`
	IsAlbum     bool        `json:"isAlbum"`
	State       dirState    `json:"state"`
	Dirs        []dirEntry  `json:"dirs"`
	Files       []fileEntry `json:"files"`
}

func (s *Server) sourceFromQuery(w http.ResponseWriter, r *http.Request) (*config.Source, string, bool) {
	src, ok := s.Config.SourceByName(r.URL.Query().Get("source"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_source", r.URL.Query().Get("source"))
		return nil, "", false
	}
	rel, err := decodePath(r.URL.Query().Get("ref"), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return nil, "", false
	}
	return src, rel, true
}

// readSourceDir は source の rel を読む。読み取りの結果を source_up に反映する。
func (s *Server) readSourceDir(w http.ResponseWriter, src *config.Source, rel string) ([]scan.Entry, bool) {
	fi, err := pathutil.CheckNoSymlink(src.Path, rel)
	if err == nil && !fi.IsDir() {
		err = pathutil.ErrNotDir
	}
	var entries []scan.Entry
	if err == nil {
		entries, err = s.Rules.ReadDir(pathutil.Join(src.Path, rel))
	}
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, pathutil.ErrNotDir):
			writeError(w, http.StatusNotFound, "not_found", rel)
		case errors.Is(err, pathutil.ErrSymlink):
			writeError(w, http.StatusBadRequest, "symlink", err.Error())
		default:
			s.Index.MarkRead(src.Name, false)
			s.Metrics.SourceUp.WithLabelValues(src.Name).Set(0)
			writeError(w, http.StatusBadGateway, "source_read", err.Error())
		}
		return nil, false
	}
	s.Index.MarkRead(src.Name, true)
	s.Metrics.SourceUp.WithLabelValues(src.Name).Set(1)
	return entries, true
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	src, rel, ok := s.sourceFromQuery(w, r)
	if !ok {
		return
	}
	entries, ok := s.readSourceDir(w, src, rel)
	if !ok {
		return
	}
	resp := browseResponse{
		Source: src.Name, Path: rel, Ref: encodeRef(rel),
		Breadcrumbs: []crumb{{Name: src.Name, Path: "", Ref: ""}},
		IsAlbum:     s.Rules.HasAudio(entries),
		Dirs:        []dirEntry{},
		Files:       []fileEntry{},
	}
	if rel != "" {
		acc := ""
		for part := range strings.SplitSeq(rel, "/") {
			if acc == "" {
				acc = part
			} else {
				acc += "/" + part
			}
			resp.Breadcrumbs = append(resp.Breadcrumbs, crumb{Name: part, Path: acc, Ref: encodeRef(acc)})
		}
	}
	active := s.Jobs.Active()
	if rel != "" {
		resp.State = s.albumState(src.Name, rel, resp.IsAlbum, active)
	}

	// 子のディレクトリがアルバムかは、索引にあればそれを使い、なければ読む。
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, 8)
	)
	dirs := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		child := e.Name
		if rel != "" {
			child = rel + "/" + e.Name
		}
		dirs = append(dirs, dirEntry{Name: e.Name, Path: child, Ref: encodeRef(child)})
	}
	for i := range dirs {
		d, found, known := s.Index.Lookup(src.Name, dirs[i].Path)
		if known && found {
			dirs[i].IsAlbum = d.IsAlbum
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ents, err := s.Rules.ReadDir(pathutil.Join(src.Path, dirs[i].Path))
			if err == nil {
				mu.Lock()
				dirs[i].IsAlbum = s.Rules.HasAudio(ents)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for i := range dirs {
		dirs[i].State = s.albumState(src.Name, dirs[i].Path, dirs[i].IsAlbum, active)
	}
	resp.Dirs = dirs

	// ファイルの一覧とサイズは、ここで読む。
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		fe := fileEntry{Name: e.Name, Audio: s.Rules.IsAudio(e.Name)}
		if fi, err := os.Lstat(pathutil.Join(pathutil.Join(src.Path, rel), e.Name)); err == nil {
			fe.Size = fi.Size()
		}
		resp.Files = append(resp.Files, fe)
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- source と library の比較 ---

type libraryCompare struct {
	Library string      `json:"library"`
	Stats   *scan.Stats `json:"stats,omitempty"`
	Differs bool        `json:"differs"`
	Error   string      `json:"error,omitempty"`
}

type compareResponse struct {
	Source    string           `json:"source"`
	Path      string           `json:"path"`
	Stats     scan.Stats       `json:"stats"`
	Libraries []libraryCompare `json:"libraries"`
}

// handleCompare は、source のアルバムと各 library の同じ相対パスのアルバムを、ファイル数と合計サイズで比べる。
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	src, rel, ok := s.sourceFromQuery(w, r)
	if !ok {
		return
	}
	if rel == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "empty path")
		return
	}
	if _, err := pathutil.CheckNoSymlink(src.Path, rel); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	tree, err := s.Rules.ReadTree(r.Context(), pathutil.Join(src.Path, rel))
	if err != nil {
		writeError(w, http.StatusBadGateway, "source_read", err.Error())
		return
	}
	resp := compareResponse{Source: src.Name, Path: rel, Stats: tree.Stats(), Libraries: []libraryCompare{}}
	for _, name := range s.Libs.PresentIn(rel) {
		lc := libraryCompare{Library: name}
		st, err := s.Libs.AlbumStats(r.Context(), name, rel)
		if err != nil {
			lc.Error = err.Error()
		} else {
			lc.Stats = &st
			lc.Differs = st.Files != resp.Stats.Files || st.Bytes != resp.Stats.Bytes
		}
		resp.Libraries = append(resp.Libraries, lc)
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- 検索 ---

type searchResult struct {
	Source  string   `json:"source"`
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Ref     string   `json:"ref"`
	Parent  string   `json:"parent"`
	IsAlbum bool     `json:"isAlbum"`
	State   dirState `json:"state"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	results, truncated := s.Index.Search(q, 300)
	active := s.Jobs.Active()
	out := make([]searchResult, 0, len(results))
	for _, res := range results {
		out = append(out, searchResult{
			Source: res.Source, Name: pathutil.Base(res.Rel), Path: res.Rel, Ref: encodeRef(res.Rel),
			Parent: pathutil.Parent(res.Rel), IsAlbum: res.IsAlbum,
			State: s.albumState(res.Source, res.Rel, res.IsAlbum, active),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": q, "results": out, "truncated": truncated, "indexReady": s.Index.AllReady(),
	})
}

// --- ジョブ ---

func (s *Server) handleJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobsWithRef(s.Jobs.List()), "counts": s.Jobs.Counts()})
}

type jobView struct {
	jobs.Job
	Ref string `json:"ref"`
}

func jobsWithRef(list []jobs.Job) []jobView {
	out := make([]jobView, 0, len(list))
	for _, j := range list {
		out = append(out, jobView{Job: j, Ref: encodeRef(j.Rel)})
	}
	return out
}

type jobItem struct {
	Source    string   `json:"source"`
	Ref       string   `json:"ref,omitempty"`
	Path      string   `json:"path,omitempty"`
	Libraries []string `json:"libraries,omitempty"`
}

type createJobsRequest struct {
	Items     []jobItem `json:"items"`
	Libraries []string  `json:"libraries,omitempty"`
}

type itemResult struct {
	Source  string `json:"source"`
	Path    string `json:"path"`
	Ref     string `json:"ref"`
	Library string `json:"library,omitempty"`
	JobID   string `json:"jobId,omitempty"`
	Result  string `json:"result"` // created / duplicate / error
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) handleCreateJobs(w http.ResponseWriter, r *http.Request) {
	var req createJobsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "no items")
		return
	}
	var results []itemResult
	for _, it := range req.Items {
		results = append(results, s.createJobsFor(r.Context(), it, req.Libraries)...)
	}
	created := 0
	for _, res := range results {
		if res.Result == "created" {
			created++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "created": created})
}

// createJobsFor は1つの依頼からジョブを作る。アルバムでないディレクトリなら、その下のアルバムごとに作る。
func (s *Server) createJobsFor(ctx context.Context, it jobItem, libs []string) []itemResult {
	errResult := func(path, code, msg string) []itemResult {
		return []itemResult{{Source: it.Source, Path: path, Ref: encodeRef(path), Result: "error", Error: code, Message: msg}}
	}
	src, ok := s.Config.SourceByName(it.Source)
	if !ok {
		return errResult(it.Path, "unknown_source", it.Source)
	}
	rel, err := decodePath(it.Ref, it.Path)
	if err != nil || rel == "" {
		return errResult(it.Path, "invalid_path", "invalid path")
	}
	targets := it.Libraries
	if len(targets) == 0 {
		targets = libs
	}
	if len(targets) == 0 {
		targets = src.DefaultLibraries
	}
	if len(targets) == 0 {
		return errResult(rel, "no_library", "no library selected and the source has no defaultLibraries")
	}
	for _, l := range targets {
		if _, ok := s.Config.LibraryByName(l); !ok {
			return errResult(rel, "unknown_library", l)
		}
		if !src.Allowed(l) {
			return errResult(rel, "library_not_allowed", l)
		}
	}
	albums, code, err := s.expandAlbums(ctx, src, rel)
	if err != nil {
		return errResult(rel, code, err.Error())
	}
	if len(albums) == 0 {
		return errResult(rel, "no_album", "no album under this directory")
	}
	var out []itemResult
	for _, a := range albums {
		for _, l := range dedupe(targets) {
			res := itemResult{Source: src.Name, Path: a, Ref: encodeRef(a), Library: l}
			j, err := s.Jobs.Enqueue(src.Name, a, l)
			switch {
			case err == nil:
				res.Result, res.JobID = "created", j.ID
			case errors.Is(err, jobs.ErrDuplicate):
				res.Result, res.JobID = "duplicate", j.ID
			default:
				res.Result, res.Error, res.Message = "error", "internal", err.Error()
			}
			out = append(out, res)
		}
	}
	return out
}

func dedupe(v []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range v {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// expandAlbums は rel の下のアルバムを返す。rel がアルバムなら rel だけを返す。
// rel がアルバムかは、索引が古いことがあるので必ず読んで確かめる。下のアルバムは索引にあれば索引を使う。
func (s *Server) expandAlbums(ctx context.Context, src *config.Source, rel string) ([]string, string, error) {
	fi, err := pathutil.CheckNoSymlink(src.Path, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "not_found", err
		}
		if errors.Is(err, pathutil.ErrSymlink) {
			return nil, "symlink", err
		}
		return nil, "source_read", err
	}
	if !fi.IsDir() {
		return nil, "not_found", pathutil.ErrNotDir
	}
	entries, err := s.Rules.ReadDir(pathutil.Join(src.Path, rel))
	if err != nil {
		return nil, "source_read", err
	}
	if s.Rules.HasAudio(entries) {
		return []string{rel}, "", nil
	}
	if _, found, known := s.Index.Lookup(src.Name, rel); known && found {
		if albums, ok := s.Index.AlbumsUnder(src.Name, rel); ok {
			return albums, "", nil
		}
	}
	albums, err := s.Rules.FindAlbums(ctx, src.Path, rel)
	if err != nil {
		return nil, "source_read", err
	}
	return albums, "", nil
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	switch err := s.Jobs.Cancel(r.PathValue("id")); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	}
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.Jobs.Retry(r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, j)
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, jobs.ErrDuplicate):
		writeError(w, http.StatusConflict, "duplicate", err.Error())
	default:
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	}
}

// --- 一括追加 ---

type bulkRow struct {
	Line      int      `json:"line"`
	Text      string   `json:"text"`
	Source    string   `json:"source"`
	Path      string   `json:"path"`
	Ref       string   `json:"ref"`
	Libraries []string `json:"libraries"`
	Status    string   `json:"status"` // found / not_found / invalid
	Error     string   `json:"error,omitempty"`
	Message   string   `json:"message,omitempty"`
	Albums    int      `json:"albums"`
	Present   []string `json:"present"`
}

// handleBulkCheck は、貼り付けた一覧の各行が source に見つかるかを調べる。ジョブは作らない。
func (s *Server) handleBulkCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rows := []bulkRow{}
	lines := strings.Split(req.Text, "\n")
	if len(lines) > 20000 {
		writeError(w, http.StatusBadRequest, "too_many_lines", "up to 20000 lines")
		return
	}
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, s.checkBulkLine(r.Context(), i+1, line))
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "counts": counts})
}

func (s *Server) checkBulkLine(ctx context.Context, n int, line string) bulkRow {
	row := bulkRow{Line: n, Text: line, Libraries: []string{}, Present: []string{}, Status: "invalid"}
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || len(fields) > 3 {
		row.Error = "format"
		return row
	}
	row.Source, row.Path = fields[0], fields[1]
	src, ok := s.Config.SourceByName(row.Source)
	if !ok {
		row.Error = "unknown_source"
		return row
	}
	rel, err := pathutil.CleanAlbumRel(row.Path)
	if err != nil {
		row.Error = "invalid_path"
		return row
	}
	row.Path, row.Ref = rel, encodeRef(rel)
	if len(fields) == 3 && strings.TrimSpace(fields[2]) != "" {
		for l := range strings.SplitSeq(fields[2], ",") {
			if l = strings.TrimSpace(l); l != "" {
				row.Libraries = append(row.Libraries, l)
			}
		}
	} else {
		row.Libraries = nonNil(append([]string(nil), src.DefaultLibraries...))
	}
	if len(row.Libraries) == 0 {
		row.Error = "no_library"
		return row
	}
	for _, l := range row.Libraries {
		if _, ok := s.Config.LibraryByName(l); !ok {
			row.Error, row.Message = "unknown_library", l
			return row
		}
		if !src.Allowed(l) {
			row.Error, row.Message = "library_not_allowed", l
			return row
		}
	}
	albums, code, err := s.expandAlbums(ctx, src, rel)
	if err != nil {
		if code == "not_found" {
			row.Status = "not_found"
			return row
		}
		row.Error, row.Message = code, err.Error()
		return row
	}
	if len(albums) == 0 {
		row.Status, row.Error = "not_found", "no_album"
		return row
	}
	row.Status = "found"
	row.Albums = len(albums)
	if len(albums) == 1 && albums[0] == rel {
		row.Present = nonNil(s.Libs.PresentIn(rel))
	}
	return row
}

// --- library ---

type libraryView struct {
	Name      string    `json:"name"`
	Albums    int       `json:"albums"`
	Bytes     int64     `json:"bytes"`
	ScannedAt time.Time `json:"scannedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

func (s *Server) handleLibraries(w http.ResponseWriter, _ *http.Request) {
	sums := s.Libs.Summaries()
	out := make([]libraryView, 0, len(s.Config.Libraries))
	for _, l := range s.Config.Libraries {
		v := libraryView{Name: l.Name}
		if sum, ok := sums[l.Name]; ok {
			v.Albums, v.Bytes, v.ScannedAt = len(sum.Albums), sum.Bytes, sum.ScannedAt
			if sum.Err != nil {
				v.Error = sum.Err.Error()
			}
		}
		out = append(out, v)
	}
	resp := map[string]any{"libraries": out, "trashBytes": s.Trash.Bytes()}
	if u, err := scan.Statfs(s.Config.LibraryRoot); err == nil {
		resp["usage"] = u
	}
	writeJSON(w, http.StatusOK, resp)
}

type libraryAlbum struct {
	Path    string     `json:"path"`
	Ref     string     `json:"ref"`
	Stats   scan.Stats `json:"stats"`
	Sources []string   `json:"sources"`
	Orphan  bool       `json:"orphan"`
	Job     *jobRef    `json:"job,omitempty"`
}

func (s *Server) handleLibraryAlbums(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.Libs.Get(name); !ok {
		writeError(w, http.StatusNotFound, "unknown_library", name)
		return
	}
	var sum *library.Summary
	if r.URL.Query().Get("refresh") == "1" {
		sum = s.Libs.Scan(r.Context(), name)
	} else if cached, ok := s.Libs.Summaries()[name]; ok {
		sum = cached
	} else {
		sum = s.Libs.Scan(r.Context(), name)
	}
	if sum.Err != nil {
		writeError(w, http.StatusInternalServerError, "library_read", sum.Err.Error())
		return
	}
	active := s.Jobs.Active()
	albums := make([]libraryAlbum, 0, len(sum.Albums))
	for _, a := range sum.Albums {
		la := libraryAlbum{Path: a.Rel, Ref: encodeRef(a.Rel), Stats: a.Stats, Sources: s.sourcesWith(a.Rel)}
		la.Orphan = len(la.Sources) == 0
		if j, ok := active[jobs.ActiveKey{Library: name, Rel: a.Rel}]; ok {
			la.Job = &jobRef{ID: j.ID, Library: j.Library, Status: j.Status, Source: j.Source}
		}
		albums = append(albums, la)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"library": name, "albums": albums, "bytes": sum.Bytes, "scannedAt": sum.ScannedAt,
	})
}

// sourcesWith は rel のディレクトリを持つ source を返す。索引ができていない source は直接確かめる。
func (s *Server) sourcesWith(rel string) []string {
	out := []string{}
	for _, src := range s.Config.Sources {
		if _, found, known := s.Index.Lookup(src.Name, rel); known {
			if found {
				out = append(out, src.Name)
			}
			continue
		}
		if fi, err := pathutil.CheckNoSymlink(src.Path, rel); err == nil && fi.IsDir() {
			out = append(out, src.Name)
		}
	}
	return out
}

type deleteRequest struct {
	Items []struct {
		Ref  string `json:"ref,omitempty"`
		Path string `json:"path,omitempty"`
	} `json:"items"`
}

func (s *Server) handleLibraryDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.Libs.Get(name); !ok {
		writeError(w, http.StatusNotFound, "unknown_library", name)
		return
	}
	var req deleteRequest
	if !readJSON(w, r, &req) {
		return
	}
	active := s.Jobs.Active()
	type result struct {
		Path    string `json:"path"`
		Ref     string `json:"ref"`
		Result  string `json:"result"`
		TrashID string `json:"trashId,omitempty"`
		Error   string `json:"error,omitempty"`
		Message string `json:"message,omitempty"`
	}
	var results []result
	for _, it := range req.Items {
		rel, err := decodePath(it.Ref, it.Path)
		if err != nil || rel == "" {
			results = append(results, result{Path: it.Path, Result: "error", Error: "invalid_path"})
			continue
		}
		res := result{Path: rel, Ref: encodeRef(rel)}
		if _, busy := active[jobs.ActiveKey{Library: name, Rel: rel}]; busy {
			res.Result, res.Error = "error", "job_active"
			results = append(results, res)
			continue
		}
		e, err := s.Libs.Delete(name, rel)
		if err != nil {
			res.Result, res.Message = "error", err.Error()
			res.Error = "delete_failed"
			if errors.Is(err, library.ErrNotFound) {
				res.Error = "not_found"
			}
			results = append(results, res)
			continue
		}
		s.Log.Info("album removed from library", "library", name, "path", rel, "trash", e.ID, "bytes", e.Bytes, "user", userOf(r))
		res.Result, res.TrashID = "deleted", e.ID
		results = append(results, res)
	}
	s.refreshTrashMetric()
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// --- trash ---

type trashView struct {
	trash.Entry
	Ref       string    `json:"ref"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Server) handleTrash(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Trash.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "trash_read", err.Error())
		return
	}
	retention := s.Config.Trash.Retention.D()
	out := make([]trashView, 0, len(list))
	var total int64
	for _, e := range list {
		out = append(out, trashView{Entry: e, Ref: encodeRef(e.Rel), ExpiresAt: e.DeletedAt.Add(retention)})
		total += e.Bytes
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "bytes": total, "retentionSeconds": int64(retention.Seconds())})
}

func (s *Server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e, err := s.Trash.Get(id); err == nil {
		if _, busy := s.Jobs.Active()[jobs.ActiveKey{Library: e.Library, Rel: e.Rel}]; busy {
			writeError(w, http.StatusConflict, "job_active", e.Rel)
			return
		}
	}
	e, err := s.Libs.Restore(id)
	switch {
	case err == nil:
		s.Log.Info("album restored from trash", "library", e.Library, "path", e.Rel, "trash", id, "user", userOf(r))
		s.refreshTrashMetric()
		writeJSON(w, http.StatusOK, e)
	case errors.Is(err, trash.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", id)
	case errors.Is(err, trash.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "restore_failed", err.Error())
	}
}

func (s *Server) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch err := s.Trash.Remove(id); {
	case err == nil:
		s.refreshTrashMetric()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, trash.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", id)
	default:
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
	}
}

func (s *Server) handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	if err := s.Trash.Empty(); err != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	s.Log.Info("trash emptied", "user", userOf(r))
	s.refreshTrashMetric()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) refreshTrashMetric() {
	s.Metrics.TrashBytes.Set(float64(s.Trash.Bytes()))
	if u, err := scan.Statfs(s.Config.LibraryRoot); err == nil {
		s.Metrics.FilesystemFree.Set(float64(u.Free))
	}
}
