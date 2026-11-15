package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/library"
	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/trash"
)

type env struct {
	t        *testing.T
	source   string
	root     string
	libs     map[string]string
	staging  string
	trash    *trash.Trash
	lm       *library.Manager
	m        *Manager
	mu       sync.Mutex
	finished []Job
	done     chan Job
	cancel   context.CancelFunc
}

func newEnv(t *testing.T, opts ...func(*Options)) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{
		t:      t,
		source: filepath.Join(base, "source"),
		root:   filepath.Join(base, "library"),
		libs:   map[string]string{},
		done:   make(chan Job, 100),
	}
	e.staging = filepath.Join(e.root, ".curator", "staging")
	for _, n := range []string{"shared", "alice"} {
		e.libs[n] = filepath.Join(e.root, n)
		mkdir(t, e.libs[n])
	}
	mkdir(t, e.source)
	rules, err := scan.NewRules([]string{"flac", "mp3"}, []string{"._*", ".DS_Store", "@eaDir"}, true)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.trash, err = trash.New(filepath.Join(e.root, ".curator", "trash"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	e.lm = library.New([]library.Library{{Name: "shared", Path: e.libs["shared"]}, {Name: "alice", Path: e.libs["alice"]}},
		rules, e.trash, 0o755, log)
	opt := Options{
		Sources:      map[string]string{"music": e.source},
		LibraryRoot:  e.root,
		StagingDir:   e.staging,
		FileMode:     0o640,
		DirMode:      0o750,
		Concurrency:  1,
		HistoryLimit: 50,
	}
	for _, o := range opts {
		o(&opt)
	}
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e.m = New(opt, rules, e.lm, store, log, Hooks{OnFinished: func(j Job, _ time.Duration) {
		e.mu.Lock()
		e.finished = append(e.finished, j)
		e.mu.Unlock()
		e.done <- j
	}})
	return e
}

func (e *env) start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	if err := e.m.Start(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		cancel()
		e.m.Wait()
	})
}

func (e *env) wait() Job {
	e.t.Helper()
	select {
	case j := <-e.done:
		return j
	case <-time.After(10 * time.Second):
		e.t.Fatal("timed out waiting for a job")
		return Job{}
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		mkdir(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(des) != 0 {
		t.Fatalf("%s is not empty: %v", dir, des)
	}
}

const album = "アーティスト/アルバム (2020)"

func sampleAlbum(t *testing.T, e *env) time.Time {
	t.Helper()
	writeFiles(t, e.source, map[string]string{
		album + "/01 曲.flac":          "0123456789",
		album + "/02 Song.flac":       "abcdef",
		album + "/cover.jpg":          "jpeg",
		album + "/lyrics.lrc":         "[00:00]",
		album + "/album.cue":          "FILE",
		album + "/Scans/back.jpg":     "back",
		album + "/._01 曲.flac":        "appledouble",
		album + "/.DS_Store":          "ds",
		album + "/@eaDir/thumb.jpg":   "nas",
		album + "/.hidden/secret.txt": "hidden",
	})
	if err := os.Symlink("/etc/passwd", filepath.Join(e.source, album, "link.flac")); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, f := range []string{"01 曲.flac", "02 Song.flac", "Scans/back.jpg"} {
		if err := os.Chtimes(filepath.Join(e.source, album, f), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return mt
}

func TestCopyAlbum(t *testing.T) {
	e := newEnv(t)
	mt := sampleAlbum(t, e)
	e.start()
	if _, err := e.m.Enqueue("music", album, "shared"); err != nil {
		t.Fatal(err)
	}
	j := e.wait()
	if j.Status != StatusCompleted {
		t.Fatalf("job = %+v", j)
	}
	dst := filepath.Join(e.libs["shared"], album)
	got := listFiles(t, dst)
	want := []string{"01 曲.flac", "02 Song.flac", "Scans/back.jpg", "album.cue", "cover.jpg", "lyrics.lrc"}
	if len(got) != len(want) {
		t.Fatalf("copied files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("copied files = %v, want %v", got, want)
		}
	}
	fi, err := os.Stat(filepath.Join(dst, "01 曲.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mt) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), mt)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("file mode = %v", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Join(dst, "Scans")); di.Mode().Perm() != 0o750 {
		t.Errorf("dir mode = %v", di.Mode().Perm())
	}
	if j.BytesTotal != int64(10+6+4+7+4+4) || j.FilesTotal != 6 || j.FilesDone != 6 || j.BytesDone != j.BytesTotal {
		t.Errorf("progress = %+v", j)
	}
	assertEmptyDir(t, e.staging)
	if j.Resync {
		t.Error("first copy must not be a resync")
	}
}

func TestFailureLeavesNoStagingAndNoPartialAlbum(t *testing.T) {
	e := newEnv(t)
	sampleAlbum(t, e)
	dst := filepath.Join(e.libs["shared"], album)
	copied := 0
	e.m.afterFile = func(j *Job, rel string) error {
		copied++
		// コピーの途中で、library に途中の状態が出ていないこと。
		if _, err := os.Lstat(dst); !os.IsNotExist(err) {
			t.Errorf("library shows a partial album during the copy: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(e.libs["shared"], "アーティスト")); !os.IsNotExist(err) {
			t.Errorf("library shows the artist directory during the copy")
		}
		if copied == 3 {
			return fail(CodeSourceRead, errors.New("injected failure"))
		}
		return nil
	}
	e.start()
	if _, err := e.m.Enqueue("music", album, "shared"); err != nil {
		t.Fatal(err)
	}
	j := e.wait()
	if j.Status != StatusFailed || j.ErrorCode != CodeSourceRead {
		t.Fatalf("job = %+v", j)
	}
	assertEmptyDir(t, e.staging)
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatal("the failed album must not appear in the library")
	}

	// 再実行すれば入る。
	e.m.afterFile = nil
	if _, err := e.m.Retry(j.ID); err != nil {
		t.Fatal(err)
	}
	if j2 := e.wait(); j2.Status != StatusCompleted || j2.RetryOf != j.ID {
		t.Fatalf("retry = %+v", j2)
	}
}

func TestResyncMovesOldVersionToTrash(t *testing.T) {
	e := newEnv(t)
	sampleAlbum(t, e)
	e.start()
	if _, err := e.m.Enqueue("music", album, "shared"); err != nil {
		t.Fatal(err)
	}
	if j := e.wait(); j.Status != StatusCompleted {
		t.Fatalf("first copy: %+v", j)
	}
	dst := filepath.Join(e.libs["shared"], album)
	// library 側だけにあるファイル（古い版の印）
	writeFiles(t, dst, map[string]string{"old-only.txt": "old"})
	// source 側を更新
	writeFiles(t, e.source, map[string]string{album + "/03 New.flac": "new track"})

	if _, err := e.m.Enqueue("music", album, "shared"); err != nil {
		t.Fatal(err)
	}
	j := e.wait()
	if j.Status != StatusCompleted || !j.Resync {
		t.Fatalf("resync: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(dst, "03 New.flac")); err != nil {
		t.Error("new version must have the new track")
	}
	if _, err := os.Stat(filepath.Join(dst, "old-only.txt")); !os.IsNotExist(err) {
		t.Error("new version must not contain files from the old version")
	}
	entries, err := e.trash.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %v, %v", entries, err)
	}
	te := entries[0]
	if te.Library != "shared" || te.Rel != album || te.Reason != trash.ReasonResync {
		t.Errorf("trash entry = %+v", te)
	}
	if _, err := os.Stat(filepath.Join(e.trash.Path(&te), "old-only.txt")); err != nil {
		t.Errorf("old version must be in the trash: %v", err)
	}
	assertEmptyDir(t, e.staging)
}

func TestSameAlbumIntoMultipleLibraries(t *testing.T) {
	e := newEnv(t)
	sampleAlbum(t, e)
	e.start()
	for _, l := range []string{"shared", "alice"} {
		if _, err := e.m.Enqueue("music", album, l); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if j := e.wait(); j.Status != StatusCompleted {
			t.Fatalf("job = %+v", j)
		}
	}
	for _, l := range []string{"shared", "alice"} {
		if !e.lm.Present(l, album) {
			t.Errorf("album missing in %s", l)
		}
	}
	if got := e.lm.PresentIn(album); len(got) != 2 {
		t.Errorf("PresentIn = %v", got)
	}
}

func TestDuplicateEnqueue(t *testing.T) {
	e := newEnv(t)
	sampleAlbum(t, e)
	// Start を呼ばないので、ジョブは待ちのまま。
	j1, err := e.m.Enqueue("music", album, "shared")
	if err != nil {
		t.Fatal(err)
	}
	j2, err := e.m.Enqueue("music", album, "shared")
	if !errors.Is(err, ErrDuplicate) || j2.ID != j1.ID {
		t.Fatalf("duplicate: %v %v", j2, err)
	}
	if err := e.m.Cancel(j1.ID); err != nil {
		t.Fatal(err)
	}
	if j, _ := e.m.Get(j1.ID); j.Status != StatusCanceled {
		t.Fatalf("canceled queued job = %+v", j)
	}
	if _, err := e.m.Enqueue("music", album, "shared"); err != nil {
		t.Fatalf("enqueue after cancel: %v", err)
	}
}

func TestCancelRunningJob(t *testing.T) {
	e := newEnv(t)
	sampleAlbum(t, e)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.m.afterFile = func(j *Job, rel string) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	}
	e.start()
	j, err := e.m.Enqueue("music", album, "shared")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := e.m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := e.wait()
	if got.Status != StatusCanceled {
		t.Fatalf("job = %+v", got)
	}
	assertEmptyDir(t, e.staging)
	if e.lm.Present("shared", album) {
		t.Fatal("canceled album must not appear in the library")
	}
}

func TestInsufficientSpace(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.FreeSpaceMargin = 1 << 62 })
	sampleAlbum(t, e)
	e.start()
	if _, err := e.m.Enqueue("music", album, "shared"); err != nil {
		t.Fatal(err)
	}
	j := e.wait()
	if j.Status != StatusFailed || j.ErrorCode != CodeInsufficientSpace {
		t.Fatalf("job = %+v", j)
	}
	assertEmptyDir(t, e.staging)
}

func TestSourceSymlinkAlbumRejected(t *testing.T) {
	e := newEnv(t)
	sampleAlbum(t, e)
	if err := os.Symlink(filepath.Join(e.source, "アーティスト"), filepath.Join(e.source, "Linked")); err != nil {
		t.Fatal(err)
	}
	e.start()
	if _, err := e.m.Enqueue("music", "Linked/アルバム (2020)", "shared"); err != nil {
		t.Fatal(err)
	}
	j := e.wait()
	if j.Status != StatusFailed || j.ErrorCode != CodeInvalidPath {
		t.Fatalf("job = %+v", j)
	}
	if _, err := os.Lstat(filepath.Join(e.libs["shared"], "Linked")); !os.IsNotExist(err) {
		t.Fatal("nothing must be written for a symlinked album")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	e := newEnv(t)
	e.start()
	if _, err := e.m.Enqueue("music", "../library/shared", "alice"); err != nil {
		t.Fatal(err)
	}
	if j := e.wait(); j.Status != StatusFailed || j.ErrorCode != CodeInvalidPath {
		t.Fatalf("job = %+v", j)
	}
}

func TestStartCleansStagingAndMarksInterrupted(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "state.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	running := &Job{ID: "j1", Source: "music", Rel: album, Library: "shared", Status: StatusRunning, CreatedAt: time.Now()}
	if err := store.Save(running); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	e := newEnv(t)
	writeFiles(t, e.staging, map[string]string{"leftover/01.flac": "x"})
	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	e.m.store = store
	e.start()
	assertEmptyDir(t, e.staging)
	j, ok := e.m.Get("j1")
	if !ok || j.Status != StatusFailed || j.ErrorCode != CodeInterrupted {
		t.Fatalf("restored job = %+v", j)
	}
}

func TestHistoryLimit(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.HistoryLimit = 3 })
	sampleAlbum(t, e)
	for range 5 {
		j, err := e.m.Enqueue("music", album, "shared")
		if err != nil {
			t.Fatal(err)
		}
		if err := e.m.Cancel(j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.m.List()); n != 3 {
		t.Fatalf("history = %d, want 3", n)
	}
}
