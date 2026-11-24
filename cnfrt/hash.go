package cnfrt

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"math/bits"
	"sync/atomic"
)

// DefaultSeed is the deterministic hash seed selected by WithDeterministicSeed.
// Seeds are stored in .cnf headers, so persisted hash tables can be probed
// by any process on any platform.
const DefaultSeed uint64 = 0x243f6a8885a308d3

// wyhash constants.
const (
	wyp0 = 0xa0761d6478bd642f
	wyp1 = 0xe7037ed1a0b428db
	wyp2 = 0x8ebc6af09c88c6e3
	wyp3 = 0x589965cc75374cc3
)

var randSeedState atomic.Uint64

func init() {
	var b [8]byte
	_, _ = rand.Read(b[:])
	s := binary.LittleEndian.Uint64(b[:])
	if s == 0 {
		s = DefaultSeed
	}
	randSeedState.Store(s)
}

// RandomSeed returns a non-zero 64-bit random hash seed derived from a
// crypto/rand-initialized splitmix64 generator.
func RandomSeed() uint64 {
	for {
		x := randSeedState.Add(0x9e3779b97f4a7c15)
		x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
		x = (x ^ (x >> 27)) * 0x94d049bb133111eb
		x ^= x >> 31
		if x != 0 {
			return x
		}
	}
}

func wymix(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return hi ^ lo
}

func wymum(a, b uint64) (uint64, uint64) {
	hi, lo := bits.Mul64(a, b)
	return lo, hi
}

func r8s(s string) uint64 {
	_ = s[7]
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

func r4s(s string) uint64 {
	_ = s[3]
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24
}

// HashString returns the deterministic 64-bit wyhash (v4) of s under seed.
// Its output depends only on the input bytes and the seed, so it is stable
// across processes, platforms and Go releases.
func HashString(p string, seed uint64) uint64 {
	n := len(p)
	seed ^= wymix(seed^wyp0, wyp1)
	var a, b uint64
	switch {
	case n <= 16:
		if n >= 4 {
			q := (n >> 3) << 2
			a = r4s(p)<<32 | r4s(p[q:])
			b = r4s(p[n-4:])<<32 | r4s(p[n-4-q:])
		} else if n > 0 {
			a = uint64(p[0])<<16 | uint64(p[n>>1])<<8 | uint64(p[n-1])
		}
	default:
		s := p
		if len(s) > 48 {
			see1, see2 := seed, seed
			for len(s) > 48 {
				_ = s[47]
				seed = wymix(r8s(s)^wyp1, r8s(s[8:])^seed)
				see1 = wymix(r8s(s[16:])^wyp2, r8s(s[24:])^see1)
				see2 = wymix(r8s(s[32:])^wyp3, r8s(s[40:])^see2)
				s = s[48:]
			}
			seed ^= see1 ^ see2
		}
		for len(s) > 16 {
			_ = s[15]
			seed = wymix(r8s(s)^wyp1, r8s(s[8:])^seed)
			s = s[16:]
		}
		tail := p[n-16:]
		_ = tail[15]
		a = r8s(tail)
		b = r8s(tail[8:])
	}
	a ^= wyp1
	b ^= seed
	a, b = wymum(a, b)
	return wymix(a^wyp0^uint64(n), b^wyp1)
}

// Hash returns the deterministic 64-bit hash of p under seed. It is equal to
// HashString(string(p), seed).
func Hash(p []byte, seed uint64) uint64 { return HashString(UnsafeSealBytes(p), seed) }

// HashUint64 returns the hash of the 8-byte little-endian encoding of v. It
// is equal to Hash(binary.LittleEndian.AppendUint64(nil, v), seed) but does
// not touch memory.
func HashUint64(v, seed uint64) uint64 {
	h0, l0 := bits.Mul64(seed^wyp0, wyp1)
	h1, l1 := bits.Mul64((v<<32|v>>32)^wyp1, v^(seed^h0^l0))
	h2, l2 := bits.Mul64(l1^wyp0^8, h1^wyp1)
	return h2 ^ l2
}

// HashUint32 returns the hash of the 4-byte little-endian encoding of v. It
// is equal to Hash(binary.LittleEndian.AppendUint32(nil, v), seed) but does
// not touch memory.
func HashUint32(v uint32, seed uint64) uint64 {
	h0, l0 := bits.Mul64(seed^wyp0, wyp1)
	x := uint64(v)<<32 | uint64(v)
	h1, l1 := bits.Mul64(x^wyp1, x^(seed^h0^l0))
	h2, l2 := bits.Mul64(l1^wyp0^4, h1^wyp1)
	return h2 ^ l2
}

// HashFloat64 hashes a float64 key after canonicalization. It returns
// ErrNaNKey for NaN.
func HashFloat64(f float64, seed uint64) (uint64, error) {
	b, err := CanonFloat64(f)
	if err != nil {
		return 0, err
	}
	return HashUint64(b, seed), nil
}

// CanonFloat64 returns the canonical key bits of f: NaN is rejected with
// ErrNaNKey and both signed zeros map to the bits of +0.0, matching the
// equality semantics of Go maps. Values (as opposed to keys) are never
// canonicalized by cnfgo.
func CanonFloat64(f float64) (uint64, error) {
	if f != f {
		return 0, ErrNaNKey
	}
	if f == 0 {
		return 0, nil
	}
	return math.Float64bits(f), nil
}

// CanonFloat32 is the float32 form of CanonFloat64.
func CanonFloat32(f float32) (uint32, error) {
	if f != f {
		return 0, ErrNaNKey
	}
	if f == 0 {
		return 0, nil
	}
	return math.Float32bits(f), nil
}

// HashCombine mixes h2 into h1. It is used to hash composite keys field by
// field.
func HashCombine(h1, h2 uint64) uint64 { return wymix(h1^wyp2, h2^wyp3) }
