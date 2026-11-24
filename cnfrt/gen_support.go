package cnfrt

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"
)

// ErrNilRoot is returned when a nil root is passed to a generated Freeze or
// Pack function.
var ErrNilRoot = errors.New("cnfrt: nil root")

// HashSpan is the flat encoding of a hash-indexed map: the span of its
// sorted key/value entries and the span of its power-of-two index region,
// whose slots hold 1-based entry numbers (0 marks an empty slot).
type HashSpan struct {
	KV  Span32
	Idx Span32
}

// IsNil reports whether h encodes a nil map.
func (h HashSpan) IsNil() bool { return h.KV.Off == NilSpanOff }

// Optional is the view of an optional value stored in a slice or map.
type Optional[T any] struct {
	Value T
	Valid bool
}

// Get returns the value and whether it is present.
func (o Optional[T]) Get() (T, bool) { return o.Value, o.Valid }

// CodecValue is the view of a value encoded by a registered codec C. The
// raw encoding is decoded on demand.
type CodecValue[T any, C Codec[T]] struct {
	Raw string
}

// Decode decodes the value.
func (v CodecValue[T, C]) Decode() (T, error) {
	var c C
	return c.Decode(v.Raw)
}

// B2U8 converts a bool to its flat byte form.
func B2U8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// KeyBits64 returns the canonical key bits of a non-NaN float64 (both
// zeros map to 0).
func KeyBits64(f float64) uint64 {
	if f == 0 {
		return 0
	}
	return math.Float64bits(f)
}

// KeyBits32 returns the canonical key bits of a non-NaN float32.
func KeyBits32(f float32) uint64 {
	if f == 0 {
		return 0
	}
	return uint64(math.Float32bits(f))
}

// Claims records which elements of a pool are referenced by validated
// spans, so that VerifyUntrusted can reject overlapping spans and validate
// every pool element at most once (linear time on crafted inputs).
type Claims struct {
	bits []uint64
	n    int
}

// NewClaims returns claims for a pool of n elements.
func NewClaims(n int) Claims { return Claims{bits: make([]uint64, (n+63)/64), n: n} }

// Claim checks that s is a valid span of the pool and does not overlap any
// previously claimed span. exact, if non-negative, is the required length
// (for fixed-size arrays); nilOK allows the nil sentinel.
func (c *Claims) Claim(s Span32, exact int, nilOK bool) error {
	if s.Off == NilSpanOff {
		if !nilOK || s.Len != 0 || exact > 0 {
			return fmt.Errorf("%w: unexpected nil span", ErrBoundsViolation)
		}
		return nil
	}
	if exact >= 0 && uint64(s.Len) != uint64(exact) {
		return fmt.Errorf("%w: span length %d, want %d", ErrBoundsViolation, s.Len, exact)
	}
	if s.Len == 0 {
		if s.Off != 0 {
			return fmt.Errorf("%w: empty span with offset %d", ErrBoundsViolation, s.Off)
		}
		return nil
	}
	if s.End() > uint64(c.n) {
		return fmt.Errorf("%w: span [%d,+%d) outside pool of %d", ErrBoundsViolation, s.Off, s.Len, c.n)
	}
	for i := s.Off; i < s.Off+s.Len; i++ {
		w, b := i/64, uint64(1)<<(i%64)
		if c.bits[w]&b != 0 {
			return fmt.Errorf("%w: overlapping spans at element %d", ErrBoundsViolation, i)
		}
		c.bits[w] |= b
	}
	return nil
}

// Graph is the node graph of a snapshot as seen by CheckAcyclic.
type Graph interface {
	// TableLens returns the number of nodes in each table.
	TableLens() []int
	// Edges calls visit for every node referenced by node id of table tbl.
	Edges(tbl int, id uint32, visit func(tbl int, id uint32))
}

// CheckAcyclic verifies that g has no directed cycle using an iterative
// depth-first search, so crafted inputs cannot exhaust the stack. All
// references must already be known to be in bounds. A cycle is reported
// with an error that matches both ErrCycleDetected and ErrBoundsViolation.
func CheckAcyclic(g Graph) error {
	lens := g.TableLens()
	offs := make([]int, len(lens))
	total := 0
	for i, n := range lens {
		offs[i] = total
		total += n
	}
	color := make([]uint8, total)
	type frame struct {
		tbl      int
		id       uint32
		lo, next int
	}
	var stack []frame
	var kids []uint64
	pack := func(t int, id uint32) uint64 { return uint64(t)<<32 | uint64(id) }
	for t, n := range lens {
		base := offs[t]
		for id := 1; id <= n; id++ {
			if color[base+id-1] != 0 {
				continue
			}
			color[base+id-1] = 1
			lo := len(kids)
			g.Edges(t, uint32(id), func(ct int, cid uint32) {
				if cid != 0 {
					kids = append(kids, pack(ct, cid))
				}
			})
			stack = append(stack, frame{tbl: t, id: uint32(id), lo: lo, next: lo})
			for len(stack) > 0 {
				top := &stack[len(stack)-1]
				if top.next == len(kids) {
					color[offs[top.tbl]+int(top.id)-1] = 2
					kids = kids[:top.lo]
					stack = stack[:len(stack)-1]
					continue
				}
				k := kids[top.next]
				top.next++
				ct, cid := int(k>>32), uint32(k)
				cidx := offs[ct] + int(cid) - 1
				switch color[cidx] {
				case 1:
					return fmt.Errorf("%w (%w): cycle through table %d node %d", ErrCycleDetected, ErrBoundsViolation, ct, cid)
				case 2:
					continue
				}
				color[cidx] = 1
				clo := len(kids)
				g.Edges(ct, cid, func(gt int, gid uint32) {
					if gid != 0 {
						kids = append(kids, pack(gt, gid))
					}
				})
				stack = append(stack, frame{tbl: ct, id: cid, lo: clo, next: clo})
			}
		}
	}
	return nil
}

// CompareAddr returns a total ordering on netip.Addr consistent with ==.
func CompareAddr(a, b netip.Addr) int { return a.Compare(b) }

// ComparePrefix returns a total ordering on netip.Prefix consistent with ==,
// including invalid prefixes that still carry an address.
func ComparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return cmp.Compare(a.Bits(), b.Bits())
}

// CompareAddrPort returns a total ordering on netip.AddrPort consistent with ==.
func CompareAddrPort(a, b netip.AddrPort) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return cmp.Compare(a.Port(), b.Port())
}

// HashAddr returns the deterministic 64-bit hash of a under seed.
func HashAddr(a netip.Addr, seed uint64) uint64 {
	if !a.IsValid() {
		return HashUint64(0, seed)
	}
	ip := a.As16()
	var buf [17]byte
	if a.Is4() {
		buf[0] = AddrV4
		copy(buf[1:5], ip[12:16])
		return HashString(UnsafeSealBytes(buf[:5]), seed)
	}
	z := a.Zone()
	if z == "" {
		buf[0] = AddrV6
		copy(buf[1:17], ip[:])
		return HashString(UnsafeSealBytes(buf[:17]), seed)
	}
	buf[0] = AddrV6Zoned
	copy(buf[1:17], ip[:])
	return HashCombine(HashString(UnsafeSealBytes(buf[:17]), seed), HashString(z, seed))
}

// HashPrefix returns the deterministic 64-bit hash of p under seed.
func HashPrefix(p netip.Prefix, seed uint64) uint64 {
	a := p.Addr()
	if !a.IsValid() {
		return HashUint64(0, seed)
	}
	ip := a.As16()
	var buf [18]byte
	if a.Is4() {
		buf[0] = AddrV4
		buf[1] = byte(p.Bits() + 1)
		copy(buf[2:6], ip[12:16])
		return HashString(UnsafeSealBytes(buf[:6]), seed)
	}
	buf[0] = AddrV6
	buf[1] = byte(p.Bits() + 1)
	copy(buf[2:18], ip[:])
	return HashString(UnsafeSealBytes(buf[:18]), seed)
}

// HashAddrPort returns the deterministic 64-bit hash of ap under seed.
func HashAddrPort(ap netip.AddrPort, seed uint64) uint64 {
	return HashCombine(HashAddr(ap.Addr(), seed), HashUint64(uint64(ap.Port()), seed))
}

// DecodeCompactTime decodes a flat time stored in a Compact value whose
// location names live in the encoded ext table.
func DecodeCompactTime(sec int64, meta uint64, ext string) time.Time {
	ft := FlatTime{UnixSec: sec, Meta: meta}
	t := time.Unix(sec, int64(meta&nsecMask))
	switch ft.Kind() {
	case LocUTC:
		return t.UTC()
	case LocFixed:
		if ft.LocIndex() == 0 {
			return t.In(FixedLocation("", ft.FixedOffset()))
		}
	}
	loc, ok := ExtLocation(ext, ft.LocIndex())
	if !ok {
		panic("cnfrt: corrupt location index in compact value")
	}
	return t.In(loc)
}

// DecodeCompactAddr decodes a flat address stored in a Compact value whose
// zone names live in the encoded ext table.
func DecodeCompactAddr(f FlatAddr, ext string) netip.Addr {
	if f.Kind != AddrV6Zoned {
		return DecodeAddr(f, nil)
	}
	zone, ok := ExtZone(ext, f.ZonedIdx)
	if !ok {
		panic("cnfrt: corrupt zone index in compact value")
	}
	return DecodeAddrZone(f, zone)
}

// CompactFlatAddr reads a FlatAddr stored at offset off of an encoded
// Compact value without allocating.
func CompactFlatAddr(raw string, off int) FlatAddr {
	var f FlatAddr
	s := raw[off : off+19]
	_ = s[18]
	copy(f.IP[:], s[:16])
	f.ZonedIdx = uint16(s[16]) | uint16(s[17])<<8
	f.Kind = s[18]
	return f
}

// CompactFlatPrefix reads a FlatPrefix stored at offset off of an encoded
// Compact value without allocating.
func CompactFlatPrefix(raw string, off int) FlatPrefix {
	var f FlatPrefix
	s := raw[off : off+18]
	_ = s[17]
	copy(f.IP[:], s[:16])
	f.BitsPlusOne = s[16]
	f.Kind = s[17]
	return f
}

// ReuseSlice returns a zeroed slice of length n that reuses the storage of
// s when its capacity suffices.
func ReuseSlice[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n)
	}
	s = s[:n]
	clear(s)
	return s
}
