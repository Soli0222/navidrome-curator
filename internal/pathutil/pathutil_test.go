package pathutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanRel(t *testing.T) {
	ok := map[string]string{
		"":                    "",
		".":                   "",
		"Artist/Album":        "Artist/Album",
		"Artist//Album/":      "Artist/Album",
		"./Artist/./Album":    "Artist/Album",
		"アーティスト/アルバム":         "アーティスト/アルバム",
		"Artist/..Album":      "Artist/..Album",
		"Artist/Album...":     "Artist/Album...",
		"Ar\\tist/Album":      "Ar\\tist/Album",
		"\xff\xfe/invalid":    "\xff\xfe/invalid", // UTF-8 でない名前もそのまま
		"Artist/Album (Disc)": "Artist/Album (Disc)",
	}
	for in, want := range ok {
		got, err := CleanRel(in)
		if err != nil {
			t.Errorf("CleanRel(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("CleanRel(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{"/etc/passwd", "..", "../x", "Artist/../../x", "Artist/..", "a\x00b", "/"}
	for _, in := range bad {
		if _, err := CleanRel(in); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanRel(%q) = %v, want ErrInvalidPath", in, err)
		}
	}
	if _, err := CleanAlbumRel(""); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("CleanAlbumRel(\"\") should fail")
	}
}

func TestNFCAndNFDAreDistinct(t *testing.T) {
	// 「が」の NFC と NFD。正規化しないので別の名前として扱う。
	nfc := "が"
	nfd := "が"
	a, _ := CleanRel(nfc)
	b, _ := CleanRel(nfd)
	if a == b {
		t.Fatal("NFC and NFD must not be normalized")
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{"/library", "/library", true},
		{"/library", "/library/a", true},
		{"/library", "/library2", false},
		{"/library/", "/library/a/b", true},
		{"/library/a", "/library", false},
		{"/", "/x", true},
	}
	for _, c := range cases {
		if got := Within(c.root, c.p); got != c.want {
			t.Errorf("Within(%q, %q) = %v, want %v", c.root, c.p, got, c.want)
		}
	}
	if StrictlyWithin("/a", "/a") {
		t.Error("StrictlyWithin must be false for the same path")
	}
	if !Overlaps("/a/b", "/a") || Overlaps("/a/b", "/a/c") {
		t.Error("Overlaps is wrong")
	}
}

func TestCheckNoSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(root, "Artist", "Album"))
	if err := os.Symlink(outside, filepath.Join(root, "Link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Artist", "Album"), filepath.Join(root, "Artist", "AlbumLink")); err != nil {
		t.Fatal(err)
	}

	if fi, err := CheckNoSymlink(root, "Artist/Album"); err != nil || !fi.IsDir() {
		t.Fatalf("plain path: %v", err)
	}
	for _, rel := range []string{"Link", "Link/x", "Artist/AlbumLink"} {
		if _, err := CheckNoSymlink(root, rel); !errors.Is(err, ErrSymlink) {
			t.Errorf("CheckNoSymlink(%q) = %v, want ErrSymlink", rel, err)
		}
	}
	if _, err := CheckNoSymlink(root, "Artist/missing"); !os.IsNotExist(err) {
		t.Errorf("missing path: %v", err)
	}
}

func TestMkdirAllNoFollow(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := MkdirAllNoFollow(root, "a/b/c", 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "a", "b", "c"))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("created dir: %v %v", fi, err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllNoFollow(root, "link/x", 0o755); !errors.Is(err, ErrSymlink) {
		t.Fatalf("want ErrSymlink, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatal("must not create anything through a symlink")
	}
}

func TestRemoveEmptyParents(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "A", "B"))
	mustMkdir(t, filepath.Join(root, "X", "Y", "Z"))
	if err := os.WriteFile(filepath.Join(root, "X", "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// アルバム自体は移したあとの想定。
	RemoveEmptyParents(root, "A/B/Album")
	if _, err := os.Stat(filepath.Join(root, "A", "B")); err == nil {
		t.Error("A/B should be removed")
	}
	if _, err := os.Stat(filepath.Join(root, "A")); err == nil {
		t.Error("A should be removed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("root must be kept")
	}
	if err := os.Remove(filepath.Join(root, "X", "Y", "Z")); err != nil {
		t.Fatal(err)
	}
	RemoveEmptyParents(root, "X/Y/Z")
	if _, err := os.Stat(filepath.Join(root, "X")); err != nil {
		t.Error("X has a file and must be kept")
	}
	if _, err := os.Stat(filepath.Join(root, "X", "Y")); err == nil {
		t.Error("X/Y should be removed")
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
