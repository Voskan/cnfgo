package cnfrt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testSections() []Section {
	rng := rand.New(rand.NewPCG(7, 9))
	mk := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return b
	}
	return []Section{
		{Kind: SectionTable, ElemAlign: 8, Tag: 0, Count: 3, Data: mk(48)},
		{Kind: SectionPool, ElemAlign: 4, Tag: 2, Count: 0, Data: nil},
		{Kind: SectionPool, ElemAlign: 4, Tag: 3, Count: 25, Data: mk(100)},
		{Kind: SectionStrBlob, ElemAlign: 1, Count: 1000, Data: mk(1000)},
	}
}

func testHeader() Header {
	return Header{Flags: FlagCyclicAllowed, SchemaHash: 0xfeedface, HashSeed: 42, RootTable: 0, RootID: 1}
}

func TestImageRoundTrip(t *testing.T) {
	h, secs := testHeader(), testSections()
	img, err := AppendImage(nil, h, secs)
	if err != nil {
		t.Fatal(err)
	}
	if len(img) != ImageSize(h, secs) || len(img)%SectionAlign != 0 {
		t.Fatalf("image size %d, ImageSize %d", len(img), ImageSize(h, secs))
	}
	var buf bytes.Buffer
	if n, err := WriteImage(&buf, h, secs); err != nil || n != int64(len(img)) || !bytes.Equal(buf.Bytes(), img) {
		t.Fatalf("WriteImage differs from AppendImage: %v", err)
	}
	for _, mode := range []VerifyMode{VerifyTrustedHeader, VerifyChecksum, VerifyUntrusted} {
		got, infos, err := ParseHeader(img, mode, h.SchemaHash)
		if err != nil {
			t.Fatalf("%v: %v", mode, err)
		}
		h.TotalSize = uint64(len(img))
		if got != h {
			t.Fatalf("header %+v, want %+v", got, h)
		}
		for i, in := range infos {
			if in.Off%SectionAlign != 0 || !bytes.Equal(SectionData(img, in), secs[i].Data) || in.Kind != secs[i].Kind ||
				in.Tag != secs[i].Tag || in.Count != secs[i].Count || in.ElemAlign != secs[i].ElemAlign {
				t.Fatalf("section %d: %+v", i, in)
			}
		}
		if in, ok := FindSection(infos, SectionPool, 3); !ok || in.Count != 25 {
			t.Fatal("FindSection")
		}
		if _, ok := FindSection(infos, SectionExt, 0); ok {
			t.Fatal("FindSection found a missing section")
		}
	}
}

// resign recomputes the header and directory checksums of img in place.
func resign(img []byte) {
	n := binary.LittleEndian.Uint32(img[12:])
	binary.LittleEndian.PutUint32(img[56:], CRC32C(img[HeaderSize:HeaderSize+n*DirEntrySize]))
	binary.LittleEndian.PutUint32(img[60:], CRC32C(img[:60]))
}

func TestParseHeaderRejects(t *testing.T) {
	img, err := AppendImage(nil, testHeader(), testSections())
	if err != nil {
		t.Fatal(err)
	}
	dirEntry := func(i, field int) int { return HeaderSize + i*DirEntrySize + field }
	cases := []struct {
		name   string
		mutate func(b []byte) []byte
		want   error
	}{
		{"short", func(b []byte) []byte { return b[:10] }, ErrCorruptHeader},
		{"magic", func(b []byte) []byte { b[0] = 'X'; resign(b); return b }, ErrCorruptHeader},
		{"header crc", func(b []byte) []byte { b[20] ^= 1; return b }, ErrChecksumMismatch},
		{"version", func(b []byte) []byte { b[4] = 9; resign(b); return b }, ErrCorruptHeader},
		{"endianness", func(b []byte) []byte { b[6] = 2; resign(b); return b }, ErrCorruptHeader},
		{"flags", func(b []byte) []byte { b[11] = 0x80; resign(b); return b }, ErrCorruptHeader},
		{"reserved", func(b []byte) []byte { b[50] = 1; resign(b); return b }, ErrCorruptHeader},
		{"truncated", func(b []byte) []byte { return b[:len(b)-64] }, ErrCorruptHeader},
		{"schema", func(b []byte) []byte { b[16] ^= 1; resign(b); return b }, ErrSchemaMismatch},
		{"section count", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:], 1<<30); resign(b); return b }, ErrCorruptHeader},
		{"directory crc", func(b []byte) []byte { b[dirEntry(0, 24)] ^= 1; return b }, ErrChecksumMismatch},
		{"offset overflow", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[dirEntry(0, 8):], ^uint64(0))
			resign(b)
			return b
		}, ErrBoundsViolation},
		{"length overflow", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[dirEntry(3, 16):], ^uint64(0)-100)
			resign(b)
			return b
		}, ErrBoundsViolation},
		{"misaligned", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[dirEntry(3, 8):], binary.LittleEndian.Uint64(b[dirEntry(3, 8):])+8)
			resign(b)
			return b
		}, ErrBoundsViolation},
		{"overlap", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[dirEntry(2, 8):], binary.LittleEndian.Uint64(b[dirEntry(0, 8):]))
			resign(b)
			return b
		}, ErrBoundsViolation},
		{"element alignment", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[dirEntry(0, 2):], 3)
			resign(b)
			return b
		}, ErrBoundsViolation},
		{"section crc", func(b []byte) []byte {
			_, infos, _ := ParseHeader(b, VerifyTrustedHeader, 0)
			b[infos[3].Off] ^= 1
			return b
		}, ErrChecksumMismatch},
	}
	for _, c := range cases {
		b := c.mutate(bytes.Clone(img))
		_, _, err := ParseHeader(b, VerifyChecksum, 0xfeedface)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	// TrustedHeader skips section checksums only.
	b := bytes.Clone(img)
	_, infos, _ := ParseHeader(b, VerifyTrustedHeader, 0)
	b[infos[3].Off] ^= 1
	if _, _, err := ParseHeader(b, VerifyTrustedHeader, 0); err != nil {
		t.Errorf("TrustedHeader verified a section checksum: %v", err)
	}
}

type memFile struct {
	buf []byte
	pos int
}

func (m *memFile) Write(p []byte) (int, error) {
	if need := m.pos + len(p); need > len(m.buf) {
		m.buf = append(m.buf, make([]byte, need-len(m.buf))...)
	}
	copy(m.buf[m.pos:], p)
	m.pos += len(p)
	return len(p), nil
}

func (m *memFile) Seek(off int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, errors.New("unsupported whence")
	}
	m.pos = int(off)
	return off, nil
}

func TestChunkedWriter(t *testing.T) {
	h, secs := testHeader(), testSections()
	want, _ := AppendImage(nil, h, secs)
	specs := make([]SectionSpec, len(secs))
	var payload []byte
	for i, s := range secs {
		specs[i] = SectionSpec{Kind: s.Kind, ElemAlign: s.ElemAlign, Tag: s.Tag, Count: s.Count, Len: uint64(len(s.Data))}
		payload = append(payload, s.Data...)
	}
	for _, chunk := range []int{1, 7, 64, 1 << 20} {
		f := &memFile{}
		cw, err := NewChunkedWriter(f, h, specs)
		if err != nil {
			t.Fatal(err)
		}
		for p := payload; len(p) > 0; {
			n := min(chunk, len(p))
			if _, err := cw.Write(p[:n]); err != nil {
				t.Fatal(err)
			}
			p = p[n:]
		}
		if err := cw.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f.buf, want) {
			t.Fatalf("chunk %d: ChunkedWriter image differs from AppendImage", chunk)
		}
	}
	cw, _ := NewChunkedWriter(&memFile{}, h, specs)
	cw.Write(payload[:10])
	if err := cw.Close(); err == nil {
		t.Fatal("short payload accepted")
	}
	cw, _ = NewChunkedWriter(&memFile{}, h, specs)
	if _, err := cw.Write(append(bytes.Clone(payload), 1)); err == nil {
		t.Fatal("excess payload accepted")
	}
}

func TestWriteSnapshotAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.cnf")
	img, _ := AppendImage(nil, testHeader(), testSections())
	for range 2 {
		if err := WriteSnapshotAtomic(path, func(w io.Writer) error { _, err := w.Write(img); return err }); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, img) {
		t.Fatalf("file contents differ: %v", err)
	}
	boom := errors.New("boom")
	if err := WriteSnapshotAtomic(path, func(w io.Writer) error { w.Write([]byte("partial")); return boom }); !errors.Is(err, boom) {
		t.Fatalf("writer error not returned: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, img) {
		t.Fatal("failed write replaced the previous file")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestExtTable(t *testing.T) {
	ny, err := ResolveLocation("America/New_York")
	if err != nil {
		t.Skip("tz database unavailable")
	}
	locs := []LocEntry{{Kind: LocIANA, Name: "America/New_York", Loc: ny}, {Kind: LocFixed, Name: "XYZ", Offset: -5400}}
	zoned := []netip.Addr{netip.MustParseAddr("fe80::1%eth0"), netip.MustParseAddr("fe80::2%en1")}
	ext := EncodeExt(locs, zoned)
	gotLocs, gotZoned, err := DecodeExt(ext)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotZoned, zoned) || len(gotLocs) != 2 || gotLocs[0].Loc != ny || gotLocs[1].Name != "XYZ" || gotLocs[1].Offset != -5400 {
		t.Fatalf("ext round trip: %+v %+v", gotLocs, gotZoned)
	}
	if _, off := time.Unix(0, 0).In(gotLocs[1].Loc).Zone(); off != -5400 {
		t.Fatal("fixed location offset")
	}
	if z, ok := ExtZone(string(ext), 2); !ok || z != "en1" {
		t.Fatal("ExtZone")
	}
	if _, ok := ExtZone(string(ext), 3); ok {
		t.Fatal("ExtZone out of range")
	}
	if EncodeExt(nil, nil) != nil {
		t.Fatal("empty ext must be nil")
	}
	for i := range ext {
		for _, bad := range [][]byte{ext[:i], func() []byte { b := bytes.Clone(ext); b[i] ^= 0xff; return b }()} {
			DecodeExt(bad) // must not panic
		}
	}
	badKind := bytes.Clone(ext)
	badKind[8] = 99 // unknown location kind in entry 1
	if loc, ok := ExtLocation(string(badKind), 1); ok || loc != nil {
		t.Fatalf("ExtLocation accepted unknown kind: %v, %v", loc, ok)
	}
	var dst [2]uint32
	if err := DecodeSliceLE(dst[:], []byte{1, 2, 3}); !errors.Is(err, ErrBoundsViolation) {
		t.Fatalf("DecodeSliceLE short src = %v, want ErrBoundsViolation", err)
	}
}

func BenchmarkParseHeaderTrusted(b *testing.B) {
	secs := make([]Section, 64)
	for i := range secs {
		secs[i] = Section{Kind: SectionPool, ElemAlign: 8, Tag: uint32(i), Count: 1 << 14, Data: make([]byte, 8<<14)}
	}
	img, _ := AppendImage(nil, testHeader(), secs)
	b.SetBytes(int64(len(img)))
	for b.Loop() {
		if _, _, err := ParseHeader(img, VerifyTrustedHeader, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseHeaderChecksum(b *testing.B) {
	secs := make([]Section, 64)
	for i := range secs {
		secs[i] = Section{Kind: SectionPool, ElemAlign: 8, Tag: uint32(i), Count: 1 << 14, Data: make([]byte, 8<<14)}
	}
	img, _ := AppendImage(nil, testHeader(), secs)
	b.SetBytes(int64(len(img)))
	for b.Loop() {
		if _, _, err := ParseHeader(img, VerifyChecksum, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDuplicateSectionsRejected(t *testing.T) {
	h := testHeader()
	secs := testSections()
	secs = append(secs, Section{Kind: SectionPool, ElemAlign: 4, Tag: 3, Count: 1, Data: []byte{1, 2, 3, 4}})
	img, err := AppendImage(nil, h, secs)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseHeader(img, VerifyUntrusted, 0); !errors.Is(err, ErrCorruptHeader) {
		t.Fatalf(".cnf image with a duplicate section: %v", err)
	}
	cimg, _, err := AppendCompactImage(nil, h, secs)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseCompactImage(cimg, VerifyUntrusted, 0); !errors.Is(err, ErrCorruptHeader) {
		t.Fatalf("compact image with a duplicate section: %v", err)
	}
	many := make([]Section, 40)
	for i := range many {
		many[i] = Section{Kind: SectionPool, ElemAlign: 1, Tag: uint32(i)}
	}
	many[39].Tag = 5
	img, _ = AppendImage(nil, h, many)
	if _, _, err := ParseHeader(img, VerifyUntrusted, 0); !errors.Is(err, ErrCorruptHeader) {
		t.Fatalf("large directory with a duplicate section: %v", err)
	}
	many[39].Tag = 39
	img, _ = AppendImage(nil, h, many)
	if _, _, err := ParseHeader(img, VerifyUntrusted, 0); err != nil {
		t.Fatalf("large directory without duplicates: %v", err)
	}
}
