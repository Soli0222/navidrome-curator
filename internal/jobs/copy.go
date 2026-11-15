package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/pathutil"
	"github.com/Soli0222/navidrome-curator/internal/scan"
)

// jobError は失敗の理由のコードを持つエラー。
type jobError struct {
	code string
	err  error
}

func (e *jobError) Error() string { return e.err.Error() }
func (e *jobError) Unwrap() error { return e.err }

func fail(code string, err error) error { return &jobError{code: code, err: err} }

func failf(code, format string, a ...any) error { return fail(code, fmt.Errorf(format, a...)) }

// classify はエラーを理由のコードと文言に分ける。
func classify(err error) (string, string) {
	var je *jobError
	if errors.As(err, &je) {
		// 書き込み側の失敗のうち、名前が使えないものは name_invalid にする。
		if je.code == CodeDestinationWrite && isNameError(err) {
			return CodeNameInvalid, err.Error()
		}
		if je.code == CodeDestinationWrite && errors.Is(err, syscall.ENOSPC) {
			return CodeInsufficientSpace, err.Error()
		}
		return je.code, err.Error()
	}
	return CodeInternal, err.Error()
}

func isNameError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, syscall.EILSEQ) || errors.Is(err, syscall.EINVAL)
}

type copyResult struct {
	resync bool
}

// copyAlbum は source のアルバムを staging にコピーし、確かめてから library に移す。
// 失敗したときと取り消したときは staging を消す。
func (m *Manager) copyAlbum(ctx context.Context, j *Job, e *entry) (copyResult, error) {
	srcRoot, ok := m.opt.Sources[j.Source]
	if !ok {
		return copyResult{}, failf(CodeNotFound, "unknown source %q", j.Source)
	}
	if _, ok := m.libs.Get(j.Library); !ok {
		return copyResult{}, failf(CodeNotFound, "unknown library %q", j.Library)
	}
	rel, err := pathutil.CleanAlbumRel(j.Rel)
	if err != nil || rel != j.Rel {
		return copyResult{}, failf(CodeInvalidPath, "invalid path %q", j.Rel)
	}
	fi, err := pathutil.CheckNoSymlink(srcRoot, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return copyResult{}, fail(CodeNotFound, err)
		}
		if errors.Is(err, pathutil.ErrSymlink) {
			return copyResult{}, fail(CodeInvalidPath, err)
		}
		return copyResult{}, fail(CodeSourceRead, err)
	}
	if !fi.IsDir() {
		return copyResult{}, failf(CodeNotFound, "%s is not a directory", rel)
	}
	srcDir := pathutil.Join(srcRoot, rel)

	// 1. コピーするものを読む。
	tree, err := m.rules.ReadTree(ctx, srcDir)
	if err != nil {
		if ctx.Err() != nil {
			return copyResult{}, ctx.Err()
		}
		return copyResult{}, fail(CodeSourceRead, err)
	}
	stats := tree.Stats()
	j.BytesTotal, j.FilesTotal = stats.Bytes, stats.Files
	m.mu.Lock()
	e.job.BytesTotal, e.job.FilesTotal = stats.Bytes, stats.Files
	m.mu.Unlock()

	// 2. 空き容量を確かめる。
	usage, err := scan.Statfs(m.opt.LibraryRoot)
	if err != nil {
		return copyResult{}, fail(CodeDestinationWrite, err)
	}
	need := uint64(stats.Bytes) + uint64(m.opt.FreeSpaceMargin) //nolint:gosec // どちらも負にならない。
	if usage.Free < need {
		var trashBytes int64
		if m.opt.TrashBytes != nil {
			trashBytes = m.opt.TrashBytes()
		}
		return copyResult{}, failf(CodeInsufficientSpace,
			"not enough free space: need %d bytes (album %d + margin %d), free %d bytes, trash holds %d bytes",
			need, stats.Bytes, m.opt.FreeSpaceMargin, usage.Free, trashBytes)
	}

	// 3. staging にコピーする。
	staging := filepath.Join(m.opt.StagingDir, j.ID)
	placed := false
	defer func() {
		if !placed {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Mkdir(staging, m.opt.DirMode); err != nil {
		return copyResult{}, fail(CodeDestinationWrite, err)
	}
	if err := os.Chmod(staging, m.opt.DirMode); err != nil {
		return copyResult{}, fail(CodeDestinationWrite, err)
	}
	for _, d := range tree.Dirs {
		if d.Rel == "" {
			continue
		}
		p := pathutil.Join(staging, d.Rel)
		if err := os.Mkdir(p, m.opt.DirMode); err != nil {
			return copyResult{}, fail(CodeDestinationWrite, err)
		}
		if err := os.Chmod(p, m.opt.DirMode); err != nil {
			return copyResult{}, fail(CodeDestinationWrite, err)
		}
	}
	buf := make([]byte, 1<<20)
	for _, f := range tree.Files {
		if err := ctx.Err(); err != nil {
			return copyResult{}, err
		}
		if err := m.copyFile(ctx, pathutil.Join(srcDir, f.Rel), pathutil.Join(staging, f.Rel), f, buf, e); err != nil {
			return copyResult{}, err
		}
		e.filesDone.Add(1)
		if m.afterFile != nil {
			if err := m.afterFile(j, f.Rel); err != nil {
				return copyResult{}, err
			}
		}
	}

	// 4. 各ファイルのサイズを source 側と比べる。
	for _, f := range tree.Files {
		sfi, err := os.Lstat(pathutil.Join(srcDir, f.Rel))
		if err != nil {
			return copyResult{}, fail(CodeSourceRead, err)
		}
		dfi, err := os.Lstat(pathutil.Join(staging, f.Rel))
		if err != nil {
			return copyResult{}, fail(CodeDestinationWrite, err)
		}
		if sfi.Size() != dfi.Size() || sfi.Size() != f.Size {
			return copyResult{}, failf(CodeSizeMismatch, "%s: source %d bytes, copied %d bytes", f.Rel, sfi.Size(), dfi.Size())
		}
	}

	// ディレクトリの更新時刻は、中身を作り終えてから深いものから合わせる。
	for i := len(tree.Dirs) - 1; i >= 0; i-- {
		d := tree.Dirs[i]
		t := time.Unix(0, d.ModTime)
		if err := os.Chtimes(pathutil.Join(staging, d.Rel), t, t); err != nil {
			return copyResult{}, fail(CodeDestinationWrite, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return copyResult{}, err
	}

	// 5. library に rename(2) で移す。すでにあれば古い版を trash に移してから置き換える。
	old, err := m.libs.Place(j.Library, rel, staging)
	if err != nil {
		return copyResult{}, fail(CodeDestinationWrite, err)
	}
	placed = true
	return copyResult{resync: old != nil}, nil
}

func (m *Manager) copyFile(ctx context.Context, src, dst string, f scan.File, buf []byte, e *entry) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fail(CodeSourceRead, err)
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return fail(CodeSourceRead, err)
	}
	if !fi.Mode().IsRegular() {
		return failf(CodeSourceRead, "%s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, m.opt.FileMode)
	if err != nil {
		return fail(CodeDestinationWrite, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = out.Close()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fail(CodeDestinationWrite, werr)
			}
			e.bytesDone.Add(int64(n))
			if m.hooks.OnCopiedBytes != nil {
				m.hooks.OnCopiedBytes(int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(CodeSourceRead, rerr)
		}
	}
	if err := out.Sync(); err != nil {
		return fail(CodeDestinationWrite, err)
	}
	closed = true
	if err := out.Close(); err != nil {
		return fail(CodeDestinationWrite, err)
	}
	// umask の影響を受けないよう、作った後にそろえる。
	if err := os.Chmod(dst, m.opt.FileMode); err != nil {
		return fail(CodeDestinationWrite, err)
	}
	t := time.Unix(0, f.ModTime)
	if err := os.Chtimes(dst, t, t); err != nil {
		return fail(CodeDestinationWrite, err)
	}
	return nil
}
