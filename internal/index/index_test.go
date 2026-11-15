package index

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Soli0222/navidrome-curator/internal/scan"
)

func build(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	other := t.TempDir()
	for _, p := range []string{
		"Artist A/Album 1/01.flac",
		"Artist A/Album 2/01.flac",
		"Artist A/Album 2/Bonus/01.flac", // アルバムの中のアルバムは外側に含める
		"Artist A/@eaDir/x/01.flac",
		"Artist A/Album 1/._01.flac",
		"Artist B/Multi/Disc 1/01.flac",
		"Artist B/Multi/Disc 2/01.flac",
		"Artist B/Only AppleDouble/._01.flac",
		"日本語のアーティスト/アルバム/01.flac",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(other, "Artist A", "Album 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules, err := scan.NewRules([]string{"flac"}, []string{"._*", "@eaDir"}, true)
	if err != nil {
		t.Fatal(err)
	}
	m := New([]Source{{Name: "music", Path: root}, {Name: "other", Path: other}}, rules,
		slog.New(slog.NewTextHandler(io.Discard, nil)), Hooks{})
	m.RefreshAll(context.Background())
	m.Wait()
	return m
}

func TestAlbumsUnder(t *testing.T) {
	m := build(t)
	got, ok := m.AlbumsUnder("music", "Artist A")
	if !ok || !reflect.DeepEqual(got, []string{"Artist A/Album 1", "Artist A/Album 2"}) {
		t.Errorf("Artist A = %v", got)
	}
	got, _ = m.AlbumsUnder("music", "Artist B")
	if !reflect.DeepEqual(got, []string{"Artist B/Multi/Disc 1", "Artist B/Multi/Disc 2"}) {
		t.Errorf("Artist B = %v", got)
	}
	got, _ = m.AlbumsUnder("music", "")
	if len(got) != 5 {
		t.Errorf("all = %v", got)
	}
	if d, found, known := m.Lookup("music", "Artist B/Only AppleDouble"); !known || !found || d.IsAlbum {
		t.Errorf("Only AppleDouble = %+v %v %v", d, found, known)
	}
	if _, found, _ := m.Lookup("music", "Artist A/@eaDir"); found {
		t.Error("@eaDir must not be indexed")
	}
}

func TestSearch(t *testing.T) {
	m := build(t)
	res, _ := m.Search("album 1", 100)
	if len(res) != 2 { // music と other の両方
		t.Fatalf("search = %+v", res)
	}
	res, _ = m.Search("アーティスト", 100)
	// アーティストのディレクトリと、その下のアルバム（相対パスに語を含む）が出る。
	if len(res) != 2 || res[0].Rel != "日本語のアーティスト" || res[0].IsAlbum || !res[1].IsAlbum {
		t.Fatalf("artist search = %+v", res)
	}
	res, _ = m.Search("bonus", 100)
	if len(res) != 0 {
		t.Fatalf("an album inside an album must not be listed: %+v", res)
	}
	if got := m.SourcesWith("Artist A/Album 1"); !reflect.DeepEqual(got, []string{"music", "other"}) {
		t.Errorf("SourcesWith = %v", got)
	}
	_, truncated := m.Search("a", 1)
	if !truncated {
		t.Error("expected truncated results")
	}
}

func TestStatusAfterFailure(t *testing.T) {
	rules, _ := scan.NewRules([]string{"flac"}, nil, true)
	m := New([]Source{{Name: "gone", Path: filepath.Join(t.TempDir(), "missing")}}, rules,
		slog.New(slog.NewTextHandler(io.Discard, nil)), Hooks{})
	m.RefreshAll(context.Background())
	m.Wait()
	st := m.Statuses()[0]
	if st.Up || st.Ready || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
}
