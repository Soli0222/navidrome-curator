// Package cover はアルバムのジャケット画像を探し、縮小した画像を作業領域にキャッシュする。
//
// アルバムのディレクトリにある画像（cover.jpg、folder.jpg など）を優先し、なければ
// 音声ファイルに埋め込まれた画像を使う。置き場はネットワークの向こうにあることが多いので、
// 一度縮小したものは作業領域に置いて使い回す。
package cover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // 画像の形式を登録する。
	"image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
	"github.com/Soli0222/navidrome-curator/internal/scan"
)

// ErrNoCover は、アルバムに画像が見つからないことを表す。
var ErrNoCover = errors.New("no cover image")

const (
	maxImageBytes = 40 << 20
	// 画像のない結果を覚えておく数の上限。超えたら忘れる。
	maxMissing = 20000
)

// Service はジャケット画像を探して縮小する。
type Service struct {
	rules    *scan.Rules
	cacheDir string
	log      *slog.Logger
	sem      chan struct{}

	mu      sync.Mutex
	missing map[string]int64 // ディレクトリ -> 画像がなかったときの更新時刻
}

// New は Service を作る。cacheDir がなければ作る。
func New(rules *scan.Rules, cacheDir string, dirMode fs.FileMode, log *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(cacheDir, dirMode); err != nil {
		return nil, err
	}
	return &Service{
		rules: rules, cacheDir: cacheDir, log: log,
		sem:     make(chan struct{}, 4),
		missing: map[string]int64{},
	}, nil
}

// Sizes は作る縮小画像の大きさ（長い辺のピクセル数）。依頼された大きさは、これ以上のうち最小のものにそろえる。
var Sizes = []int{160, 320, 640}

// SnapSize は依頼された大きさを Sizes のどれかにそろえる。
func SnapSize(n int) int {
	for _, s := range Sizes {
		if n <= s {
			return s
		}
	}
	return Sizes[len(Sizes)-1]
}

type candidate struct {
	path     string
	embedded bool
	size     int64
	modTime  int64
}

// Thumbnail は root/rel のアルバムの縮小画像のパスと、内容を表す ETag を返す。
// rel は正規化し、シンボリックリンクがないことを確かめたものを渡す。
func (s *Service) Thumbnail(ctx context.Context, root, rel string, size int) (string, string, error) {
	dir := pathutil.Join(root, rel)
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", "", err
	}
	if !fi.IsDir() {
		return "", "", pathutil.ErrNotDir
	}
	dirTime := fi.ModTime().UnixNano()
	s.mu.Lock()
	t, miss := s.missing[dir]
	s.mu.Unlock()
	if miss && t == dirTime {
		return "", "", ErrNoCover
	}

	c, err := s.pick(root, rel)
	if err != nil {
		if errors.Is(err, ErrNoCover) {
			s.mu.Lock()
			if len(s.missing) >= maxMissing {
				s.missing = map[string]int64{}
			}
			s.missing[dir] = dirTime
			s.mu.Unlock()
		}
		return "", "", err
	}

	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%t\x00%d\x00%d\x00%d", c.path, c.embedded, c.size, c.modTime, size))
	key := hex.EncodeToString(sum[:12])
	out := filepath.Join(s.cacheDir, key+".jpg")
	if _, err := os.Stat(out); err == nil {
		// 使ったものは消さずに残すため、更新時刻を進める。
		now := time.Now()
		_ = os.Chtimes(out, now, now)
		return out, key, nil
	}

	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	defer func() { <-s.sem }()

	data, err := s.read(c)
	if err != nil {
		if errors.Is(err, errNoPicture) {
			s.mu.Lock()
			s.missing[dir] = dirTime
			s.mu.Unlock()
			return "", "", ErrNoCover
		}
		return "", "", err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", "", fmt.Errorf("decode %s: %w", c.path, err)
	}
	thumb := resize(img, size)
	tmp, err := os.CreateTemp(s.cacheDir, ".tmp-*")
	if err != nil {
		return "", "", err
	}
	if err := jpeg.Encode(tmp, thumb, &jpeg.Options{Quality: 85}); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", "", err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		_ = os.Remove(tmp.Name())
		return "", "", err
	}
	return out, key, nil
}

func (s *Service) read(c candidate) ([]byte, error) {
	if c.embedded {
		return embeddedPicture(c.path)
	}
	f, err := os.OpenFile(c.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxImageBytes))
}

// pick は使う画像を選ぶ。アルバムの画像、親のディレクトリの表紙らしい画像（Disc 1 などのとき）、
// 音声ファイルに埋め込まれた画像の順に探す。
func (s *Service) pick(root, rel string) (candidate, error) {
	dir := pathutil.Join(root, rel)
	entries, err := s.rules.ReadDir(dir)
	if err != nil {
		return candidate{}, err
	}
	if c, ok := s.bestImage(dir, entries, false); ok {
		return c, nil
	}
	if parent := pathutil.Parent(rel); parent != "" {
		pdir := pathutil.Join(root, parent)
		if pentries, err := s.rules.ReadDir(pdir); err == nil && !s.rules.HasAudio(pentries) {
			if c, ok := s.bestImage(pdir, pentries, true); ok {
				return c, nil
			}
		}
	}
	// 埋め込み画像は、最初の曲のものを使う。中身はキャッシュがないときだけ読む。
	for _, e := range entries {
		if e.IsDir || !s.rules.IsAudio(e.Name) {
			continue
		}
		p := filepath.Join(dir, e.Name)
		fi, err := os.Lstat(p)
		if err != nil {
			return candidate{}, err
		}
		return candidate{path: p, embedded: true, size: fi.Size(), modTime: fi.ModTime().UnixNano()}, nil
	}
	return candidate{}, ErrNoCover
}

var (
	frontNames = []string{"cover", "folder", "front", "album", "jacket", "artwork"}
	otherParts = []string{"back", "inlay", "inside", "tray", "booklet", "disc", "cd", "obi", "spine", "label"}
)

// rank は画像の名前から、表紙らしさの順位を返す。小さいほど表紙らしい。
func rank(name string) int {
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	for _, n := range frontNames {
		if base == n {
			return 0
		}
	}
	for _, n := range frontNames {
		if strings.HasPrefix(base, n) {
			return 1
		}
	}
	for _, p := range otherParts {
		if strings.Contains(base, p) {
			return 3
		}
	}
	return 2
}

func (s *Service) bestImage(dir string, entries []scan.Entry, strongOnly bool) (candidate, bool) {
	var names []string
	for _, e := range entries {
		if !e.IsDir && isImageName(e.Name) {
			if strongOnly && rank(e.Name) > 1 {
				continue
			}
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		return candidate{}, false
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := rank(names[i]), rank(names[j])
		if ri != rj {
			return ri < rj
		}
		if len(names[i]) != len(names[j]) {
			return len(names[i]) < len(names[j])
		}
		return names[i] < names[j]
	})
	p := filepath.Join(dir, names[0])
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return candidate{}, false
	}
	return candidate{path: p, size: fi.Size(), modTime: fi.ModTime().UnixNano()}, true
}

// resize は長い辺が size になるよう縮小する。元が小さければそのまま使う。
func resize(src image.Image, size int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= size && h <= size {
		return src
	}
	nw, nh := size, size
	if w > h {
		nh = max(1, h*size/w)
	} else {
		nw = max(1, w*size/h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

// Prune は、maxAge のあいだ使われなかった縮小画像を消す。
func (s *Service) Prune(maxAge time.Duration) (int, error) {
	des, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for _, de := range des {
		fi, err := de.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(s.cacheDir, de.Name())) == nil {
			n++
		}
	}
	return n, nil
}

// RunPrune は interval ごとに Prune を呼ぶ。
func (s *Service) RunPrune(ctx context.Context, maxAge, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.Prune(maxAge); err != nil {
			s.log.Warn("cover cache prune failed", "error", err)
		} else if n > 0 {
			s.log.Info("old cover thumbnails removed", "files", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
