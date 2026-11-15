package library

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/scan"
	"github.com/Soli0222/navidrome-curator/internal/trash"
)

func setup(t *testing.T) (*Manager, *trash.Trash, string) {
	t.Helper()
	root := t.TempDir()
	lib := filepath.Join(root, "shared")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	rules, err := scan.NewRules([]string{"flac"}, []string{"._*"}, true)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := trash.New(filepath.Join(root, ".curator", "trash"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	return New([]Library{{Name: "shared", Path: lib}}, rules, tr, 0o755, slog.New(slog.NewTextHandler(io.Discard, nil))), tr, lib
}

func put(t *testing.T, lib, rel string) {
	t.Helper()
	p := filepath.Join(lib, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "01.flac"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	m, tr, lib := setup(t)
	put(t, lib, "Artist/Album")
	put(t, lib, "Other/Album")

	e, err := m.Delete("shared", "Artist/Album")
	if err != nil {
		t.Fatal(err)
	}
	if m.Present("shared", "Artist/Album") {
		t.Fatal("album still present")
	}
	if _, err := os.Stat(filepath.Join(lib, "Artist")); !os.IsNotExist(err) {
		t.Fatal("empty parent directory must be removed")
	}
	if _, err := os.Stat(lib); err != nil {
		t.Fatal("library root must stay")
	}
	// `<trash>/<日時>/<library>/<相対パス>`
	if _, err := os.Stat(filepath.Join(tr.Path(e), "01.flac")); err != nil {
		t.Fatal(err)
	}
	list, _ := tr.List()
	if len(list) != 1 || list[0].Bytes != 5 || list[0].Files != 1 || list[0].Reason != trash.ReasonDelete {
		t.Fatalf("trash = %+v", list)
	}

	if _, err := m.Restore(e.ID); err != nil {
		t.Fatal(err)
	}
	if !m.Present("shared", "Artist/Album") {
		t.Fatal("album not restored")
	}
	if list, _ := tr.List(); len(list) != 0 {
		t.Fatalf("trash must be empty after restore: %+v", list)
	}

	// 戻す先にすでにあれば失敗する。
	e2, err := m.Delete("shared", "Other/Album")
	if err != nil {
		t.Fatal(err)
	}
	put(t, lib, "Other/Album")
	if _, err := m.Restore(e2.ID); !errors.Is(err, trash.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if _, err := m.Delete("shared", "Missing/Album"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestDeleteRefusesSymlink(t *testing.T) {
	m, _, lib := setup(t)
	outside := t.TempDir()
	put(t, outside, "Album")
	if err := os.Symlink(outside, filepath.Join(lib, "Link")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete("shared", "Link/Album"); err == nil {
		t.Fatal("delete through a symlink must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "Album", "01.flac")); err != nil {
		t.Fatal("file outside the library was touched")
	}
}

func TestPurge(t *testing.T) {
	m, tr, lib := setup(t)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	tr.SetClock(func() time.Time { return now.Add(-8 * 24 * time.Hour) })
	put(t, lib, "Old/Album")
	if _, err := m.Delete("shared", "Old/Album"); err != nil {
		t.Fatal(err)
	}
	tr.SetClock(func() time.Time { return now.Add(-time.Hour) })
	put(t, lib, "New/Album")
	if _, err := m.Delete("shared", "New/Album"); err != nil {
		t.Fatal(err)
	}
	tr.SetClock(func() time.Time { return now })
	n, err := tr.Purge(7 * 24 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	list, _ := tr.List()
	if len(list) != 1 || list[0].Rel != "New/Album" {
		t.Fatalf("remaining = %+v", list)
	}
	if err := tr.Empty(); err != nil {
		t.Fatal(err)
	}
	if tr.Bytes() != 0 {
		t.Fatal("trash must be empty")
	}
}

func TestScan(t *testing.T) {
	m, _, lib := setup(t)
	put(t, lib, "A/One")
	put(t, lib, "B/Two")
	if err := os.MkdirAll(filepath.Join(lib, "C", "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := m.Scan(context.Background(), "shared")
	if s.Err != nil || len(s.Albums) != 2 || s.Bytes != 10 {
		t.Fatalf("scan = %+v", s)
	}
}
