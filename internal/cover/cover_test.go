package cover

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Soli0222/navidrome-curator/internal/scan"
)

func pngBytes(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	rules, err := scan.NewRules([]string{"flac", "mp3", "m4a"}, []string{"._*"}, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(rules, filepath.Join(t.TempDir(), "covers"), 0o755, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img.Bounds().Dx(), img.Bounds().Dy()
}

func TestRank(t *testing.T) {
	for name, want := range map[string]int{
		"cover.jpg": 0, "Folder.JPG": 0, "front.png": 0, "cover (front).jpg": 1,
		"scan01.jpg": 2, "back.jpg": 3, "CD.jpg": 3, "inlay.png": 3,
	} {
		if got := rank(name); got != want {
			t.Errorf("rank(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestFolderImagePreferred(t *testing.T) {
	s, root := newService(t)
	album := filepath.Join(root, "Artist", "Album")
	write(t, filepath.Join(album, "01.flac"), []byte("fLaC"))
	write(t, filepath.Join(album, "back.png"), pngBytes(t, 50, 50, color.Black))
	write(t, filepath.Join(album, "cover.png"), pngBytes(t, 800, 600, color.White))
	write(t, filepath.Join(album, "._cover.png"), []byte("appledouble"))

	p, etag, err := s.Thumbnail(context.Background(), root, "Artist/Album", 320)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodeSize(t, p); w != 320 || h != 240 {
		t.Errorf("thumbnail = %dx%d, want 320x240", w, h)
	}
	// 2回目はキャッシュを返す。
	p2, etag2, err := s.Thumbnail(context.Background(), root, "Artist/Album", 320)
	if err != nil || p2 != p || etag2 != etag {
		t.Fatalf("cache miss: %v %v %v", p2, etag2, err)
	}
}

func TestParentCoverForDisc(t *testing.T) {
	s, root := newService(t)
	write(t, filepath.Join(root, "Artist", "Box", "folder.png"), pngBytes(t, 100, 100, color.White))
	write(t, filepath.Join(root, "Artist", "Box", "Disc 1", "01.flac"), []byte("fLaC"))
	if _, _, err := s.Thumbnail(context.Background(), root, "Artist/Box/Disc 1", 160); err != nil {
		t.Fatal(err)
	}
}

func TestNoCover(t *testing.T) {
	s, root := newService(t)
	write(t, filepath.Join(root, "A", "B", "01.flac"), []byte("fLaC\x80\x00\x00\x00"))
	_, _, err := s.Thumbnail(context.Background(), root, "A/B", 160)
	if !errors.Is(err, ErrNoCover) {
		t.Fatalf("want ErrNoCover, got %v", err)
	}
	// 2回目は覚えている結果を返す。
	if _, _, err := s.Thumbnail(context.Background(), root, "A/B", 160); !errors.Is(err, ErrNoCover) {
		t.Fatalf("want ErrNoCover, got %v", err)
	}
}

func flacWithPicture(pic []byte) []byte {
	var body bytes.Buffer
	w := func(v uint32) { _ = binary.Write(&body, binary.BigEndian, v) }
	w(3) // front cover
	mime := "image/png"
	w(uint32(len(mime)))
	body.WriteString(mime)
	w(0)
	w(0)
	w(0)
	w(0)
	w(0)
	w(uint32(len(pic)))
	body.Write(pic)
	var out bytes.Buffer
	out.WriteString("fLaC")
	// STREAMINFO（中身は使わない）
	out.Write([]byte{0x00, 0x00, 0x00, 34})
	out.Write(make([]byte, 34))
	n := body.Len()
	out.Write([]byte{0x80 | 6, byte(n >> 16), byte(n >> 8), byte(n)})
	out.Write(body.Bytes())
	return out.Bytes()
}

func syncsafeBytes(n int) []byte {
	return []byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
}

func mp3WithPicture(pic []byte) []byte {
	var frame bytes.Buffer
	frame.WriteByte(0) // ISO-8859-1
	frame.WriteString("image/png\x00")
	frame.WriteByte(3)
	frame.WriteString("desc\x00")
	frame.Write(pic)
	var tag bytes.Buffer
	tag.WriteString("TIT2")
	_ = binary.Write(&tag, binary.BigEndian, uint32(6))
	tag.Write([]byte{0, 0, 0})
	tag.WriteString("Title")
	tag.WriteString("APIC")
	_ = binary.Write(&tag, binary.BigEndian, uint32(frame.Len()))
	tag.Write([]byte{0, 0})
	tag.Write(frame.Bytes())
	var out bytes.Buffer
	out.WriteString("ID3")
	out.Write([]byte{3, 0, 0})
	out.Write(syncsafeBytes(tag.Len()))
	out.Write(tag.Bytes())
	out.Write([]byte{0xff, 0xfb, 0x90, 0x00})
	return out.Bytes()
}

func atom(typ string, body ...[]byte) []byte {
	var b bytes.Buffer
	n := 8
	for _, x := range body {
		n += len(x)
	}
	_ = binary.Write(&b, binary.BigEndian, uint32(n))
	b.WriteString(typ)
	for _, x := range body {
		b.Write(x)
	}
	return b.Bytes()
}

func m4aWithPicture(pic []byte) []byte {
	data := atom("data", []byte{0, 0, 0, 14, 0, 0, 0, 0}, pic)
	meta := atom("meta", []byte{0, 0, 0, 0}, atom("hdlr", make([]byte, 25)), atom("ilst", atom("covr", data)))
	var out bytes.Buffer
	out.Write(atom("ftyp", []byte("M4A \x00\x00\x00\x00")))
	out.Write(atom("mdat", make([]byte, 64)))
	out.Write(atom("moov", atom("mvhd", make([]byte, 100)), atom("udta", meta)))
	return out.Bytes()
}

func TestEmbeddedPictures(t *testing.T) {
	pic := pngBytes(t, 400, 400, color.RGBA{200, 30, 30, 255})
	cases := map[string][]byte{
		"01.flac": flacWithPicture(pic),
		"01.mp3":  mp3WithPicture(pic),
		"01.m4a":  m4aWithPicture(pic),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			s, root := newService(t)
			write(t, filepath.Join(root, "A", "B", name), body)
			got, err := embeddedPicture(filepath.Join(root, "A", "B", name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, pic) {
				t.Fatalf("picture differs: %d bytes", len(got))
			}
			p, _, err := s.Thumbnail(context.Background(), root, "A/B", 160)
			if err != nil {
				t.Fatal(err)
			}
			if w, h := decodeSize(t, p); w != 160 || h != 160 {
				t.Errorf("thumbnail = %dx%d", w, h)
			}
		})
	}
}

func TestSnapSize(t *testing.T) {
	for in, want := range map[int]int{1: 160, 160: 160, 161: 320, 500: 640, 5000: 640} {
		if got := SnapSize(in); got != want {
			t.Errorf("SnapSize(%d) = %d, want %d", in, got, want)
		}
	}
}
