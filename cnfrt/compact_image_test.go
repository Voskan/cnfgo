package cnfrt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestCompactImageRoundTrip(t *testing.T) {
	h, secs := testHeader(), testSections()
	prefix := []byte("prefix")
	buf, infos, err := AppendCompactImage(bytes.Clone(prefix), h, secs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf, prefix) {
		t.Fatal("prefix overwritten")
	}
	img := buf[len(prefix):]
	if len(img) != CompactImageSize(secs) || len(img)%CompactAlign != 0 {
		t.Fatalf("image size %d, CompactImageSize %d", len(img), CompactImageSize(secs))
	}
	if full := ImageSize(h, secs); len(img) >= full {
		t.Fatalf("compact image (%d bytes) is not smaller than the .cnf image (%d bytes)", len(img), full)
	}
	for _, mode := range []VerifyMode{VerifyTrustedHeader, VerifyChecksum, VerifyUntrusted} {
		got, parsed, err := ParseCompactImage(img, mode, h.SchemaHash)
		if err != nil {
			t.Fatalf("%v: %v", mode, err)
		}
		want := h
		want.TotalSize = uint64(len(img))
		if got != want || len(parsed) != len(secs) {
			t.Fatalf("%v: header %+v, %d sections", mode, got, len(parsed))
		}
		for i, in := range parsed {
			if in != infos[i] || !bytes.Equal(SectionData(img, in), secs[i].Data) || in.Count != secs[i].Count || in.Off%CompactAlign != 0 {
				t.Fatalf("%v: section %d = %+v, want %+v", mode, i, in, infos[i])
			}
		}
	}
	empty, infos, err := AppendCompactImage(nil, h, nil)
	if err != nil || len(infos) != 0 || len(empty) != 48 {
		t.Fatalf("empty image: %d bytes, %v", len(empty), err)
	}
	if _, _, err := ParseCompactImage(empty, VerifyChecksum, 0); err != nil {
		t.Fatal(err)
	}
}

func TestCompactImageRejects(t *testing.T) {
	h, secs := testHeader(), testSections()
	img, _, err := AppendCompactImage(nil, h, secs)
	if err != nil {
		t.Fatal(err)
	}
	dirEnd := CompactHeaderSize + len(secs)*CompactDirEntrySize
	// resign recomputes the header checksum after a mutation.
	resign := func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b[0:], CRC32C(b[4:dirEnd]))
		return b
	}
	cases := []struct {
		name   string
		data   []byte
		mode   VerifyMode
		schema uint64
		want   error
	}{
		{"short", img[:CompactHeaderSize-1], VerifyTrustedHeader, 0, ErrCorruptHeader},
		{"truncated", img[:len(img)-8], VerifyTrustedHeader, 0, ErrCorruptHeader},
		{"header bit flip", func() []byte { b := bytes.Clone(img); b[20] ^= 1; return b }(), VerifyTrustedHeader, 0, ErrChecksumMismatch},
		{"payload bit flip", func() []byte { b := bytes.Clone(img); b[len(b)-20] ^= 1; return b }(), VerifyChecksum, 0, ErrChecksumMismatch},
		{"schema", img, VerifyTrustedHeader, 1, ErrSchemaMismatch},
		{"unknown flag", func() []byte { b := bytes.Clone(img); b[8] |= 0x80; return resign(b) }(), VerifyTrustedHeader, 0, ErrCorruptHeader},
		{"reserved", func() []byte { b := bytes.Clone(img); b[42] = 1; return resign(b) }(), VerifyTrustedHeader, 0, ErrCorruptHeader},
		{"total size", func() []byte {
			b := bytes.Clone(img)
			binary.LittleEndian.PutUint32(b[12:], uint32(len(b)+8))
			return resign(b)
		}(), VerifyTrustedHeader, 0, ErrCorruptHeader},
		{"section count", func() []byte {
			b := bytes.Clone(img)
			binary.LittleEndian.PutUint16(b[40:], 0xffff)
			return b
		}(), VerifyTrustedHeader, 0, ErrCorruptHeader},
		{"section length", func() []byte {
			b := bytes.Clone(img)
			binary.LittleEndian.PutUint32(b[CompactHeaderSize+8:], 0xffffffff)
			return resign(b)
		}(), VerifyTrustedHeader, 0, ErrBoundsViolation},
		{"short section", func() []byte {
			b := bytes.Clone(img)
			binary.LittleEndian.PutUint32(b[CompactHeaderSize+8:], 40)
			return resign(b)
		}(), VerifyTrustedHeader, 0, ErrBoundsViolation},
		{"element alignment", func() []byte { b := bytes.Clone(img); b[CompactHeaderSize+1] = 3; return resign(b) }(), VerifyTrustedHeader, 0, ErrBoundsViolation},
		{"large alignment", func() []byte { b := bytes.Clone(img); b[CompactHeaderSize+1] = 16; return resign(b) }(), VerifyTrustedHeader, 0, ErrBoundsViolation},
	}
	for _, c := range cases {
		if _, _, err := ParseCompactImage(c.data, c.mode, c.schema); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	// A payload corruption is detected only when checksums are verified.
	b := bytes.Clone(img)
	b[len(b)-20] ^= 1
	if _, _, err := ParseCompactImage(b, VerifyTrustedHeader, 0); err != nil {
		t.Fatalf("VerifyTrustedHeader must not read the payload: %v", err)
	}
	if _, _, err := AppendCompactImage(nil, h, []Section{{Kind: SectionTable, ElemAlign: 8, Tag: 1 << 16}}); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("oversized tag accepted: %v", err)
	}
}
