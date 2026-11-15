// Package scan は、除外パターンと音声ファイルの判定、ディレクトリの走査をまとめる。
//
// シンボリックリンクはたどらず、結果にも含めない。
package scan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
)

// Rules は、どのファイルを曲とみなし、どのファイルとディレクトリを無視するかを決める。
type Rules struct {
	audio          map[string]struct{}
	exclude        []string
	excludeDotDirs bool
}

// NewRules は Rules を作る。拡張子は先頭のドットの有無を問わない。比較は大文字小文字を区別しない。
func NewRules(audioExtensions, exclude []string, excludeDotDirs bool) (*Rules, error) {
	r := &Rules{audio: map[string]struct{}{}, excludeDotDirs: excludeDotDirs}
	for _, e := range audioExtensions {
		e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e != "" {
			r.audio[e] = struct{}{}
		}
	}
	for _, p := range exclude {
		p = strings.ToLower(p)
		if _, err := path.Match(p, ""); err != nil {
			return nil, err
		}
		r.exclude = append(r.exclude, p)
	}
	return r, nil
}

// Excluded は、名前が除外パターンに当たるかを返す。
func (r *Rules) Excluded(name string, isDir bool) bool {
	if isDir && r.excludeDotDirs && strings.HasPrefix(name, ".") {
		return true
	}
	lower := strings.ToLower(name)
	for _, p := range r.exclude {
		if ok, _ := path.Match(p, lower); ok {
			return true
		}
	}
	return false
}

// IsAudio は、除外されていない、音声の拡張子を持つファイル名かを返す。
func (r *Rules) IsAudio(name string) bool {
	if r.Excluded(name, false) {
		return false
	}
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return false
	}
	_, ok := r.audio[strings.ToLower(name[i+1:])]
	return ok
}

// Entry はディレクトリの中の1要素。シンボリックリンクと除外したものは含めない。
type Entry struct {
	Name  string
	IsDir bool
}

// ReadDir は dir の要素を名前順に返す。除外パターンに当たるものと、シンボリックリンク、
// 通常のファイルとディレクトリ以外は除く。
func (r *Rules) ReadDir(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		t := de.Type()
		if t&fs.ModeSymlink != 0 {
			continue
		}
		isDir := t.IsDir()
		if !isDir && !t.IsRegular() {
			continue
		}
		if r.Excluded(de.Name(), isDir) {
			continue
		}
		out = append(out, Entry{Name: de.Name(), IsDir: isDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// HasAudio は、要素の中に曲があるか（アルバムか）を返す。
func (r *Rules) HasAudio(entries []Entry) bool {
	for _, e := range entries {
		if !e.IsDir && r.IsAudio(e.Name) {
			return true
		}
	}
	return false
}

// File はアルバムの中の1ファイル。Rel はアルバムのディレクトリからの相対パス。
type File struct {
	Rel     string
	Size    int64
	ModTime int64 // UnixNano
	Audio   bool
}

// Dir はアルバムの中の1ディレクトリ。Rel が空文字列ならアルバム自身。
type Dir struct {
	Rel     string
	ModTime int64
}

// Tree はアルバムのディレクトリをサブディレクトリまでたどった結果。
type Tree struct {
	Dirs  []Dir
	Files []File
}

// Stats はアルバムの集計。
type Stats struct {
	Files  int   `json:"files"`
	Tracks int   `json:"tracks"`
	Bytes  int64 `json:"bytes"`
}

// Stats は Tree を集計する。
func (t *Tree) Stats() Stats {
	var s Stats
	for _, f := range t.Files {
		s.Files++
		s.Bytes += f.Size
		if f.Audio {
			s.Tracks++
		}
	}
	return s
}

// ReadTree は root の下をすべてたどり、コピーの対象になるファイルとディレクトリを返す。
func (r *Rules) ReadTree(ctx context.Context, root string) (*Tree, error) {
	fi, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, pathutil.ErrNotDir
	}
	t := &Tree{Dirs: []Dir{{Rel: "", ModTime: fi.ModTime().UnixNano()}}}
	if err := r.readTree(ctx, root, "", t); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *Rules) readTree(ctx context.Context, root, rel string, t *Tree) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := pathutil.Join(root, rel)
	entries, err := r.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		childRel := e.Name
		if rel != "" {
			childRel = rel + "/" + e.Name
		}
		fi, err := os.Lstat(pathutil.Join(root, childRel))
		if err != nil {
			return err
		}
		// ReadDir の時点から変わっていたら（シンボリックリンクに置き換わったなど）無視する。
		if fi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if e.IsDir {
			if !fi.IsDir() {
				continue
			}
			t.Dirs = append(t.Dirs, Dir{Rel: childRel, ModTime: fi.ModTime().UnixNano()})
			if err := r.readTree(ctx, root, childRel, t); err != nil {
				return err
			}
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		t.Files = append(t.Files, File{
			Rel:     childRel,
			Size:    fi.Size(),
			ModTime: fi.ModTime().UnixNano(),
			Audio:   r.IsAudio(e.Name),
		})
	}
	return nil
}

// FindAlbums は root/rel の下にあるアルバムの相対パスを返す。rel 自身がアルバムなら rel だけを返す。
// アルバムの中のサブディレクトリはアルバムの一部として扱い、さらに下は探さない。
func (r *Rules) FindAlbums(ctx context.Context, root, rel string) ([]string, error) {
	var out []string
	var walk func(rel string) error
	walk = func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := r.ReadDir(pathutil.Join(root, rel))
		if err != nil {
			return err
		}
		if r.HasAudio(entries) {
			out = append(out, rel)
			return nil
		}
		for _, e := range entries {
			if !e.IsDir {
				continue
			}
			child := e.Name
			if rel != "" {
				child = rel + "/" + e.Name
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(rel); err != nil {
		return nil, err
	}
	return out, nil
}

// IsNotExist は、ファイルがないことを表すエラーかを返す。
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
