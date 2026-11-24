package cnfrt

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestHashDeterministic(t *testing.T) {
	// Golden values pin the hash function: persisted FrozenMap tables depend
	// on it never changing.
	golden := map[string]uint64{}
	inputs := []string{"", "a", "ab", "abc", "abcd", "hello world!", "0123456789abcdef",
		"0123456789abcdefg", "the quick brown fox jumps over the lazy dog, twice over: the quick brown fox"}
	for _, in := range inputs {
		golden[in] = HashString(in, DefaultSeed)
	}
	for _, in := range inputs {
		if got := Hash([]byte(in), DefaultSeed); got != golden[in] {
			t.Errorf("Hash(%q) bytes/string mismatch", in)
		}
		if HashString(in, DefaultSeed+1) == golden[in] && in != "" {
			t.Errorf("seed has no effect for %q", in)
		}
	}
	if got, want := HashString("hello world!", 0), uint64(0xe1be5c789eb0f055); got != want {
		t.Fatalf("HashString changed: %#x, want %#x", got, want)
	}
	if got, want := HashUint64(42, DefaultSeed), uint64(0xc8544b0d9707af10); got != want {
		t.Fatalf("HashUint64 changed: %#x, want %#x", got, want)
	}
	seen := map[uint64]string{}
	for i := 0; i < 1<<16; i++ {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(i))
		h := Hash(b[:], 7)
		if h != HashUint64(uint64(i), 7) {
			t.Fatalf("HashUint64(%d) differs from Hash of LE bytes", i)
		}
		if prev, dup := seen[h]; dup {
			t.Fatalf("collision between %d and %s", i, prev)
		}
		seen[h] = "x"
	}
}

func TestHashAllLengths(t *testing.T) {
	buf := make([]byte, 300)
	for i := range buf {
		buf[i] = byte(i * 31)
	}
	seen := map[uint64]int{}
	for n := 0; n <= len(buf); n++ {
		h := Hash(buf[:n], 1)
		if HashString(string(buf[:n]), 1) != h {
			t.Fatalf("len %d mismatch", n)
		}
		if prev, ok := seen[h]; ok {
			t.Fatalf("collision between lengths %d and %d", prev, n)
		}
		seen[h] = n
	}
}

func TestCanonFloat(t *testing.T) {
	if b, err := CanonFloat64(math.Copysign(0, -1)); err != nil || b != 0 {
		t.Fatalf("-0.0: %x %v", b, err)
	}
	if _, err := CanonFloat64(math.NaN()); !errors.Is(err, ErrNaNKey) {
		t.Fatal("NaN accepted")
	}
	if b, _ := CanonFloat64(1.5); b != math.Float64bits(1.5) {
		t.Fatal("1.5")
	}
	if b, err := CanonFloat32(float32(math.Copysign(0, -1))); err != nil || b != 0 {
		t.Fatal("-0.0f")
	}
	if _, err := CanonFloat32(float32(math.NaN())); err == nil {
		t.Fatal("NaN32 accepted")
	}
	h1, _ := HashFloat64(0, 3)
	h2, _ := HashFloat64(math.Copysign(0, -1), 3)
	if h1 != h2 {
		t.Fatal("signed zeros hash differently")
	}
	if _, err := HashFloat64(math.NaN(), 3); err == nil {
		t.Fatal("NaN hashed")
	}
}

func BenchmarkHashString16(b *testing.B) {
	s := "0123456789abcdef"
	for b.Loop() {
		HashString(s, DefaultSeed)
	}
}

func BenchmarkHashString64(b *testing.B) {
	s := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for b.Loop() {
		HashString(s, DefaultSeed)
	}
}
