package scan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

var defaultAudio = []string{"mp3", "flac", "m4a", "aac", "ogg", "opus", "wav", "aif", "aiff", "wv", "ape", "dsf", "dff", "alac"}
var defaultExclude = []string{"._*", ".DS_Store", ".AppleDouble", "Thumbs.db", "desktop.ini", "@eaDir", "#recycle", "#snapshot"}

func rules(t *testing.T) *Rules {
	t.Helper()
	r, err := NewRules(defaultAudio, defaultExclude, true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExcluded(t *testing.T) {
	r := rules(t)
	cases := []struct {
		name string
		dir  bool
		want bool
	}{
		{"._01 Track.flac", false, true},
		{".DS_Store", false, true},
		{".ds_store", false, true}, // 大文字小文字を区別しない
		{".AppleDouble", true, true},
		{"Thumbs.db", false, true},
		{"THUMBS.DB", false, true},
		{"desktop.ini", false, true},
		{"@eaDir", true, true},
		{"#recycle", true, true},
		{"#snapshot", true, true},
		{".hidden", true, true},   // ドットで始まるディレクトリ
		{".hidden", false, false}, // ドットで始まるファイルはパターンに当たらなければ除外しない
		{"01 Track.flac", false, false},
		{"cover.jpg", false, false},
		{"Disc 1", true, false},
	}
	for _, c := range cases {
		if got := r.Excluded(c.name, c.dir); got != c.want {
			t.Errorf("Excluded(%q, dir=%v) = %v, want %v", c.name, c.dir, got, c.want)
		}
	}
}

func TestIsAudio(t *testing.T) {
	r := rules(t)
	for name, want := range map[string]bool{
		"01.flac":      true,
		"01.FLAC":      true,
		"01.Mp3":       true,
		"01.dsf":       true,
		"._01.flac":    false, // AppleDouble は曲ではない
		"cover.jpg":    false,
		"album.cue":    false,
		"lyrics.lrc":   false,
		"noext":        false,
		"trailingdot.": false,
		"日本語のタイトル.m4a": true,
	} {
		if got := r.IsAudio(name); got != want {
			t.Errorf("IsAudio(%q) = %v, want %v", name, got, want)
		}
	}
}

// 実際の NAS に近い木。
func nasTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, map[string]string{
		"Artist A/Album 1/01 Song.flac":                             "aaaa",
		"Artist A/Album 1/02 Song.flac":                             "bbbbbb",
		"Artist A/Album 1/._01 Song.flac":                           "x",
		"Artist A/Album 1/.DS_Store":                                "x",
		"Artist A/Album 1/cover.jpg":                                "img",
		"Artist A/Album 1/album.cue":                                "cue",
		"Artist A/Album 1/Scans/back.jpg":                           "img2",
		"Artist A/Album 1/@eaDir/01 Song.flac/SYNOINDEX_MEDIA_INFO": "x",
		"Artist A/Album 1/.AppleDouble/01 Song.flac":                "x",
		"Artist A/Only AppleDouble/._01 Song.flac":                  "x",
		"Artist A/Only AppleDouble/cover.jpg":                       "img",
		"Artist A/@eaDir/Album 1/thumb.jpg":                         "x",
		"Artist B/Multi/Disc 1/01.mp3":                              "1",
		"Artist B/Multi/Disc 2/01.mp3":                              "2",
		"#recycle/Old/01.flac":                                      "old",
		".hidden/Album/01.flac":                                     "hidden",
	})
	return root
}

func TestHasAudioAndFindAlbums(t *testing.T) {
	r := rules(t)
	root := nasTree(t)
	ctx := context.Background()

	entries, err := r.ReadDir(filepath.Join(root, "Artist A", "Album 1"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"01 Song.flac", "02 Song.flac", "Scans", "album.cue", "cover.jpg"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("ReadDir = %v, want %v", names, want)
	}
	if !r.HasAudio(entries) {
		t.Error("Album 1 must be an album")
	}

	only, err := r.ReadDir(filepath.Join(root, "Artist A", "Only AppleDouble"))
	if err != nil {
		t.Fatal(err)
	}
	if r.HasAudio(only) {
		t.Error("a directory with only AppleDouble files is not an album")
	}

	albums, err := r.FindAlbums(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(albums)
	wantAlbums := []string{"Artist A/Album 1", "Artist B/Multi/Disc 1", "Artist B/Multi/Disc 2"}
	if !reflect.DeepEqual(albums, wantAlbums) {
		t.Errorf("FindAlbums = %v, want %v", albums, wantAlbums)
	}
	one, err := r.FindAlbums(ctx, root, "Artist A/Album 1")
	if err != nil || !reflect.DeepEqual(one, []string{"Artist A/Album 1"}) {
		t.Errorf("FindAlbums(album) = %v, %v", one, err)
	}
}

func TestReadTreeStats(t *testing.T) {
	r := rules(t)
	root := nasTree(t)
	album := filepath.Join(root, "Artist A", "Album 1")
	if err := os.Symlink(filepath.Join(album, "cover.jpg"), filepath.Join(album, "link.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(album, "linkdir")); err != nil {
		t.Fatal(err)
	}
	tree, err := r.ReadTree(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range tree.Files {
		files = append(files, f.Rel)
	}
	sort.Strings(files)
	want := []string{"01 Song.flac", "02 Song.flac", "Scans/back.jpg", "album.cue", "cover.jpg"}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	st := tree.Stats()
	if st.Files != 5 || st.Tracks != 2 || st.Bytes != int64(4+6+4+3+3) {
		t.Errorf("stats = %+v", st)
	}
	if len(tree.Dirs) != 2 {
		t.Errorf("dirs = %+v", tree.Dirs)
	}
}
