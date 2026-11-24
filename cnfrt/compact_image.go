package cnfrt

import (
	"encoding/binary"
	"fmt"
)

// Compact images are the variant of the .cnf image embedded in generated
// Compact values. They carry the same header fields and sections, but use a
// smaller directory, because a compact value usually holds a single small
// object for which a 64-byte header and 32 bytes per section would dominate
// the payload:
//
//	[0..4]   CRC32C of bytes [4, directory end): header and directory
//	[4..8]   CRC32C of the section payloads, bytes [directory end, total)
//	[8..12]  Flags
//	[12..16] TotalSize
//	[16..24] SchemaHash
//	[24..32] HashSeed
//	[32..36] RootTable
//	[36..40] RootID
//	[40..42] SectionCount
//	[42..44] zero
//	directory: SectionCount entries of 12 bytes:
//	         Kind uint8 | ElemAlign uint8 | Tag uint16 | Count uint32 | Len uint32
//
// Section payloads follow the directory in order, each starting at the
// next multiple of 8 bytes; their offsets are implied by the lengths, and
// all padding bytes are zero. All integers are little-endian.
const (
	CompactHeaderSize   = 44
	CompactDirEntrySize = 12
	CompactAlign        = 8
)

func compactLayoutInto(secs []Section, dst []SectionInfo) ([]SectionInfo, uint64) {
	pos := alignUp(CompactHeaderSize+uint64(len(secs))*CompactDirEntrySize, CompactAlign)
	var infos []SectionInfo
	if cap(dst) >= len(secs) {
		infos = dst[:len(secs)]
	} else {
		infos = make([]SectionInfo, len(secs))
	}
	for i, s := range secs {
		infos[i] = SectionInfo{Kind: s.Kind, ElemAlign: s.ElemAlign, Tag: s.Tag, Off: pos, Len: uint64(len(s.Data)), Count: s.Count}
		pos = alignUp(pos+uint64(len(s.Data)), CompactAlign)
	}
	return infos, pos
}

// CompactImageSize returns the size of the compact image of secs.
func CompactImageSize(secs []Section) int {
	pos := alignUp(CompactHeaderSize+uint64(len(secs))*CompactDirEntrySize, CompactAlign)
	for _, s := range secs {
		pos = alignUp(pos+uint64(len(s.Data)), CompactAlign)
	}
	return int(pos)
}

// AppendCompactImage appends the compact image of h and secs to dst and
// returns the extended buffer and the layout of the sections, with offsets
// relative to the start of the image. h.TotalSize is ignored.
func AppendCompactImage(dst []byte, h Header, secs []Section) ([]byte, []SectionInfo, error) {
	return AppendCompactImageInto(dst, h, secs, nil)
}

// AppendCompactImageInto is like AppendCompactImage but writes the section
// layout into infoDst when its capacity is sufficient, avoiding a heap
// allocation.
func AppendCompactImageInto(dst []byte, h Header, secs []Section, infoDst []SectionInfo) ([]byte, []SectionInfo, error) {
	infos, total := compactLayoutInto(secs, infoDst)
	if total > MaxBlob || len(secs) > 0xffff {
		return nil, nil, ErrCapacityExceeded
	}
	for _, s := range secs {
		if s.Kind > 0xff || s.ElemAlign > CompactAlign || s.Tag > 0xffff {
			return nil, nil, fmt.Errorf("%w: section kind %d tag %d does not fit a compact directory", ErrCapacityExceeded, s.Kind, s.Tag)
		}
	}
	start := len(dst)
	var hdr [CompactHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[8:], h.Flags)
	binary.LittleEndian.PutUint32(hdr[12:], uint32(total))
	binary.LittleEndian.PutUint64(hdr[16:], h.SchemaHash)
	binary.LittleEndian.PutUint64(hdr[24:], h.HashSeed)
	binary.LittleEndian.PutUint32(hdr[32:], h.RootTable)
	binary.LittleEndian.PutUint32(hdr[36:], h.RootID)
	binary.LittleEndian.PutUint16(hdr[40:], uint16(len(secs)))
	dst = append(dst, hdr[:]...)
	var e [CompactDirEntrySize]byte
	for _, s := range secs {
		e[0], e[1] = byte(s.Kind), byte(s.ElemAlign)
		binary.LittleEndian.PutUint16(e[2:], uint16(s.Tag))
		binary.LittleEndian.PutUint32(e[4:], s.Count)
		binary.LittleEndian.PutUint32(e[8:], uint32(len(s.Data)))
		dst = append(dst, e[:]...)
	}
	dirEnd := len(dst) - start
	for i, s := range secs {
		for uint64(len(dst)-start) < infos[i].Off {
			dst = append(dst, 0)
		}
		dst = append(dst, s.Data...)
	}
	for uint64(len(dst)-start) < total {
		dst = append(dst, 0)
	}
	img := dst[start:]
	binary.LittleEndian.PutUint32(img[4:], CRC32C(img[dirEnd:]))
	binary.LittleEndian.PutUint32(img[0:], CRC32C(img[4:dirEnd]))
	return dst, infos, nil
}

// ParseCompactImage decodes and validates the header and directory of a
// compact image. If schema is non-zero the schema hash must match. The
// header checksum, the section geometry and the uniqueness of every
// (Kind, Tag) pair are always verified; for modes
// VerifyChecksum and VerifyUntrusted the payload checksum is verified too.
// The CRC fields of the returned sections are zero.
func ParseCompactImage(data []byte, mode VerifyMode, schema uint64) (Header, []SectionInfo, error) {
	return parseCompactImage(data, mode, schema, nil, true)
}

// ParseCompactHeaderFast decodes the header and section directory of a
// previously validated compact image into infoDst without recomputing
// CRC32C checksums or allocating when infoDst has enough capacity. It still
// enforces all structural bounds, alignment and schema checks.
func ParseCompactHeaderFast(data []byte, schema uint64, infoDst []SectionInfo) (Header, []SectionInfo, error) {
	return parseCompactImage(data, VerifyTrustedHeader, schema, infoDst, false)
}

func parseCompactImage(data []byte, mode VerifyMode, schema uint64, infoDst []SectionInfo, checkCRC bool) (Header, []SectionInfo, error) {
	var h Header
	if len(data) < CompactHeaderSize {
		return h, nil, fmt.Errorf("%w: %d-byte compact image is shorter than the header", ErrCorruptHeader, len(data))
	}
	nsec := uint64(binary.LittleEndian.Uint16(data[40:]))
	if nsec > (uint64(len(data))-CompactHeaderSize)/CompactDirEntrySize {
		return h, nil, fmt.Errorf("%w: section count %d", ErrCorruptHeader, nsec)
	}
	dirEnd := CompactHeaderSize + nsec*CompactDirEntrySize
	if checkCRC && CRC32C(data[4:dirEnd]) != binary.LittleEndian.Uint32(data[0:]) {
		return h, nil, fmt.Errorf("%w: compact header checksum", ErrChecksumMismatch)
	}
	h.Flags = binary.LittleEndian.Uint32(data[8:])
	h.TotalSize = uint64(binary.LittleEndian.Uint32(data[12:]))
	h.SchemaHash = binary.LittleEndian.Uint64(data[16:])
	h.HashSeed = binary.LittleEndian.Uint64(data[24:])
	h.RootTable = binary.LittleEndian.Uint32(data[32:])
	h.RootID = binary.LittleEndian.Uint32(data[36:])
	switch {
	case h.Flags&^knownFlags != 0:
		return h, nil, fmt.Errorf("%w: unknown flags %#x", ErrCorruptHeader, h.Flags)
	case binary.LittleEndian.Uint16(data[42:]) != 0:
		return h, nil, fmt.Errorf("%w: reserved header bytes are not zero", ErrCorruptHeader)
	case h.TotalSize != uint64(len(data)):
		return h, nil, fmt.Errorf("%w: header size %d, image size %d", ErrCorruptHeader, h.TotalSize, len(data))
	case schema != 0 && h.SchemaHash != schema:
		return h, nil, fmt.Errorf("%w: image %#x, schema %#x", ErrSchemaMismatch, h.SchemaHash, schema)
	}
	var infos []SectionInfo
	if cap(infoDst) >= int(nsec) {
		infos = infoDst[:nsec]
	} else {
		infos = make([]SectionInfo, nsec)
	}
	pos := alignUp(dirEnd, CompactAlign)
	for i := range infos {
		e := data[CompactHeaderSize+uint64(i)*CompactDirEntrySize:]
		in := SectionInfo{
			Kind:      uint16(e[0]),
			ElemAlign: uint16(e[1]),
			Tag:       uint32(binary.LittleEndian.Uint16(e[2:])),
			Count:     binary.LittleEndian.Uint32(e[4:]),
			Off:       pos,
			Len:       uint64(binary.LittleEndian.Uint32(e[8:])),
		}
		switch {
		case in.Off > h.TotalSize || in.Len > h.TotalSize-in.Off:
			return h, nil, fmt.Errorf("%w: section %d out of bounds", ErrBoundsViolation, i)
		case in.ElemAlign == 0 || in.ElemAlign&(in.ElemAlign-1) != 0 || in.ElemAlign > CompactAlign:
			return h, nil, fmt.Errorf("%w: section %d element alignment %d", ErrBoundsViolation, i, in.ElemAlign)
		}
		pos = alignUp(in.Off+in.Len, CompactAlign)
		infos[i] = in
	}
	if pos != h.TotalSize {
		return h, nil, fmt.Errorf("%w: sections end at %d, image size %d", ErrBoundsViolation, pos, h.TotalSize)
	}
	if err := uniqueSections(infos); err != nil {
		return h, nil, err
	}
	if checkCRC && mode >= VerifyChecksum && CRC32C(data[dirEnd:]) != binary.LittleEndian.Uint32(data[4:]) {
		return h, nil, fmt.Errorf("%w: compact payload", ErrChecksumMismatch)
	}
	return h, infos, nil
}

// Slim compact format (version 0x03) constants.
//
// A slim compact value stores the root record once at byte offset 1, followed
// by a fixed strBase offset, optional dictToken / ext / hashSeed fields, the
// 64-bit schema hash, a compile-time-sized section presence bitmask, unsigned
// varint element counts for present sections only, section payloads ordered by
// descending element alignment (8, 4, 2, 1), and a trailing 4-byte CRC32C.
//
// The dictToken is the 8-byte little-endian Dict.Token of the dictionary
// holding the value's dictionary strings: a 16-bit registry slot in the low
// bits and a non-zero 48-bit random nonce in the high bits.
const (
	// SlimCompactVersion is the 3-bit format version in byte 0 of a slim
	// compact value.
	SlimCompactVersion = 3

	// SlimFlagExt marks a slim compact value with a non-empty ext table
	// (extOff and extLen follow strBase/dictToken in the header).
	SlimFlagExt = 1 << 3
	// SlimFlagDict marks a slim compact value whose strings may reference a
	// shared Dict (the 8-byte dictToken follows strBase in the header).
	SlimFlagDict = 1 << 4
	// SlimFlagCyclic marks a slim compact value whose embedded snapshot permits
	// cycles (FlagCyclicAllowed).
	SlimFlagCyclic = 1 << 5
	// SlimFlagSeed marks a slim compact value carrying an explicit 64-bit hash
	// seed (otherwise DefaultSeed is implied).
	SlimFlagSeed = 1 << 6
	// SlimFlagReserved is reserved and must be zero.
	SlimFlagReserved = 1 << 7
)

// SlimSectionSpec describes one statically known section slot of a slim
// compact schema. Specs must be ordered by descending ElemAlign (8, 4, 2, 1)
// so that at most one alignment pad is needed before the first section.
type SlimSectionSpec struct {
	Kind      uint16
	ElemAlign uint16
	Tag       uint32
	ElemSize  uint32
}

func uvarintLen(x uint64) int {
	n := 1
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}

// SlimDictTokenSize is the size of the dictToken field of a slim compact
// header.
const SlimDictTokenSize = 8

// U32S reads a little-endian uint32 from the first four bytes of s.
func U32S(s string) uint32 {
	_ = s[3]
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

// ParseOption configures how generated Parse and Borrow functions accept a
// Compact value.
type ParseOption struct {
	dict *Dict
}

// AllowDict permits a Compact value verified with VerifyUntrusted to
// reference strings in d. Without it, VerifyUntrusted rejects values that
// use a Dict with ErrDictRequired, so external bytes can never read strings
// from a dictionary of this process.
func AllowDict(d *Dict) ParseOption { return ParseOption{dict: d} }

// AllowedDict returns the Dict permitted by opts, or nil.
func AllowedDict(opts []ParseOption) *Dict {
	var d *Dict
	for _, o := range opts {
		if o.dict != nil {
			d = o.dict
		}
	}
	return d
}

// resolveSlimDict returns the Dict referenced by token. With VerifyUntrusted
// only the explicitly allowed Dict is accepted.
func resolveSlimDict(token uint64, mode VerifyMode, allow *Dict) (*Dict, error) {
	if mode >= VerifyUntrusted {
		switch {
		case allow == nil:
			return nil, ErrDictRequired
		case allow.Closed():
			return nil, ErrUnknownDict
		case token == 0 || token != allow.Token():
			return nil, ErrDictRequired
		}
		return allow, nil
	}
	d := LookupDictToken(token)
	if d == nil {
		return nil, ErrUnknownDict
	}
	return d, nil
}

// CheckSlimCompactPrefix validates the header prefix of a slim compact value
// without decoding its sections or verifying its CRC. hdr must be 1+rootSize.
// If schema is non-zero, the stored schema hash must equal it. If
// SlimFlagDict is set, the referenced Dict must be open in this process;
// ErrUnknownDict is returned otherwise.
func CheckSlimCompactPrefix(raw string, hdr int, schema uint64) error {
	if len(raw) >= hdr+16 && len(raw) <= MaxBlob {
		switch raw[0] {
		case SlimCompactVersion:
			h := raw[hdr : hdr+12]
			if r4s(h) <= uint64(len(raw)-4) && (schema == 0 || r8s(h[4:]) == schema) {
				return nil
			}
		case SlimCompactVersion | SlimFlagDict:
			if len(raw) >= hdr+24 {
				h := raw[hdr : hdr+20]
				if r4s(h) <= uint64(len(raw)-4) && LookupDictToken(r8s(h[4:12])) != nil && (schema == 0 || r8s(h[12:20]) == schema) {
					return nil
				}
			}
		}
	}
	return checkSlimCompactPrefixSlow(raw, hdr, schema)
}

// CheckSlimCompactString validates raw as CheckSlimCompactPrefix does,
// accepting an empty string as the zero Compact value. On success it returns
// (raw, nil); on error it returns ("", err).
//
//go:noinline
func CheckSlimCompactString(raw string, hdr int, schema uint64) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) >= hdr+16 && len(raw) <= MaxBlob {
		switch raw[0] {
		case SlimCompactVersion:
			h := raw[hdr : hdr+12]
			if r4s(h) <= uint64(len(raw)-4) && (schema == 0 || r8s(h[4:]) == schema) {
				return raw, nil
			}
		case SlimCompactVersion | SlimFlagDict:
			if len(raw) >= hdr+24 {
				h := raw[hdr : hdr+20]
				if r4s(h) <= uint64(len(raw)-4) && LookupDictToken(r8s(h[4:12])) != nil && (schema == 0 || r8s(h[12:20]) == schema) {
					return raw, nil
				}
			}
		}
	}
	if err := checkSlimCompactPrefixSlow(raw, hdr, schema); err != nil {
		return "", err
	}
	return raw, nil
}

// CheckSlimCompactBytes is CheckSlimCompactString for a byte slice without
// copying. The returned string aliases raw.
//
//go:noinline
func CheckSlimCompactBytes(raw []byte, hdr int, schema uint64) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	s := UnsafeSealBytes(raw)
	if len(s) >= hdr+16 && len(s) <= MaxBlob {
		switch s[0] {
		case SlimCompactVersion:
			h := s[hdr : hdr+12]
			if r4s(h) <= uint64(len(s)-4) && (schema == 0 || r8s(h[4:]) == schema) {
				return s, nil
			}
		case SlimCompactVersion | SlimFlagDict:
			if len(s) >= hdr+24 {
				h := s[hdr : hdr+20]
				if r4s(h) <= uint64(len(s)-4) && LookupDictToken(r8s(h[4:12])) != nil && (schema == 0 || r8s(h[12:20]) == schema) {
					return s, nil
				}
			}
		}
	}
	if err := checkSlimCompactPrefixSlow(s, hdr, schema); err != nil {
		return "", err
	}
	return s, nil
}

//go:noinline
func checkSlimCompactPrefixSlow(raw string, hdr int, schema uint64) error {
	if len(raw) < hdr+16 || len(raw) > MaxBlob {
		return fmt.Errorf("%w: invalid slim compact length %d", ErrCorruptHeader, len(raw))
	}
	flags := raw[0]
	if flags&0x07 != SlimCompactVersion || flags&SlimFlagReserved != 0 {
		return fmt.Errorf("%w: unsupported slim compact header %#x", ErrCorruptHeader, flags)
	}
	limit := uint64(len(raw) - 4)
	strBase := r4s(raw[hdr : hdr+4])
	if strBase > limit {
		return fmt.Errorf("%w: compact string offset", ErrBoundsViolation)
	}
	pos := uint64(hdr + 4)
	if flags&SlimFlagDict != 0 {
		if pos+SlimDictTokenSize > limit {
			return fmt.Errorf("%w: truncated compact dict header", ErrCorruptHeader)
		}
		if LookupDictToken(r8s(raw[pos:pos+SlimDictTokenSize])) == nil {
			return ErrUnknownDict
		}
		pos += SlimDictTokenSize
	}
	if flags&SlimFlagExt != 0 {
		if pos+8 > limit {
			return fmt.Errorf("%w: truncated compact ext header", ErrCorruptHeader)
		}
		extOff := r4s(raw[pos : pos+4])
		extLen := r4s(raw[pos+4 : pos+8])
		if extOff > limit || extLen > limit-extOff {
			return fmt.Errorf("%w: compact ext offsets", ErrBoundsViolation)
		}
		pos += 8
	}
	if flags&SlimFlagSeed != 0 {
		if pos+8 > limit {
			return fmt.Errorf("%w: truncated compact seed header", ErrCorruptHeader)
		}
		pos += 8
	}
	if pos+8 > limit {
		return fmt.Errorf("%w: truncated compact schema hash", ErrCorruptHeader)
	}
	if got := r8s(raw[pos : pos+8]); schema != 0 && got != schema {
		return fmt.Errorf("%w: image %#x, schema %#x", ErrSchemaMismatch, got, schema)
	}
	return nil
}

// SlimCompactSeed returns the hash seed of the slim compact value raw whose
// root record is hdr-1 bytes long: the explicit seed if SlimFlagSeed is set,
// and DefaultSeed otherwise. raw must have passed CheckSlimCompactPrefix.
func SlimCompactSeed(raw string, hdr int) uint64 {
	flags := raw[0]
	if flags&SlimFlagSeed == 0 {
		return DefaultSeed
	}
	pos := hdr + 4
	if flags&SlimFlagDict != 0 {
		pos += SlimDictTokenSize
	}
	if flags&SlimFlagExt != 0 {
		pos += 8
	}
	return binary.LittleEndian.Uint64([]byte(raw[pos : pos+8]))
}

// AppendSlimCompact encodes a slim compact value (version 0x03) to dst and
// returns the extended slice. rootBytes is the single wire-encoded root record.
// dictToken is the Dict.Token stored when flags has SlimFlagDict.
// secs holds the non-empty auxiliary sections (extra root nodes if any, other
// tables, pools, string blob, ext table), each of which must correspond to an
// entry in specs.
func AppendSlimCompact(dst []byte, flags uint8, rootBytes []byte, dictToken uint64, seed uint64, schema uint64, specs []SlimSectionSpec, secs []Section) ([]byte, error) {
	flags = (flags &^ (0x07 | SlimFlagReserved)) | SlimCompactVersion

	// Match each spec to its section index (-1 if absent).
	var smallMatch [64]int
	var match []int
	if len(specs) <= len(smallMatch) {
		match = smallMatch[:len(specs)]
	} else {
		match = make([]int, len(specs))
	}
	for i := range match {
		match[i] = -1
	}
	matchedSecs := 0
	hasExt := false
	for si, s := range secs {
		if len(s.Data) == 0 {
			continue
		}
		found := false
		for pi, sp := range specs {
			if sp.Kind == s.Kind && sp.Tag == s.Tag {
				if match[pi] >= 0 {
					return nil, fmt.Errorf("%w: duplicate slim section kind %d tag %d", ErrCorruptHeader, s.Kind, s.Tag)
				}
				if uint64(len(s.Data)) != uint64(s.Count)*uint64(sp.ElemSize) {
					return nil, fmt.Errorf("%w: slim section kind %d tag %d size mismatch", ErrBoundsViolation, s.Kind, s.Tag)
				}
				match[pi] = si
				matchedSecs++
				if s.Kind == SectionExt {
					hasExt = true
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: unexpected slim section kind %d tag %d", ErrCorruptHeader, s.Kind, s.Tag)
		}
	}
	if hasExt {
		flags |= SlimFlagExt
	} else {
		flags &^= SlimFlagExt
	}
	if flags&SlimFlagDict != 0 && (dictToken == 0 || dictToken>>16 == 0) {
		return nil, ErrUnknownDict
	}

	// Precompute exact wire size so we perform at most one allocation.
	hdr := 1 + len(rootBytes)
	pos := uint64(hdr + 4) // strBase
	if flags&SlimFlagDict != 0 {
		pos += SlimDictTokenSize
	}
	extHdrOff := uint64(0)
	if flags&SlimFlagExt != 0 {
		extHdrOff = pos
		pos += 8
	}
	if flags&SlimFlagSeed != 0 {
		pos += 8
	}
	pos += 8 // schemaHash
	maskBytes := (len(specs) + 7) / 8
	pos += uint64(maskBytes)
	for _, si := range match {
		if si < 0 {
			continue
		}
		pos += uint64(uvarintLen(uint64(secs[si].Count)))
	}
	for pi, si := range match {
		if si < 0 {
			continue
		}
		align := uint64(specs[pi].ElemAlign)
		if align > 1 {
			pos = alignUp(pos, align)
		}
		pos += uint64(len(secs[si].Data))
	}
	total := pos + 4 // trailing CRC32C
	if total > MaxBlob {
		return nil, ErrCapacityExceeded
	}

	start := len(dst)
	if cap(dst)-start < int(total) {
		nb := make([]byte, start, start+int(total))
		copy(nb, dst)
		dst = nb
	}

	dst = append(dst, flags)
	dst = append(dst, rootBytes...)
	dst = append(dst, 0, 0, 0, 0) // strBase placeholder
	if flags&SlimFlagDict != 0 {
		dst = binary.LittleEndian.AppendUint64(dst, dictToken)
	}
	if flags&SlimFlagExt != 0 {
		dst = append(dst, 0, 0, 0, 0, 0, 0, 0, 0) // extOff, extLen placeholder
	}
	if flags&SlimFlagSeed != 0 {
		dst = binary.LittleEndian.AppendUint64(dst, seed)
	}
	dst = binary.LittleEndian.AppendUint64(dst, schema)

	// Section presence bitmask.
	maskStart := len(dst)
	for range maskBytes {
		dst = append(dst, 0)
	}
	for pi, si := range match {
		if si >= 0 {
			dst[maskStart+pi/8] |= 1 << (uint(pi) % 8)
		}
	}

	// Element counts for present sections.
	var vbuf [binary.MaxVarintLen64]byte
	for _, si := range match {
		if si < 0 {
			continue
		}
		n := binary.PutUvarint(vbuf[:], uint64(secs[si].Count))
		dst = append(dst, vbuf[:n]...)
	}

	// Aligned section payloads.
	var strOff, extOff, extLen uint32
	for pi, si := range match {
		if si < 0 {
			continue
		}
		align := uint64(specs[pi].ElemAlign)
		if align > 1 {
			cur := uint64(len(dst) - start)
			target := alignUp(cur, align)
			for cur < target {
				dst = append(dst, 0)
				cur++
			}
		}
		secOff := uint32(len(dst) - start)
		s := secs[si]
		dst = append(dst, s.Data...)
		switch s.Kind {
		case SectionStrBlob:
			strOff = secOff
		case SectionExt:
			extOff = secOff
			extLen = uint32(len(s.Data))
		}
	}

	// Patch strBase and optional extOff/extLen.
	img := dst[start:]
	binary.LittleEndian.PutUint32(img[hdr:], strOff)
	if flags&SlimFlagExt != 0 {
		binary.LittleEndian.PutUint32(img[extHdrOff:], extOff)
		binary.LittleEndian.PutUint32(img[extHdrOff+4:], extLen)
	}

	// Trailing CRC32C covering everything before the checksum.
	crc := CRC32C(img)
	dst = binary.LittleEndian.AppendUint32(dst, crc)
	return dst, nil
}

// ParseSlimCompact decodes and validates a slim compact value (version 0x03)
// and returns its header, the Dict its strings reference (nil without
// SlimFlagDict) and its section layout. When checkCRC is true and
// mode >= VerifyChecksum, the trailing CRC32C is verified. With
// VerifyUntrusted, a value using a Dict is accepted only if its token is
// that of allow, which must be open (ErrDictRequired or ErrUnknownDict
// otherwise); in other modes the token is looked up in the process
// registry. Section metadata is written into infoDst when its capacity
// suffices.
func ParseSlimCompact(data []byte, rootSize int, schema uint64, rootTable uint32, specs []SlimSectionSpec, mode VerifyMode, checkCRC bool, allow *Dict, infoDst []SectionInfo) (Header, *Dict, []SectionInfo, error) {
	var h Header
	hdr := 1 + rootSize
	if len(data) < hdr+16 || len(data) > MaxBlob {
		return h, nil, nil, fmt.Errorf("%w: %d-byte slim compact image is invalid", ErrCorruptHeader, len(data))
	}
	flags := data[0]
	if flags&0x07 != SlimCompactVersion || flags&SlimFlagReserved != 0 {
		return h, nil, nil, fmt.Errorf("%w: unsupported slim compact flags %#x", ErrCorruptHeader, flags)
	}
	limit := uint64(len(data) - 4)
	if checkCRC && mode >= VerifyChecksum {
		want := binary.LittleEndian.Uint32(data[limit:])
		if CRC32C(data[:limit]) != want {
			return h, nil, nil, fmt.Errorf("%w: slim compact checksum", ErrChecksumMismatch)
		}
	}

	strBase := binary.LittleEndian.Uint32(data[hdr : hdr+4])
	pos := uint64(hdr + 4)

	var dict *Dict
	if flags&SlimFlagDict != 0 {
		if pos+SlimDictTokenSize > limit {
			return h, nil, nil, fmt.Errorf("%w: truncated slim dict header", ErrCorruptHeader)
		}
		var err error
		if dict, err = resolveSlimDict(binary.LittleEndian.Uint64(data[pos:pos+SlimDictTokenSize]), mode, allow); err != nil {
			return h, nil, nil, err
		}
		pos += SlimDictTokenSize
	}

	var extOff, extLen uint32
	if flags&SlimFlagExt != 0 {
		if pos+8 > limit {
			return h, nil, nil, fmt.Errorf("%w: truncated slim ext header", ErrCorruptHeader)
		}
		extOff = binary.LittleEndian.Uint32(data[pos : pos+4])
		extLen = binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		pos += 8
	}

	seed := uint64(DefaultSeed)
	if flags&SlimFlagSeed != 0 {
		if pos+8 > limit {
			return h, nil, nil, fmt.Errorf("%w: truncated slim seed header", ErrCorruptHeader)
		}
		seed = binary.LittleEndian.Uint64(data[pos : pos+8])
		pos += 8
	}

	if pos+8 > limit {
		return h, nil, nil, fmt.Errorf("%w: truncated slim schema hash", ErrCorruptHeader)
	}
	gotSchema := binary.LittleEndian.Uint64(data[pos : pos+8])
	pos += 8
	if schema != 0 && gotSchema != schema {
		return h, nil, nil, fmt.Errorf("%w: image %#x, schema %#x", ErrSchemaMismatch, gotSchema, schema)
	}

	maskBytes := uint64((len(specs) + 7) / 8)
	if pos+maskBytes > limit {
		return h, nil, nil, fmt.Errorf("%w: truncated slim section mask", ErrCorruptHeader)
	}
	mask := data[pos : pos+maskBytes]
	pos += maskBytes
	if rem := len(specs) % 8; rem != 0 {
		if mask[len(mask)-1]&^byte((1<<uint(rem))-1) != 0 {
			return h, nil, nil, fmt.Errorf("%w: reserved slim mask bits set", ErrCorruptHeader)
		}
	}

	nPresent := 0
	for i := range specs {
		if mask[i/8]&(1<<(uint(i)%8)) != 0 {
			nPresent++
		}
	}
	var infos []SectionInfo
	if cap(infoDst) >= nPresent {
		infos = infoDst[:nPresent]
	} else {
		infos = make([]SectionInfo, nPresent)
	}

	// Decode canonical varint counts for all present sections.
	idx := 0
	for i, sp := range specs {
		if mask[i/8]&(1<<(uint(i)%8)) == 0 {
			continue
		}
		cnt, n := binary.Uvarint(data[pos:limit])
		if n <= 0 || (n > 1 && data[pos+uint64(n)-1] == 0) {
			return h, nil, nil, fmt.Errorf("%w: invalid slim section count varint", ErrCorruptHeader)
		}
		pos += uint64(n)
		maxCnt := uint64(MaxElems)
		if sp.Kind == SectionStrBlob || sp.Kind == SectionExt {
			maxCnt = MaxBlob
		}
		if cnt == 0 || cnt > maxCnt {
			return h, nil, nil, fmt.Errorf("%w: slim section %d count %d", ErrBoundsViolation, i, cnt)
		}
		byteLen := cnt * uint64(sp.ElemSize)
		if sp.ElemSize == 0 || byteLen/uint64(sp.ElemSize) != cnt || byteLen > MaxBlob {
			return h, nil, nil, fmt.Errorf("%w: slim section %d size overflow", ErrBoundsViolation, i)
		}
		infos[idx] = SectionInfo{
			Kind:      sp.Kind,
			ElemAlign: sp.ElemAlign,
			Tag:       sp.Tag,
			Len:       byteLen,
			Count:     uint32(cnt),
		}
		idx++
	}

	// Assign aligned offsets and validate exact bounds and zero padding.
	var foundStr, foundExt bool
	var gotStrOff, gotExtOff, gotExtLen uint64
	for i := range infos {
		in := &infos[i]
		align := uint64(in.ElemAlign)
		if align == 0 || align&(align-1) != 0 || align > CompactAlign {
			return h, nil, nil, fmt.Errorf("%w: slim section %d alignment %d", ErrBoundsViolation, i, align)
		}
		if align > 1 {
			aligned := alignUp(pos, align)
			if aligned > limit {
				return h, nil, nil, fmt.Errorf("%w: slim section %d padding out of bounds", ErrBoundsViolation, i)
			}
			for p := pos; p < aligned; p++ {
				if data[p] != 0 {
					return h, nil, nil, fmt.Errorf("%w: non-zero slim alignment padding", ErrBoundsViolation)
				}
			}
			pos = aligned
		}
		if in.Len > limit-pos {
			return h, nil, nil, fmt.Errorf("%w: slim section %d out of bounds", ErrBoundsViolation, i)
		}
		in.Off = pos
		pos += in.Len
		switch in.Kind {
		case SectionStrBlob:
			foundStr = true
			gotStrOff = in.Off
		case SectionExt:
			foundExt = true
			gotExtOff = in.Off
			gotExtLen = in.Len
		}
	}
	if pos != limit {
		return h, nil, nil, fmt.Errorf("%w: slim sections end at %d, payload limit %d", ErrBoundsViolation, pos, limit)
	}

	// Verify header offsets match the actual section positions.
	if foundStr {
		if uint64(strBase) != gotStrOff {
			return h, nil, nil, fmt.Errorf("%w: slim strBase mismatch", ErrBoundsViolation)
		}
	} else if strBase != 0 {
		return h, nil, nil, fmt.Errorf("%w: unexpected slim strBase %d", ErrBoundsViolation, strBase)
	}
	if flags&SlimFlagExt != 0 {
		if !foundExt || uint64(extOff) != gotExtOff || uint64(extLen) != gotExtLen {
			return h, nil, nil, fmt.Errorf("%w: slim ext header mismatch", ErrBoundsViolation)
		}
	} else if foundExt {
		return h, nil, nil, fmt.Errorf("%w: unexpected slim ext section", ErrBoundsViolation)
	}

	var hFlags uint32
	if flags&SlimFlagCyclic != 0 {
		hFlags |= FlagCyclicAllowed
	}
	if flags&SlimFlagDict != 0 {
		hFlags |= FlagHasDict
	}
	h = Header{
		Flags:      hFlags,
		SchemaHash: gotSchema,
		HashSeed:   seed,
		RootTable:  rootTable,
		RootID:     1,
		TotalSize:  uint64(len(data)),
	}
	return h, dict, infos, nil
}
