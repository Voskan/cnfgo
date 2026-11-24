package cnfrt

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// .cnf container constants.
const (
	Magic         = "CNFG"
	FormatVersion = 0x0002
	HeaderSize    = 64
	DirEntrySize  = 32
	SectionAlign  = 64
)

// Header flags.
const (
	// FlagCyclicAllowed marks snapshots produced by FreezeCyclic; the
	// acyclicity invariant is not enforced for them.
	FlagCyclicAllowed = 1 << 0
	// FlagFullStringIntern marks snapshots whose strings were deduplicated
	// exactly.
	FlagFullStringIntern = 1 << 1
	// FlagHasDict marks in-memory snapshots or compact values whose string
	// spans reference a process-local Dict. Standalone .cnf files do not
	// permit this flag.
	FlagHasDict = 1 << 2

	knownFlags = FlagCyclicAllowed | FlagFullStringIntern
)

// Section kinds.
const (
	SectionTable       = 1
	SectionPool        = 2
	SectionStrBlob     = 3
	SectionExt         = 4
	SectionFrozenCtrl  = 5
	SectionFrozenSlots = 6
	SectionFrozenData  = 7
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C returns the Castagnoli CRC of b (hardware accelerated on amd64
// and arm64).
func CRC32C(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// Header is the decoded 64-byte .cnf header.
type Header struct {
	Flags      uint32
	SchemaHash uint64
	HashSeed   uint64
	RootTable  uint32
	RootID     uint32
	TotalSize  uint64
}

// Section is a section to be written.
type Section struct {
	Kind      uint16
	ElemAlign uint16
	Tag       uint32
	Count     uint32
	Data      []byte
}

// SectionInfo is a section directory entry of a parsed image.
type SectionInfo struct {
	Kind      uint16
	ElemAlign uint16
	Tag       uint32
	Off       uint64
	Len       uint64
	Count     uint32
	CRC       uint32
}

func alignUp(n, a uint64) uint64 { return (n + a - 1) &^ (a - 1) }

// layoutImage computes section offsets and the total image size.
func layoutImage(h *Header, secs []Section) []SectionInfo {
	pos := alignUp(HeaderSize+uint64(len(secs))*DirEntrySize, SectionAlign)
	infos := make([]SectionInfo, len(secs))
	for i, s := range secs {
		infos[i] = SectionInfo{Kind: s.Kind, ElemAlign: s.ElemAlign, Tag: s.Tag, Off: pos, Len: uint64(len(s.Data)), Count: s.Count, CRC: CRC32C(s.Data)}
		pos = alignUp(pos+uint64(len(s.Data)), SectionAlign)
	}
	h.TotalSize = pos
	return infos
}

func encodeHeader(h Header, infos []SectionInfo) ([]byte, []byte) {
	dir := make([]byte, len(infos)*DirEntrySize)
	for i, in := range infos {
		e := dir[i*DirEntrySize:]
		binary.LittleEndian.PutUint16(e[0:], in.Kind)
		binary.LittleEndian.PutUint16(e[2:], in.ElemAlign)
		binary.LittleEndian.PutUint32(e[4:], in.Tag)
		binary.LittleEndian.PutUint64(e[8:], in.Off)
		binary.LittleEndian.PutUint64(e[16:], in.Len)
		binary.LittleEndian.PutUint32(e[24:], in.Count)
		binary.LittleEndian.PutUint32(e[28:], in.CRC)
	}
	hdr := make([]byte, HeaderSize)
	copy(hdr, Magic)
	binary.LittleEndian.PutUint16(hdr[4:], FormatVersion)
	hdr[6] = 0x01
	hdr[7] = 0x04
	binary.LittleEndian.PutUint32(hdr[8:], h.Flags)
	binary.LittleEndian.PutUint32(hdr[12:], uint32(len(infos)))
	binary.LittleEndian.PutUint64(hdr[16:], h.SchemaHash)
	binary.LittleEndian.PutUint64(hdr[24:], h.HashSeed)
	binary.LittleEndian.PutUint32(hdr[32:], h.RootTable)
	binary.LittleEndian.PutUint32(hdr[36:], h.RootID)
	binary.LittleEndian.PutUint64(hdr[40:], h.TotalSize)
	binary.LittleEndian.PutUint32(hdr[56:], CRC32C(dir))
	binary.LittleEndian.PutUint32(hdr[60:], CRC32C(hdr[:60]))
	return hdr, dir
}

var zeroPad [SectionAlign]byte

// AppendImage appends the complete .cnf image of the given header and
// sections to dst. Empty sections may be omitted by the caller; readers
// treat missing tags as empty.
func AppendImage(dst []byte, h Header, secs []Section) ([]byte, error) {
	infos := layoutImage(&h, secs)
	hdr, dir := encodeHeader(h, infos)
	start := len(dst)
	dst = append(dst, hdr...)
	dst = append(dst, dir...)
	for i, s := range secs {
		for uint64(len(dst)-start) < infos[i].Off {
			dst = append(dst, 0)
		}
		dst = append(dst, s.Data...)
	}
	for uint64(len(dst)-start) < h.TotalSize {
		dst = append(dst, 0)
	}
	return dst, nil
}

// ImageSize returns the size of the image AppendImage would produce.
func ImageSize(h Header, secs []Section) int {
	pos := alignUp(HeaderSize+uint64(len(secs))*DirEntrySize, SectionAlign)
	for _, s := range secs {
		pos = alignUp(pos+uint64(len(s.Data)), SectionAlign)
	}
	return int(pos)
}

// WriteImage streams the .cnf image to w and returns the number of bytes
// written.
func WriteImage(w io.Writer, h Header, secs []Section) (int64, error) {
	infos := layoutImage(&h, secs)
	hdr, dir := encodeHeader(h, infos)
	var n int64
	write := func(b []byte) error {
		k, err := w.Write(b)
		n += int64(k)
		return err
	}
	if err := write(hdr); err != nil {
		return n, err
	}
	if err := write(dir); err != nil {
		return n, err
	}
	for i, s := range secs {
		for uint64(n) < infos[i].Off {
			pad := min(infos[i].Off-uint64(n), SectionAlign)
			if err := write(zeroPad[:pad]); err != nil {
				return n, err
			}
		}
		if err := write(s.Data); err != nil {
			return n, err
		}
	}
	for uint64(n) < h.TotalSize {
		if err := write(zeroPad[:min(h.TotalSize-uint64(n), SectionAlign)]); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ParseHeader decodes and validates the header and section directory of a
// .cnf image. If schema is non-zero the schema hash must match. For modes
// VerifyChecksum and VerifyUntrusted every section CRC is also verified.
// The section geometry (bounds, alignment, ordering, element alignment) and
// the uniqueness of every (Kind, Tag) pair are always verified.
func ParseHeader(data []byte, mode VerifyMode, schema uint64) (Header, []SectionInfo, error) {
	var h Header
	if len(data) < HeaderSize {
		return h, nil, fmt.Errorf("%w: %d-byte image is shorter than the header", ErrCorruptHeader, len(data))
	}
	if string(data[:4]) != Magic {
		return h, nil, fmt.Errorf("%w: bad magic", ErrCorruptHeader)
	}
	if CRC32C(data[:60]) != binary.LittleEndian.Uint32(data[60:]) {
		return h, nil, fmt.Errorf("%w: header checksum", ErrChecksumMismatch)
	}
	if v := binary.LittleEndian.Uint16(data[4:]); v != FormatVersion {
		return h, nil, fmt.Errorf("%w: unsupported format version %#x", ErrCorruptHeader, v)
	}
	if data[6] != 0x01 || data[7] != 0x04 {
		return h, nil, fmt.Errorf("%w: unsupported endianness or index width", ErrCorruptHeader)
	}
	h.Flags = binary.LittleEndian.Uint32(data[8:])
	nsec := uint64(binary.LittleEndian.Uint32(data[12:]))
	h.SchemaHash = binary.LittleEndian.Uint64(data[16:])
	h.HashSeed = binary.LittleEndian.Uint64(data[24:])
	h.RootTable = binary.LittleEndian.Uint32(data[32:])
	h.RootID = binary.LittleEndian.Uint32(data[36:])
	h.TotalSize = binary.LittleEndian.Uint64(data[40:])
	if h.Flags&^knownFlags != 0 {
		return h, nil, fmt.Errorf("%w: unknown flags %#x", ErrCorruptHeader, h.Flags)
	}
	if binary.LittleEndian.Uint64(data[48:]) != 0 {
		return h, nil, fmt.Errorf("%w: reserved header bytes are not zero", ErrCorruptHeader)
	}
	if h.TotalSize != uint64(len(data)) {
		return h, nil, fmt.Errorf("%w: header size %d, image size %d", ErrCorruptHeader, h.TotalSize, len(data))
	}
	if schema != 0 && h.SchemaHash != schema {
		return h, nil, fmt.Errorf("%w: image %#x, schema %#x", ErrSchemaMismatch, h.SchemaHash, schema)
	}
	if nsec > (uint64(len(data))-HeaderSize)/DirEntrySize {
		return h, nil, fmt.Errorf("%w: section count %d", ErrCorruptHeader, nsec)
	}
	dirEnd := HeaderSize + nsec*DirEntrySize
	dir := data[HeaderSize:dirEnd]
	if CRC32C(dir) != binary.LittleEndian.Uint32(data[56:]) {
		return h, nil, fmt.Errorf("%w: directory checksum", ErrChecksumMismatch)
	}
	align := uint64(SectionAlign)
	infos := make([]SectionInfo, nsec)
	prevEnd := dirEnd
	fileLen := uint64(len(data))
	for i := range infos {
		e := dir[i*DirEntrySize:]
		in := SectionInfo{
			Kind:      binary.LittleEndian.Uint16(e[0:]),
			ElemAlign: binary.LittleEndian.Uint16(e[2:]),
			Tag:       binary.LittleEndian.Uint32(e[4:]),
			Off:       binary.LittleEndian.Uint64(e[8:]),
			Len:       binary.LittleEndian.Uint64(e[16:]),
			Count:     binary.LittleEndian.Uint32(e[24:]),
			CRC:       binary.LittleEndian.Uint32(e[28:]),
		}
		switch {
		case in.Off > fileLen || in.Len > fileLen-in.Off:
			return h, nil, fmt.Errorf("%w: section %d out of bounds", ErrBoundsViolation, i)
		case in.Off&(align-1) != 0:
			return h, nil, fmt.Errorf("%w: section %d misaligned in file", ErrBoundsViolation, i)
		case in.Off < prevEnd:
			return h, nil, fmt.Errorf("%w: section %d overlaps its predecessor", ErrBoundsViolation, i)
		case in.ElemAlign == 0 || in.ElemAlign&(in.ElemAlign-1) != 0 || uint64(in.ElemAlign) > align:
			return h, nil, fmt.Errorf("%w: section %d element alignment %d", ErrBoundsViolation, i, in.ElemAlign)
		}
		prevEnd = in.Off + in.Len
		if mode >= VerifyChecksum && CRC32C(data[in.Off:in.Off+in.Len]) != in.CRC {
			return h, nil, fmt.Errorf("%w: section %d (tag %d)", ErrChecksumMismatch, i, in.Tag)
		}
		infos[i] = in
	}
	if err := uniqueSections(infos); err != nil {
		return h, nil, err
	}
	return h, infos, nil
}

// uniqueSections rejects directories that list a (Kind, Tag) pair twice;
// a loader would otherwise silently use one of them.
func uniqueSections(infos []SectionInfo) error {
	type key struct {
		kind uint16
		tag  uint32
	}
	if len(infos) <= 32 {
		for i := range infos {
			for j := range i {
				if infos[i].Kind == infos[j].Kind && infos[i].Tag == infos[j].Tag {
					return fmt.Errorf("%w: sections %d and %d are both kind %d tag %d", ErrCorruptHeader, j, i, infos[i].Kind, infos[i].Tag)
				}
			}
		}
		return nil
	}
	seen := make(map[key]int, len(infos))
	for i, in := range infos {
		k := key{in.Kind, in.Tag}
		if j, dup := seen[k]; dup {
			return fmt.Errorf("%w: sections %d and %d are both kind %d tag %d", ErrCorruptHeader, j, i, in.Kind, in.Tag)
		}
		seen[k] = i
	}
	return nil
}

// SectionData returns the payload of a parsed section.
func SectionData(data []byte, in SectionInfo) []byte { return data[in.Off : in.Off+in.Len] }

// FindSection returns the section with the given kind and tag. A missing
// section is reported as found == false and treated as empty by readers.
func FindSection(infos []SectionInfo, kind uint16, tag uint32) (SectionInfo, bool) {
	for _, in := range infos {
		if in.Kind == kind && in.Tag == tag {
			return in, true
		}
	}
	return SectionInfo{}, false
}

// WriteSnapshotAtomic writes a file atomically: write is called with a
// buffered writer on a temporary file in the destination directory, which
// is then flushed, fsynced, renamed over path and followed by an fsync of
// the directory. Readers that memory-map path therefore never observe a
// partially written or truncated file.
func WriteSnapshotAtomic(path string, write func(w io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid()) + "." + hex.EncodeToString(rnd[:])
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	bw := bufio.NewWriterSize(f, 1<<20)
	if err = write(bw); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, derr := os.Open(dir)
	if derr != nil {
		return nil
	}
	defer d.Close()
	if serr := d.Sync(); serr != nil && !errors.Is(serr, os.ErrInvalid) {
		return serr
	}
	return nil
}

// ChunkedWriter streams a .cnf image whose section payloads are produced
// incrementally, for exports too large to assemble in memory. Sections are
// declared up front with their exact lengths; payloads are then written in
// declaration order and the directory checksums are patched in Close.
type ChunkedWriter struct {
	w      io.WriteSeeker
	h      Header
	infos  []SectionInfo
	cur    int
	pos    uint64
	crc    uint32
	err    error
	closed bool
}

// SectionSpec declares a section of a ChunkedWriter.
type SectionSpec struct {
	Kind      uint16
	ElemAlign uint16
	Tag       uint32
	Count     uint32
	Len       uint64
}

// NewChunkedWriter writes a provisional header and directory to w.
func NewChunkedWriter(w io.WriteSeeker, h Header, specs []SectionSpec) (*ChunkedWriter, error) {
	const align = SectionAlign
	pos := alignUp(HeaderSize+uint64(len(specs))*DirEntrySize, align)
	infos := make([]SectionInfo, len(specs))
	for i, s := range specs {
		infos[i] = SectionInfo{Kind: s.Kind, ElemAlign: s.ElemAlign, Tag: s.Tag, Count: s.Count, Off: pos, Len: s.Len}
		pos = alignUp(pos+s.Len, align)
	}
	h.TotalSize = pos
	cw := &ChunkedWriter{w: w, h: h, infos: infos, cur: -1}
	hdr, dir := encodeHeader(h, infos)
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	if _, err := w.Write(dir); err != nil {
		return nil, err
	}
	cw.pos = uint64(len(hdr) + len(dir))
	return cw, nil
}

func (cw *ChunkedWriter) pad(to uint64) error {
	for cw.pos < to {
		k, err := cw.w.Write(zeroPad[:min(to-cw.pos, SectionAlign)])
		cw.pos += uint64(k)
		if err != nil {
			return err
		}
	}
	return nil
}

func (cw *ChunkedWriter) finishSection() error {
	if cw.cur < 0 {
		return nil
	}
	in := &cw.infos[cw.cur]
	if cw.pos != in.Off+in.Len {
		return fmt.Errorf("cnfrt: section %d: wrote %d of %d bytes", cw.cur, cw.pos-in.Off, in.Len)
	}
	in.CRC = cw.crc
	return nil
}

// Write appends payload bytes, advancing to the next declared section when
// the current one is full. It returns ErrClosed after Close.
func (cw *ChunkedWriter) Write(p []byte) (int, error) {
	if cw.closed {
		return 0, ErrClosed
	}
	if cw.err != nil {
		return 0, cw.err
	}
	written := 0
	for len(p) > 0 {
		if cw.cur < 0 || cw.pos == cw.infos[cw.cur].Off+cw.infos[cw.cur].Len {
			if cw.err = cw.finishSection(); cw.err != nil {
				return written, cw.err
			}
			cw.cur++
			if cw.cur >= len(cw.infos) {
				cw.err = errors.New("cnfrt: ChunkedWriter: data beyond declared sections")
				return written, cw.err
			}
			if cw.err = cw.pad(cw.infos[cw.cur].Off); cw.err != nil {
				return written, cw.err
			}
			cw.crc = 0
			continue
		}
		in := cw.infos[cw.cur]
		chunk := p[:min(uint64(len(p)), in.Off+in.Len-cw.pos)]
		k, err := cw.w.Write(chunk)
		cw.crc = crc32.Update(cw.crc, castagnoli, chunk[:k])
		cw.pos += uint64(k)
		written += k
		p = p[k:]
		if err != nil {
			cw.err = err
			return written, err
		}
	}
	return written, nil
}

// Close finishes the last section, pads the image and rewrites the header
// and directory with the final checksums.
func (cw *ChunkedWriter) Close() (err error) {
	if cw.closed {
		return cw.err
	}
	cw.closed = true
	if cw.err != nil {
		return cw.err
	}
	defer func() {
		if err != nil {
			cw.err = err
		}
	}()
	for cw.cur < len(cw.infos)-1 {
		if err = cw.finishSection(); err != nil {
			return err
		}
		cw.cur++
		if err = cw.pad(cw.infos[cw.cur].Off); err != nil {
			return err
		}
		cw.crc = 0
	}
	if err = cw.finishSection(); err != nil {
		return err
	}
	if err = cw.pad(cw.h.TotalSize); err != nil {
		return err
	}
	hdr, dir := encodeHeader(cw.h, cw.infos)
	if _, err = cw.w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = cw.w.Write(hdr); err != nil {
		return err
	}
	if _, err = cw.w.Write(dir); err != nil {
		return err
	}
	_, err = cw.w.Seek(int64(cw.h.TotalSize), io.SeekStart)
	return err
}

// Ext side table encoding: a header of two uint32 counts, 16-byte location
// entries, 24-byte zoned address entries and the name bytes they reference.
const (
	extLocSize  = 16
	extZoneSize = 24
)

// EncodeExt encodes a snapshot's location and zoned address side tables.
func EncodeExt(locs []LocEntry, zoned []netip.Addr) []byte {
	if len(locs) == 0 && len(zoned) == 0 {
		return nil
	}
	names := 8 + len(locs)*extLocSize + len(zoned)*extZoneSize
	size := names
	for _, l := range locs {
		size += len(l.Name)
	}
	for _, a := range zoned {
		size += len(a.Zone())
	}
	b := make([]byte, names, size)
	binary.LittleEndian.PutUint32(b[0:], uint32(len(locs)))
	binary.LittleEndian.PutUint32(b[4:], uint32(len(zoned)))
	for i, l := range locs {
		e := b[8+i*extLocSize:]
		e[0] = l.Kind
		binary.LittleEndian.PutUint32(e[4:], uint32(l.Offset))
		binary.LittleEndian.PutUint32(e[8:], uint32(len(b)))
		binary.LittleEndian.PutUint32(e[12:], uint32(len(l.Name)))
		b = append(b, l.Name...)
	}
	for i, a := range zoned {
		e := b[8+len(locs)*extLocSize+i*extZoneSize:]
		ip := a.As16()
		copy(e[0:16], ip[:])
		binary.LittleEndian.PutUint32(e[16:], uint32(len(b)))
		binary.LittleEndian.PutUint32(e[20:], uint32(len(a.Zone())))
		b = append(b, a.Zone()...)
	}
	return b
}

func extCounts(ext string) (int, int, bool) {
	if len(ext) == 0 {
		return 0, 0, true
	}
	if len(ext) < 8 {
		return 0, 0, false
	}
	nl := uint64(binary.LittleEndian.Uint32(UnsafeStringBytes(ext[0:4])))
	nz := uint64(binary.LittleEndian.Uint32(UnsafeStringBytes(ext[4:8])))
	if 8+nl*extLocSize+nz*extZoneSize > uint64(len(ext)) {
		return 0, 0, false
	}
	return int(nl), int(nz), true
}

func extName(ext string, e string) (string, bool) {
	off := uint64(binary.LittleEndian.Uint32(UnsafeStringBytes(e[8:12])))
	n := uint64(binary.LittleEndian.Uint32(UnsafeStringBytes(e[12:16])))
	if off+n > uint64(len(ext)) {
		return "", false
	}
	return ext[off : off+n], true
}

// ExtLoc returns the 1-based location entry idx of an encoded ext table
// without allocating. The name aliases ext.
func ExtLoc(ext string, idx uint32) (kind uint8, name string, offset int32, ok bool) {
	nl, _, valid := extCounts(ext)
	if !valid || idx == 0 || int(idx) > nl {
		return 0, "", 0, false
	}
	e := ext[8+(int(idx)-1)*extLocSize:]
	name, ok = extName(ext, e[:16])
	return e[0], name, int32(binary.LittleEndian.Uint32(UnsafeStringBytes(e[4:8]))), ok
}

// ExtZone returns the zone name of the 1-based zoned address idx of an
// encoded ext table without allocating.
func ExtZone(ext string, idx uint16) (string, bool) {
	nl, nz, valid := extCounts(ext)
	if !valid || idx == 0 || int(idx) > nz {
		return "", false
	}
	e := ext[8+nl*extLocSize+(int(idx)-1)*extZoneSize:]
	off := uint64(binary.LittleEndian.Uint32(UnsafeStringBytes(e[16:20])))
	n := uint64(binary.LittleEndian.Uint32(UnsafeStringBytes(e[20:24])))
	if off+n > uint64(len(ext)) {
		return "", false
	}
	return ext[off : off+n], true
}

// ExtLocation resolves the 1-based location entry idx of an encoded ext
// table using the process-wide location caches.
func ExtLocation(ext string, idx uint32) (*time.Location, bool) {
	kind, name, off, ok := ExtLoc(ext, idx)
	if !ok {
		return nil, false
	}
	switch kind {
	case LocIANA:
		loc, err := ResolveLocation(name)
		return loc, err == nil
	case LocFixed:
		return FixedLocation(name, int(off)), true
	default:
		return nil, false
	}
}

// DecodeExt decodes and resolves an ext side table. Names are copied, so
// the result does not reference data.
func DecodeExt(data []byte) ([]LocEntry, []netip.Addr, error) {
	ext := UnsafeSealBytes(data)
	nl, nz, ok := extCounts(ext)
	if !ok || nz > 1<<16-1 {
		return nil, nil, fmt.Errorf("%w: ext table header", ErrBoundsViolation)
	}
	var locs []LocEntry
	if nl > 0 {
		locs = make([]LocEntry, nl)
	}
	for i := range locs {
		kind, name, off, ok := ExtLoc(ext, uint32(i+1))
		if !ok || (kind != LocFixed && kind != LocIANA) {
			return nil, nil, fmt.Errorf("%w: ext location %d", ErrBoundsViolation, i+1)
		}
		locs[i] = LocEntry{Kind: kind, Name: CloneStrings(name), Offset: off}
		if err := locs[i].Resolve(); err != nil {
			return nil, nil, fmt.Errorf("cnfrt: resolving location %q: %w", name, err)
		}
	}
	var zoned []netip.Addr
	if nz > 0 {
		zoned = make([]netip.Addr, nz)
	}
	for i := range zoned {
		zone, ok := ExtZone(ext, uint16(i+1))
		if !ok || zone == "" {
			return nil, nil, fmt.Errorf("%w: ext zoned address %d", ErrBoundsViolation, i+1)
		}
		e := ext[8+nl*extLocSize+i*extZoneSize:]
		zoned[i] = netip.AddrFrom16([16]byte(UnsafeStringBytes(e[0:16]))).WithZone(CloneStrings(zone))
	}
	return locs, zoned, nil
}
