package cover

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
)

// 埋め込み画像を探すときに読む量の上限。タグは通常ファイルの先頭にあり、これより大きいことはまずない。
const maxTagBytes = 32 << 20

var errNoPicture = errors.New("no embedded picture")

// embeddedPicture は音声ファイルに埋め込まれた画像を返す。FLAC、ID3v2（MP3 など）、MP4（M4A）に対応する。
func embeddedPicture(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errNoPicture
		}
		return nil, err
	}
	switch {
	case string(head[:4]) == "fLaC":
		return flacPicture(f)
	case string(head[:3]) == "ID3":
		return id3Picture(f, head[:10])
	case string(head[4:8]) == "ftyp":
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		return mp4Picture(f, fi.Size())
	}
	return nil, errNoPicture
}

// flacPicture は FLAC の METADATA_BLOCK_PICTURE を読む。表紙（種類 3）を優先する。
func flacPicture(f *os.File) ([]byte, error) {
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		return nil, err
	}
	var best []byte
	read := int64(0)
	for {
		var h [4]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			break
		}
		last := h[0]&0x80 != 0
		typ := h[0] & 0x7f
		n := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		read += 4 + n
		if read > maxTagBytes {
			break
		}
		if typ == 6 {
			buf := make([]byte, n)
			if _, err := io.ReadFull(f, buf); err != nil {
				break
			}
			ptype, data := parseFlacPicture(buf)
			if data != nil && (best == nil || ptype == 3) {
				best = data
				if ptype == 3 {
					return best, nil
				}
			}
		} else if _, err := f.Seek(n, io.SeekCurrent); err != nil {
			break
		}
		if last {
			break
		}
	}
	if best == nil {
		return nil, errNoPicture
	}
	return best, nil
}

func parseFlacPicture(b []byte) (uint32, []byte) {
	r := bytes.NewReader(b)
	var ptype, l uint32
	if binary.Read(r, binary.BigEndian, &ptype) != nil {
		return 0, nil
	}
	// MIME と説明を飛ばす。
	for range 2 {
		if binary.Read(r, binary.BigEndian, &l) != nil || int64(l) > int64(r.Len()) {
			return 0, nil
		}
		if _, err := r.Seek(int64(l), io.SeekCurrent); err != nil {
			return 0, nil
		}
	}
	// 幅、高さ、色深度、色数
	if _, err := r.Seek(16, io.SeekCurrent); err != nil {
		return 0, nil
	}
	if binary.Read(r, binary.BigEndian, &l) != nil || int64(l) > int64(r.Len()) {
		return 0, nil
	}
	data := make([]byte, l)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil
	}
	return ptype, data
}

func syncsafe(b []byte) int64 {
	return int64(b[0]&0x7f)<<21 | int64(b[1]&0x7f)<<14 | int64(b[2]&0x7f)<<7 | int64(b[3]&0x7f)
}

// id3Picture は ID3v2.3 と v2.4 の APIC フレームを読む。表紙（種類 3）を優先する。
func id3Picture(f *os.File, header []byte) ([]byte, error) {
	major := header[3]
	if major != 3 && major != 4 {
		return nil, errNoPicture
	}
	size := syncsafe(header[6:10])
	if size > maxTagBytes {
		return nil, errNoPicture
	}
	if _, err := f.Seek(10, io.SeekStart); err != nil {
		return nil, err
	}
	tag := make([]byte, size)
	if _, err := io.ReadFull(f, tag); err != nil {
		return nil, err
	}
	pos := 0
	// 拡張ヘッダーを飛ばす。
	if header[5]&0x40 != 0 && len(tag) >= 4 {
		if major == 4 {
			pos = int(syncsafe(tag[:4]))
		} else {
			pos = int(binary.BigEndian.Uint32(tag[:4])) + 4
		}
	}
	var best []byte
	for pos+10 <= len(tag) {
		id := string(tag[pos : pos+4])
		if id[0] == 0 {
			break
		}
		var n int
		if major == 4 {
			n = int(syncsafe(tag[pos+4 : pos+8]))
		} else {
			n = int(binary.BigEndian.Uint32(tag[pos+4 : pos+8]))
		}
		body := pos + 10
		if n < 0 || body+n > len(tag) {
			break
		}
		if id == "APIC" {
			ptype, data := parseAPIC(tag[body : body+n])
			if data != nil && (best == nil || ptype == 3) {
				best = data
				if ptype == 3 {
					return best, nil
				}
			}
		}
		pos = body + n
	}
	if best == nil {
		return nil, errNoPicture
	}
	return best, nil
}

func parseAPIC(b []byte) (byte, []byte) {
	if len(b) < 4 {
		return 0, nil
	}
	enc := b[0]
	i := bytes.IndexByte(b[1:], 0)
	if i < 0 {
		return 0, nil
	}
	p := 1 + i + 1
	if p >= len(b) {
		return 0, nil
	}
	ptype := b[p]
	p++
	// 説明。UTF-16 の文字コード（1、2）は 0x00 0x00 で終わる。
	if enc == 1 || enc == 2 {
		for p+1 < len(b) && (b[p] != 0 || b[p+1] != 0) {
			p += 2
		}
		p += 2
	} else {
		j := bytes.IndexByte(b[p:], 0)
		if j < 0 {
			return 0, nil
		}
		p += j + 1
	}
	if p >= len(b) {
		return 0, nil
	}
	return ptype, b[p:]
}

// mp4Picture は moov/udta/meta/ilst/covr/data を探す。
func mp4Picture(f *os.File, size int64) ([]byte, error) {
	path := []string{"moov", "udta", "meta", "ilst", "covr", "data"}
	start, end := int64(0), size
	for depth, want := range path {
		found := false
		pos := start
		for pos+8 <= end {
			var h [8]byte
			if _, err := f.ReadAt(h[:], pos); err != nil {
				return nil, err
			}
			n := int64(binary.BigEndian.Uint32(h[:4]))
			typ := string(h[4:8])
			hdr := int64(8)
			switch n {
			case 1:
				var ext [8]byte
				if _, err := f.ReadAt(ext[:], pos+8); err != nil {
					return nil, err
				}
				n = int64(binary.BigEndian.Uint64(ext[:]))
				hdr = 16
			case 0:
				n = end - pos
			}
			if n < hdr || pos+n > end {
				return nil, errNoPicture
			}
			if typ == want {
				start, end = pos+hdr, pos+n
				if want == "meta" {
					// meta は version と flags の4バイトを持つ。
					start += 4
				}
				found = true
				break
			}
			pos += n
		}
		if !found {
			return nil, errNoPicture
		}
		if depth == len(path)-1 {
			// data は種類とロケールの8バイトのあとに画像が続く。
			n := end - start - 8
			if n <= 0 || n > maxTagBytes {
				return nil, errNoPicture
			}
			buf := make([]byte, n)
			if _, err := f.ReadAt(buf, start+8); err != nil {
				return nil, err
			}
			return buf, nil
		}
	}
	return nil, errNoPicture
}

// isImageName は画像ファイルの名前かを返す。
func isImageName(name string) bool {
	switch ext(name) {
	case "jpg", "jpeg", "png", "webp", "gif":
		return true
	}
	return false
}

func ext(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}
