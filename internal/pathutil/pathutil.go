// Package pathutil は、依頼された相対パスの正規化と、決めたディレクトリの外に出ないための確認をまとめる。
//
// ファイル名はバイト列のまま扱い、Unicode の正規化はしない。
package pathutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrInvalidPath は、正規化できないパス（絶対パス、`..` を含む、NUL を含む）を表す。
	ErrInvalidPath = errors.New("invalid path")
	// ErrSymlink は、たどろうとしたパスの途中にシンボリックリンクがあることを表す。
	ErrSymlink = errors.New("path contains a symbolic link")
	// ErrNotDir は、ディレクトリであるはずの要素がディレクトリでないことを表す。
	ErrNotDir = errors.New("not a directory")
)

// CleanRel は相対パスを正規化する。空の要素と `.` を取り除き、`..`、絶対パス、NUL を拒否する。
// 空文字列はルートを表し、そのまま空文字列を返す。
func CleanRel(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains NUL", ErrInvalidPath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrInvalidPath, p)
	}
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, s := range parts {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: %q contains a .. element", ErrInvalidPath, p)
		}
		out = append(out, s)
	}
	return strings.Join(out, "/"), nil
}

// CleanAlbumRel は CleanRel と同じだが、ルート（空文字列）を拒否する。
func CleanAlbumRel(p string) (string, error) {
	c, err := CleanRel(p)
	if err != nil {
		return "", err
	}
	if c == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	return c, nil
}

// Join は root と正規化済みの相対パスをつなぐ。rel は CleanRel を通したものを渡す。
func Join(root, rel string) string {
	if rel == "" {
		return root
	}
	return root + "/" + rel
}

// Within は、p が root と同じか、その内側にあるかを字句で判定する。両方とも絶対パスを渡す。
func Within(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if root == p {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, root+"/")
}

// StrictlyWithin は、p が root の内側にあり、root と同じではないかを判定する。
func StrictlyWithin(root, p string) bool {
	return filepath.Clean(root) != filepath.Clean(p) && Within(root, p)
}

// Overlaps は、どちらかが他方の内側にあるか、同じであるかを判定する。
func Overlaps(a, b string) bool {
	return Within(a, b) || Within(b, a)
}

// Base は相対パスの最後の要素を返す。
func Base(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// Parent は相対パスの親を返す。最上位なら空文字列を返す。
func Parent(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// CheckNoSymlink は root から rel までの各要素を Lstat し、シンボリックリンクがないことを確かめる。
// 最後の要素の情報を返す。root 自体は設定で与えたものとして確かめない。
func CheckNoSymlink(root, rel string) (fs.FileInfo, error) {
	if rel == "" {
		fi, err := os.Lstat(root)
		if err != nil {
			return nil, err
		}
		return fi, nil
	}
	cur := root
	var fi fs.FileInfo
	parts := strings.Split(rel, "/")
	for i, s := range parts {
		cur = cur + "/" + s
		var err error
		fi, err = os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", ErrSymlink, cur)
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return nil, fmt.Errorf("%w: %s", ErrNotDir, cur)
		}
	}
	return fi, nil
}

// MkdirAllNoFollow は root の下に rel のディレクトリを順に作る。途中の要素がシンボリックリンクか
// ディレクトリ以外なら失敗する。作ったディレクトリのパーミッションは mode にそろえる。
func MkdirAllNoFollow(root, rel string, mode fs.FileMode) error {
	if rel == "" {
		return nil
	}
	cur := root
	for s := range strings.SplitSeq(rel, "/") {
		cur = cur + "/" + s
		fi, err := os.Lstat(cur)
		switch {
		case err == nil:
			if fi.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%w: %s", ErrSymlink, cur)
			}
			if !fi.IsDir() {
				return fmt.Errorf("%w: %s", ErrNotDir, cur)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, mode); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			// umask の影響を受けないよう、作った後にそろえる。
			if err := os.Chmod(cur, mode); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// RemoveEmptyParents は rel の親から順に、空のディレクトリを消す。root より上には遡らず、root 自体も消さない。
func RemoveEmptyParents(root, rel string) {
	for p := Parent(rel); p != ""; p = Parent(p) {
		// os.Remove は空でないディレクトリを消さない。
		if err := os.Remove(Join(root, p)); err != nil {
			return
		}
	}
}
