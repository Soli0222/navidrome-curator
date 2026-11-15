// Package index は source のディレクトリの索引をメモリに持つ。
//
// 索引はディレクトリの相対パスと、それがアルバムか（音声ファイルを直接含むか）だけを持つ。
// ファイルのサイズは持たない。アルバムを開いたときに読む。
package index

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
	"github.com/Soli0222/navidrome-curator/internal/scan"
)

// Source は索引を作る source。
type Source struct {
	Name string
	Path string
}

// Dir は索引の1要素。
type Dir struct {
	Rel     string
	IsAlbum bool
}

type snapshot struct {
	dirs    []Dir          // Rel の順
	byRel   map[string]int // Rel -> dirs の位置
	albums  []string       // アルバムの Rel。順に並べる
	lower   []string       // dirs と同じ順の、小文字にした Rel
	builtAt time.Time
}

type sourceIndex struct {
	src Source

	mu        sync.RWMutex
	snap      *snapshot
	building  bool
	scanned   atomic.Int64
	lastErr   string
	lastOK    time.Time
	lastTry   time.Time
	up        bool
	startedAt time.Time
}

// Status は1つの source の索引の状態。
type Status struct {
	Source      string    `json:"source"`
	Building    bool      `json:"building"`
	Scanned     int64     `json:"scanned"`
	Dirs        int       `json:"dirs"`
	Albums      int       `json:"albums"`
	LastSuccess time.Time `json:"lastSuccess"`
	LastError   string    `json:"lastError,omitempty"`
	Up          bool      `json:"up"`
	Ready       bool      `json:"ready"`
}

// Hooks は索引の状態が変わったときに呼ぶ関数。メトリクスに使う。
type Hooks struct {
	OnBuilt func(source string, ok bool, at time.Time)
}

// Manager はすべての source の索引を持つ。
type Manager struct {
	rules   *scan.Rules
	log     *slog.Logger
	hooks   Hooks
	sources []*sourceIndex
	byName  map[string]*sourceIndex
	workers int
	wg      sync.WaitGroup
}

// New は Manager を作る。索引はまだ作らない。
func New(sources []Source, rules *scan.Rules, log *slog.Logger, hooks Hooks) *Manager {
	m := &Manager{rules: rules, log: log, hooks: hooks, byName: map[string]*sourceIndex{}, workers: 8}
	for _, s := range sources {
		si := &sourceIndex{src: s}
		m.sources = append(m.sources, si)
		m.byName[s.Name] = si
	}
	return m
}

// Run は起動時と interval ごとに索引を作り直す。ctx が終わるまで戻らない。
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.RefreshAll(ctx)
	if interval <= 0 {
		<-ctx.Done()
		m.wg.Wait()
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.wg.Wait()
			return
		case <-t.C:
			m.RefreshAll(ctx)
		}
	}
}

// RefreshAll はすべての source の索引の作り直しを始める。作成中のものは飛ばす。
func (m *Manager) RefreshAll(ctx context.Context) {
	for _, si := range m.sources {
		m.start(ctx, si)
	}
}

// Refresh は1つの source の索引の作り直しを始める。source がなければ false を返す。
func (m *Manager) Refresh(ctx context.Context, name string) bool {
	si, ok := m.byName[name]
	if !ok {
		return false
	}
	m.start(ctx, si)
	return true
}

// Wait は作成中の索引がすべて終わるのを待つ。テスト用。
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) start(ctx context.Context, si *sourceIndex) {
	si.mu.Lock()
	if si.building {
		si.mu.Unlock()
		return
	}
	si.building = true
	si.startedAt = time.Now()
	si.scanned.Store(0)
	si.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		snap, err := m.build(ctx, si)
		now := time.Now()
		si.mu.Lock()
		si.building = false
		si.lastTry = now
		if err != nil {
			si.lastErr = err.Error()
			si.up = false
		} else {
			si.snap = snap
			si.lastErr = ""
			si.lastOK = now
			si.up = true
		}
		si.mu.Unlock()
		if err != nil {
			m.log.Error("index build failed", "source", si.src.Name, "error", err)
		} else {
			m.log.Info("index built", "source", si.src.Name, "dirs", len(snap.dirs), "albums", len(snap.albums),
				"duration_ms", now.Sub(si.startedAt).Milliseconds())
		}
		if m.hooks.OnBuilt != nil {
			m.hooks.OnBuilt(si.src.Name, err == nil, now)
		}
	}()
}

func (m *Manager) build(ctx context.Context, si *sourceIndex) (*snapshot, error) {
	// ルートが読めなければ失敗にする。下の階層の読み取りの失敗は記録して続ける。
	rootEntries, err := m.rules.ReadDir(si.src.Path)
	if err != nil {
		return nil, err
	}
	var (
		mu     sync.Mutex
		dirs   []Dir
		failed atomic.Int64
		wg     sync.WaitGroup
		sem    = make(chan struct{}, m.workers)
	)
	var visit func(rel string, entries []scan.Entry)
	visit = func(rel string, entries []scan.Entry) {
		si.scanned.Add(1)
		if rel != "" {
			mu.Lock()
			dirs = append(dirs, Dir{Rel: rel, IsAlbum: m.rules.HasAudio(entries)})
			mu.Unlock()
		}
		for _, e := range entries {
			if !e.IsDir || ctx.Err() != nil {
				continue
			}
			child := e.Name
			if rel != "" {
				child = rel + "/" + e.Name
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				ents, err := m.rules.ReadDir(pathutil.Join(si.src.Path, child))
				<-sem
				if err != nil {
					failed.Add(1)
					m.log.Warn("index: read dir failed", "source", si.src.Name, "path", child, "error", err)
					return
				}
				visit(child, ents)
			}()
		}
	}
	visit("", rootEntries)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Rel < dirs[j].Rel })
	s := &snapshot{dirs: dirs, byRel: make(map[string]int, len(dirs)), lower: make([]string, len(dirs)), builtAt: time.Now()}
	for i, d := range dirs {
		s.byRel[d.Rel] = i
		s.lower[i] = strings.ToLower(d.Rel)
		if d.IsAlbum {
			s.albums = append(s.albums, d.Rel)
		}
	}
	if n := failed.Load(); n > 0 {
		m.log.Warn("index built with unreadable directories", "source", si.src.Name, "failed", n)
	}
	return s, nil
}

func (si *sourceIndex) current() *snapshot {
	si.mu.RLock()
	defer si.mu.RUnlock()
	return si.snap
}

// Statuses はすべての source の索引の状態を返す。
func (m *Manager) Statuses() []Status {
	out := make([]Status, 0, len(m.sources))
	for _, si := range m.sources {
		si.mu.RLock()
		st := Status{
			Source:      si.src.Name,
			Building:    si.building,
			Scanned:     si.scanned.Load(),
			LastSuccess: si.lastOK,
			LastError:   si.lastErr,
			Up:          si.up,
			Ready:       si.snap != nil,
		}
		if si.snap != nil {
			st.Dirs = len(si.snap.dirs)
			st.Albums = len(si.snap.albums)
		}
		si.mu.RUnlock()
		out = append(out, st)
	}
	return out
}

// MarkRead は source の読み取りの結果を記録する。source_up に反映する。
func (m *Manager) MarkRead(name string, ok bool) {
	si, found := m.byName[name]
	if !found {
		return
	}
	si.mu.Lock()
	si.up = ok
	si.mu.Unlock()
}

// Up は source が直近の索引作成か読み取りで成功したかを返す。
func (m *Manager) Up(name string) bool {
	si, ok := m.byName[name]
	if !ok {
		return false
	}
	si.mu.RLock()
	defer si.mu.RUnlock()
	return si.up
}

// Ready は source の索引が一度でもできたかを返す。
func (m *Manager) Ready(name string) bool {
	si, ok := m.byName[name]
	return ok && si.current() != nil
}

// Lookup は source の rel が索引にあるかを返す。索引がまだなければ known が false。
func (m *Manager) Lookup(source, rel string) (d Dir, found, known bool) {
	si, ok := m.byName[source]
	if !ok {
		return Dir{}, false, true
	}
	s := si.current()
	if s == nil {
		return Dir{}, false, false
	}
	i, ok := s.byRel[rel]
	if !ok {
		return Dir{}, false, true
	}
	return s.dirs[i], true, true
}

// SourcesWith は、rel のディレクトリを持つ source の名前を返す。索引ができていない source は除く。
func (m *Manager) SourcesWith(rel string) []string {
	var out []string
	for _, si := range m.sources {
		s := si.current()
		if s == nil {
			continue
		}
		if _, ok := s.byRel[rel]; ok {
			out = append(out, si.src.Name)
		}
	}
	return out
}

// AllReady は、すべての source の索引ができているかを返す。
func (m *Manager) AllReady() bool {
	for _, si := range m.sources {
		if si.current() == nil {
			return false
		}
	}
	return true
}

// AlbumsUnder は rel の下（rel 自身を含む）のアルバムを返す。索引がなければ ok が false。
func (m *Manager) AlbumsUnder(source, rel string) (albums []string, ok bool) {
	si, found := m.byName[source]
	if !found {
		return nil, false
	}
	s := si.current()
	if s == nil {
		return nil, false
	}
	if i, ok := s.byRel[rel]; ok && s.dirs[i].IsAlbum {
		return []string{rel}, true
	}
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	lo := sort.SearchStrings(s.albums, prefix)
	for i := lo; i < len(s.albums) && strings.HasPrefix(s.albums[i], prefix); i++ {
		a := s.albums[i]
		// アルバムの中のアルバムは、外側のアルバムに含める。
		if s.insideAlbum(a) {
			continue
		}
		albums = append(albums, a)
	}
	return albums, true
}

// Result は検索の1件。
type Result struct {
	Source  string
	Rel     string
	IsAlbum bool
}

// Search は、空白で区切った語がすべて相対パスに含まれるディレクトリを探す。大文字小文字を区別しない。
// アルバムと、名前（最後の要素）に語を含むディレクトリを返す。
func (m *Manager) Search(query string, limit int) (results []Result, truncated bool) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil, false
	}
	for _, si := range m.sources {
		s := si.current()
		if s == nil {
			continue
		}
		for i, lower := range s.lower {
			if !matchAll(lower, terms) {
				continue
			}
			d := s.dirs[i]
			// アルバムでないディレクトリは、名前そのものが語に当たるもの（アーティストなど）だけを出す。
			if !d.IsAlbum && !matchAny(strings.ToLower(pathutil.Base(d.Rel)), terms) {
				continue
			}
			// アルバムの中のディレクトリ（Disc 1 など）は出さない。
			if d.IsAlbum && s.insideAlbum(d.Rel) {
				continue
			}
			if len(results) >= limit {
				return results, true
			}
			results = append(results, Result{Source: si.src.Name, Rel: d.Rel, IsAlbum: d.IsAlbum})
		}
	}
	return results, false
}

func (s *snapshot) insideAlbum(rel string) bool {
	for p := pathutil.Parent(rel); p != ""; p = pathutil.Parent(p) {
		if i, ok := s.byRel[p]; ok && s.dirs[i].IsAlbum {
			return true
		}
	}
	return false
}

func matchAll(s string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(s, t) {
			return false
		}
	}
	return true
}

func matchAny(s string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// AlbumCounts は rel の下のアルバムの数を返す。索引がなければ ok が false。
func (m *Manager) AlbumCounts(source, rel string) (int, bool) {
	a, ok := m.AlbumsUnder(source, rel)
	return len(a), ok
}
