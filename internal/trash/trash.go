// Package trash は、library から外したアルバムを作業領域の trash に置き、期間を過ぎたら消す。
//
// 1回の移動ごとに `<trash>/<日時>/<library>/<相対パス>` を作り、`<trash>/<日時>/entry.json` に中身を記録する。
package trash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
)

// 移動の理由。
const (
	ReasonDelete = "delete"
	ReasonResync = "resync"
)

const (
	entryFile = "entry.json"
	idLayout  = "20060102T150405.000000000Z"
)

var (
	// ErrNotFound は、指定した trash の項目がないことを表す。
	ErrNotFound = errors.New("trash entry not found")
	// ErrConflict は、元に戻す先にすでにディレクトリがあることを表す。
	ErrConflict = errors.New("destination already exists")
)

// Entry は trash の1項目。
type Entry struct {
	ID        string    `json:"id"`
	Library   string    `json:"library"`
	Rel       string    `json:"path"`
	Reason    string    `json:"reason"`
	DeletedAt time.Time `json:"deletedAt"`
	Bytes     int64     `json:"bytes"`
	Files     int       `json:"files"`
}

// Trash は trash のディレクトリを管理する。
type Trash struct {
	dir     string
	dirMode fs.FileMode
	now     func() time.Time

	mu    sync.Mutex
	sizes map[string]sizeInfo // ID -> 大きさ。中身は変わらないので覚えておく
}

type sizeInfo struct {
	bytes int64
	files int
}

// New は Trash を作る。dir がなければ作る。
func New(dir string, dirMode fs.FileMode) (*Trash, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	return &Trash{dir: dir, dirMode: dirMode, now: time.Now, sizes: map[string]sizeInfo{}}, nil
}

// SetClock は時刻を返す関数を差し替える。テスト用。
func (t *Trash) SetClock(now func() time.Time) { t.now = now }

// Move は libPath/rel を trash に移す。同じファイルシステムの上で rename(2) する。
func (t *Trash) Move(library, libPath, rel, reason string) (*Entry, error) {
	src := pathutil.Join(libPath, rel)
	size := measure(src)

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	id := now.Format(idLayout)
	base := filepath.Join(t.dir, id)
	for i := 1; ; i++ {
		err := os.Mkdir(base, t.dirMode)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		id = fmt.Sprintf("%s-%d", now.Format(idLayout), i)
		base = filepath.Join(t.dir, id)
	}
	e := &Entry{ID: id, Library: library, Rel: rel, Reason: reason, DeletedAt: now, Bytes: size.bytes, Files: size.files}
	if err := writeEntry(base, e); err != nil {
		_ = os.RemoveAll(base)
		return nil, err
	}
	if err := pathutil.MkdirAllNoFollow(base, pathutil.Join(library, pathutil.Parent(rel)), t.dirMode); err != nil {
		_ = os.RemoveAll(base)
		return nil, err
	}
	dst := filepath.Join(base, library, rel)
	if err := os.Rename(src, dst); err != nil {
		_ = os.RemoveAll(base)
		return nil, err
	}
	t.sizes[id] = size
	return e, nil
}

// Path は trash の項目の中のアルバムのパスを返す。
func (t *Trash) Path(e *Entry) string {
	return filepath.Join(t.dir, e.ID, e.Library, e.Rel)
}

// Undo は Move の直後に、移したものを元に戻す。再同期の途中で失敗したときに使う。
func (t *Trash) Undo(e *Entry, libPath string) error {
	if err := os.Rename(t.Path(e), pathutil.Join(libPath, e.Rel)); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sizes, e.ID)
	return os.RemoveAll(filepath.Join(t.dir, e.ID))
}

func writeEntry(base string, e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(base, entryFile), b, 0o644) //nolint:gosec // 中身は秘密ではない。
}

// List は trash の項目を新しい順に返す。
func (t *Trash) List() ([]Entry, error) {
	des, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		e, err := t.load(de.Name())
		if err != nil {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (t *Trash) load(id string) (*Entry, error) {
	if strings.ContainsAny(id, "/\x00") || id == "." || id == ".." || id == "" {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(t.dir, id, entryFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	e.ID = id
	t.mu.Lock()
	sz, ok := t.sizes[id]
	t.mu.Unlock()
	if !ok {
		sz = measure(filepath.Join(t.dir, id, e.Library))
		t.mu.Lock()
		t.sizes[id] = sz
		t.mu.Unlock()
	}
	e.Bytes, e.Files = sz.bytes, sz.files
	return &e, nil
}

// Get は1つの項目を返す。
func (t *Trash) Get(id string) (*Entry, error) { return t.load(id) }

// Restore は項目を libPath/rel に戻す。戻す先にすでにディレクトリがあれば ErrConflict を返す。
func (t *Trash) Restore(id, libPath string) (*Entry, error) {
	e, err := t.load(id)
	if err != nil {
		return nil, err
	}
	rel, err := pathutil.CleanAlbumRel(e.Rel)
	if err != nil {
		return nil, err
	}
	dst := pathutil.Join(libPath, rel)
	if _, err := os.Lstat(dst); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := pathutil.MkdirAllNoFollow(libPath, pathutil.Parent(rel), t.dirMode); err != nil {
		return nil, err
	}
	if err := os.Rename(t.Path(e), dst); err != nil {
		return nil, err
	}
	t.mu.Lock()
	delete(t.sizes, id)
	t.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(t.dir, id)); err != nil {
		return e, err
	}
	return e, nil
}

// Remove は1つの項目を消す。
func (t *Trash) Remove(id string) error {
	if _, err := t.load(id); err != nil {
		return err
	}
	t.mu.Lock()
	delete(t.sizes, id)
	t.mu.Unlock()
	return os.RemoveAll(filepath.Join(t.dir, id))
}

// Purge は retention より古い項目を消し、消した数を返す。entry.json が読めないものは、名前の日時で判断する。
func (t *Trash) Purge(retention time.Duration) (int, error) {
	des, err := os.ReadDir(t.dir)
	if err != nil {
		return 0, err
	}
	cutoff := t.now().Add(-retention)
	n := 0
	var errs []error
	for _, de := range des {
		at, ok := parseID(de.Name())
		if !ok {
			continue
		}
		if at.After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(t.dir, de.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		t.mu.Lock()
		delete(t.sizes, de.Name())
		t.mu.Unlock()
		n++
	}
	return n, errors.Join(errs...)
}

// Empty は trash を空にする。
func (t *Trash) Empty() error {
	des, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, de := range des {
		if err := os.RemoveAll(filepath.Join(t.dir, de.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	t.mu.Lock()
	t.sizes = map[string]sizeInfo{}
	t.mu.Unlock()
	return errors.Join(errs...)
}

// Bytes は trash 全体の大きさを返す。
func (t *Trash) Bytes() int64 {
	list, err := t.List()
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range list {
		n += e.Bytes
	}
	return n
}

// RunPurge は interval ごとに Purge を呼ぶ。
func (t *Trash) RunPurge(ctx context.Context, retention, interval time.Duration, onErr func(error), onPurged func(int)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if n, err := t.Purge(retention); err != nil {
			onErr(err)
		} else if n > 0 {
			onPurged(n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func parseID(name string) (time.Time, bool) {
	if i := strings.LastIndexByte(name, '-'); i > 0 {
		name = name[:i]
	}
	at, err := time.Parse(idLayout, name)
	return at, err == nil
}

// measure は path の下の通常のファイルの数と大きさを数える。シンボリックリンクはたどらない。
func measure(path string) sizeInfo {
	var s sizeInfo
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				s.bytes += fi.Size()
				s.files++
			}
		}
		return nil
	})
	return s
}
