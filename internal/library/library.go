// Package library は library のディレクトリを読み、書き込みをまとめる。
//
// どのアルバムが library にあるかは、library のファイルを正とする。アプリ独自の台帳は持たない。
// library への書き込み（アルバムの配置、削除、元に戻す）は、すべてこのパッケージを通し、1つずつ行う。
package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/trash"
)

var (
	// ErrNotFound は、library にアルバムがないことを表す。
	ErrNotFound = errors.New("album not found in library")
	// ErrUnknownLibrary は、設定にない library の名前を表す。
	ErrUnknownLibrary = errors.New("unknown library")
)

// Library はコピー先の1つ。
type Library struct {
	Name string
	Path string
}

// Album は library の中のアルバム。
type Album struct {
	Rel   string
	Stats scan.Stats
}

// Summary は1つの library の集計。
type Summary struct {
	Name      string
	Albums    []Album
	Bytes     int64
	ScannedAt time.Time
	Err       error
}

// Manager は library をまとめて扱う。
type Manager struct {
	libs    []Library
	byName  map[string]Library
	rules   *scan.Rules
	trash   *trash.Trash
	dirMode fs.FileMode
	log     *slog.Logger

	writeMu sync.Mutex // library への書き込みを1つずつにする

	cacheMu  sync.Mutex
	cache    map[string]*Summary
	dirty    chan struct{}
	onChange func(map[string]*Summary)
}

// New は Manager を作る。
func New(libs []Library, rules *scan.Rules, tr *trash.Trash, dirMode fs.FileMode, log *slog.Logger) *Manager {
	m := &Manager{
		libs:    libs,
		byName:  map[string]Library{},
		rules:   rules,
		trash:   tr,
		dirMode: dirMode,
		log:     log,
		cache:   map[string]*Summary{},
		dirty:   make(chan struct{}, 1),
	}
	for _, l := range libs {
		m.byName[l.Name] = l
	}
	return m
}

// OnScanned は集計を作り直したときに呼ぶ関数を設定する。メトリクスに使う。
func (m *Manager) OnScanned(f func(map[string]*Summary)) { m.onChange = f }

// Libraries は library の一覧を返す。
func (m *Manager) Libraries() []Library { return m.libs }

// Get は名前で library を返す。
func (m *Manager) Get(name string) (Library, bool) {
	l, ok := m.byName[name]
	return l, ok
}

// Present は、library に rel のディレクトリがあるかを返す。途中にシンボリックリンクがあれば、ないものとする。
func (m *Manager) Present(name, rel string) bool {
	l, ok := m.byName[name]
	if !ok {
		return false
	}
	fi, err := pathutil.CheckNoSymlink(l.Path, rel)
	return err == nil && fi.IsDir()
}

// PresentIn は、rel のディレクトリを持つ library の名前を返す。
func (m *Manager) PresentIn(rel string) []string {
	var out []string
	for _, l := range m.libs {
		if m.Present(l.Name, rel) {
			out = append(out, l.Name)
		}
	}
	return out
}

// AlbumStats は library の中のアルバムを集計する。
func (m *Manager) AlbumStats(ctx context.Context, name, rel string) (scan.Stats, error) {
	l, ok := m.byName[name]
	if !ok {
		return scan.Stats{}, ErrUnknownLibrary
	}
	if _, err := pathutil.CheckNoSymlink(l.Path, rel); err != nil {
		return scan.Stats{}, err
	}
	t, err := m.rules.ReadTree(ctx, pathutil.Join(l.Path, rel))
	if err != nil {
		return scan.Stats{}, err
	}
	return t.Stats(), nil
}

// Place は staged のディレクトリを library の rel に rename(2) で移す。
// すでにアルバムがあれば、古い版を trash に移してから置き換える（再同期）。古い版の trash の項目を返す。
func (m *Manager) Place(name, rel, staged string) (old *trash.Entry, err error) {
	l, ok := m.byName[name]
	if !ok {
		return nil, ErrUnknownLibrary
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	defer m.Invalidate()

	if err := pathutil.MkdirAllNoFollow(l.Path, pathutil.Parent(rel), m.dirMode); err != nil {
		return nil, err
	}
	dst := pathutil.Join(l.Path, rel)
	fi, err := os.Lstat(dst)
	switch {
	case err == nil:
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return nil, fmt.Errorf("destination %s exists and is not a directory", dst)
		}
		old, err = m.trash.Move(name, l.Path, rel, trash.ReasonResync)
		if err != nil {
			return nil, fmt.Errorf("move the old version to trash: %w", err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	if err := os.Rename(staged, dst); err != nil {
		if old != nil {
			if uerr := m.trash.Undo(old, l.Path); uerr != nil {
				m.log.Error("failed to restore the old version after a failed resync", "library", name, "path", rel, "error", uerr)
			}
		}
		return nil, err
	}
	return old, nil
}

// Delete は library の rel を trash に移し、空になった親のディレクトリを消す。
func (m *Manager) Delete(name, rel string) (*trash.Entry, error) {
	l, ok := m.byName[name]
	if !ok {
		return nil, ErrUnknownLibrary
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	defer m.Invalidate()

	fi, err := pathutil.CheckNoSymlink(l.Path, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, ErrNotFound
	}
	e, err := m.trash.Move(name, l.Path, rel, trash.ReasonDelete)
	if err != nil {
		return nil, err
	}
	pathutil.RemoveEmptyParents(l.Path, rel)
	return e, nil
}

// Restore は trash の項目を元の library に戻す。
func (m *Manager) Restore(id string) (*trash.Entry, error) {
	e, err := m.trash.Get(id)
	if err != nil {
		return nil, err
	}
	l, ok := m.byName[e.Library]
	if !ok {
		return nil, ErrUnknownLibrary
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	defer m.Invalidate()
	return m.trash.Restore(id, l.Path)
}

// Invalidate は集計を作り直すよう求める。すぐには作り直さない。
func (m *Manager) Invalidate() {
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

// Run は起動時と interval ごと、Invalidate のあと少し待ってから集計を作り直す。
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.rescan(ctx)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-m.dirty:
			// 続けて変わることが多いので、少しまとめる。
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		m.rescan(ctx)
	}
}

func (m *Manager) rescan(ctx context.Context) {
	out := map[string]*Summary{}
	for _, l := range m.libs {
		s := m.Scan(ctx, l.Name)
		out[l.Name] = s
	}
	m.cacheMu.Lock()
	m.cache = out
	m.cacheMu.Unlock()
	if m.onChange != nil {
		m.onChange(out)
	}
}

// Summaries は最後に作った集計を返す。
func (m *Manager) Summaries() map[string]*Summary {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	return m.cache
}

// Scan は library を読み、アルバムの一覧と集計を作る。
func (m *Manager) Scan(ctx context.Context, name string) *Summary {
	s := &Summary{Name: name, ScannedAt: time.Now()}
	l, ok := m.byName[name]
	if !ok {
		s.Err = ErrUnknownLibrary
		return s
	}
	rels, err := m.rules.FindAlbums(ctx, l.Path, "")
	if err != nil {
		s.Err = err
		return s
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if rel == "" {
			// library の直下に曲が置かれている。アルバムとしては扱わない。
			continue
		}
		t, err := m.rules.ReadTree(ctx, pathutil.Join(l.Path, rel))
		if err != nil {
			m.log.Warn("library: read album failed", "library", name, "path", rel, "error", err)
			continue
		}
		st := t.Stats()
		s.Albums = append(s.Albums, Album{Rel: rel, Stats: st})
		s.Bytes += st.Bytes
	}
	return s
}
