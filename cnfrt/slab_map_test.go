package cnfrt

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestSlabMapScanAndFragmentation(t *testing.T) {
	const n = 500_000
	before := scanHeapBytes()
	m, err := NewSlabMap[string, string](nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := make(map[int]int, n) // key index -> value version, -1 deleted
	val := func(i, ver int) string {
		return "value-" + strconv.Itoa(i) + "-v" + strconv.Itoa(ver) + "-padding-padding"
	}
	for i := range n {
		if err := m.Put(keyN(i), val(i, 0)); err != nil {
			t.Fatal(err)
		}
		ref[i] = 0
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 250_000 {
		i := r.IntN(n)
		if r.IntN(2) == 0 {
			ref[i]++
			m.Put(keyN(i), val(i, ref[i]))
		} else {
			deleted := ref[i] < 0
			if got := m.Delete(keyN(i)); got == deleted {
				t.Fatalf("Delete(%d) = %v", i, got)
			}
			ref[i] = -1
		}
	}
	m.Compact()
	live := 0
	for i, ver := range ref {
		got, ok := m.Get(keyN(i))
		if ver < 0 {
			if ok {
				t.Fatalf("deleted key %d resurrected", i)
			}
			continue
		}
		live++
		if !ok || got != val(i, ver) {
			t.Fatalf("Get(%d) = %q, %v; want version %d", i, got, ok, ver)
		}
	}
	if m.Len() != live {
		t.Fatalf("Len = %d, want %d", m.Len(), live)
	}
	st := m.Stats()
	t.Logf("stats after churn and compaction: %+v", st)
	if st.DeadBytes > st.LiveBytes {
		t.Fatalf("dead space %d exceeds live space %d after compaction", st.DeadBytes, st.LiveBytes)
	}
	ref = nil
	after := scanHeapBytes()
	delta := int64(after) - int64(before)
	t.Logf("scannable heap delta with %d-entry SlabMap alive: %d bytes", live, delta)
	if delta > 512<<10 {
		t.Fatalf("SlabMap adds %d bytes of scannable heap, want < 512 KiB", delta)
	}
	runtime.KeepAlive(m)
}

func TestSlabMapForcedCollisions(t *testing.T) {
	m, err := NewSlabMapSize[string, int](4<<10, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.hashMask = 0x7 // only 8 distinct hashes: long collision chains
	ref := map[string]int{}
	r := rand.New(rand.NewPCG(5, 6))
	for step := range 20_000 {
		k := keyN(r.IntN(500))
		switch r.IntN(3) {
		case 0, 1:
			m.Put(k, step)
			ref[k] = step
		case 2:
			_, want := ref[k]
			if got := m.Delete(k); got != want {
				t.Fatalf("Delete(%s) = %v, want %v", k, got, want)
			}
			delete(ref, k)
		}
		if step%997 == 0 {
			m.Compact()
		}
	}
	m.Compact()
	if m.Len() != len(ref) {
		t.Fatalf("Len = %d, want %d", m.Len(), len(ref))
	}
	for k, v := range ref {
		if got, ok := m.Get(k); !ok || got != v {
			t.Fatalf("Get(%s) = %d, %v; want %d", k, got, ok, v)
		}
	}
	seen := 0
	for k, v := range m.All() {
		if ref[k] != v {
			t.Fatalf("All yielded %s=%d", k, v)
		}
		seen++
	}
	if seen != len(ref) {
		t.Fatalf("All yielded %d entries, want %d", seen, len(ref))
	}
	heads := 0
	for i := range m.shards {
		heads += len(m.shards[i].index)
	}
	if heads > 8 {
		t.Fatalf("index has %d heads with an 8-value hash", heads)
	}
}

func TestSlabMapOversizedAndKeys(t *testing.T) {
	m, _ := NewSlabMapSize[[2]float64, []byte](4<<10, nil)
	big := make([]byte, 100<<10)
	for i := range big {
		big[i] = byte(i)
	}
	if err := m.Put([2]float64{1, math.Copysign(0, -1)}, big); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Get([2]float64{1, 0})
	if !ok || string(got) != string(big) {
		t.Fatal("oversized value / canonical key")
	}
	if err := m.Put([2]float64{math.NaN(), 0}, nil); !errors.Is(err, ErrNaNKey) {
		t.Fatal("NaN key accepted")
	}
	m.Put([2]float64{2, 2}, nil)
	if v, ok := m.Get([2]float64{2, 2}); !ok || v != nil {
		t.Fatal("nil value")
	}
	m.Put([2]float64{3, 3}, []byte{})
	if v, ok := m.Get([2]float64{3, 3}); !ok || v == nil {
		t.Fatal("empty value became nil")
	}
	before := m.Stats().Slabs
	if !m.Delete([2]float64{1, 0}) {
		t.Fatal("delete oversized")
	}
	if st := m.Stats(); st.Slabs != before-1 {
		t.Fatalf("oversized slab not released: %+v", st)
	}
	var borrowed int
	m.Borrow([2]float64{3, 3}, func(b []byte) { borrowed = len(b) })
	if borrowed != 1 {
		t.Fatal("borrow")
	}
}

func TestSlabMapConcurrent(t *testing.T) {
	m, _ := NewSlabMapSize[uint64, uint64](16<<10, nil)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 5000 {
				k := uint64(w*1_000_000 + i%300)
				m.Put(k, uint64(i))
				if i%3 == 0 {
					m.Delete(k)
				}
				m.Get(uint64(i))
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for i := range 20000 {
				m.Get(uint64(i % 5000))
				m.Has(uint64(i))
			}
		})
	}
	wg.Wait()
	m.Compact()
	for w := range 8 {
		for j := range 300 {
			last := 4800 + j
			if last >= 5000 {
				last -= 300
			}
			v, ok := m.Get(uint64(w*1_000_000 + j))
			if want := last%3 != 0; ok != want || (ok && v != uint64(last)) {
				t.Fatalf("worker %d key %d: got %d, %v; want %d, %v", w, j, v, ok, last, want)
			}
		}
	}
	if err := AssertNoPointers[slabSlot](); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSlabMapGet(b *testing.B) {
	m, _ := NewSlabMap[string, int64](nil)
	for i := range 100_000 {
		m.Put(keyN(i), int64(i))
	}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = keyN(i * 97)
	}
	i := 0
	for b.Loop() {
		m.Get(keys[i&1023])
		i++
	}
}

// TestSlabMapShardsAreIndependent checks that keys spread over the shards
// and that a shard held exclusively (as compaction holds it) does not block
// operations on keys of other shards.
func TestSlabMapShardsAreIndependent(t *testing.T) {
	m, err := NewSlabMap[uint64, uint64](nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range uint64(10_000) {
		m.Put(i, i)
	}
	used := 0
	for i := range m.shards {
		if m.shards[i].n > 0 {
			used++
		}
	}
	if used < slabShards*3/4 {
		t.Fatalf("10000 keys use only %d of %d shards", used, slabShards)
	}
	shardOf := func(k uint64) *slabShard {
		var buf keyBuf
		key, _ := m.encodeKey(&buf, &k)
		return m.shard(m.hash(key))
	}
	held := shardOf(0)
	held.mu.Lock()
	done := make(chan int)
	go func() {
		n := 0
		for k := range uint64(10_000) {
			if shardOf(k) == held {
				continue
			}
			if v, ok := m.Get(k); !ok || v != k {
				t.Errorf("Get(%d) = %d, %v", k, v, ok)
			}
			m.Put(k, k)
			n++
		}
		done <- n
	}()
	select {
	case n := <-done:
		if n == 0 {
			t.Fatal("no key outside the held shard")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("operations on other shards blocked by one held shard")
	}
	held.mu.Unlock()
}

func TestSlabMapFixedIntKeysAndCollisions(t *testing.T) {
	t.Run("uint64", func(t *testing.T) {
		m, err := NewSlabMapSize[uint64, string](minSlabSize, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.hashMask = 0 // force every key into the same hash chain
		for i := range uint64(300) {
			if err := m.Put(i, fmt.Sprintf("val-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		for i := range uint64(150) {
			if err := m.Put(i, fmt.Sprintf("upd-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		for i := uint64(200); i < 300; i++ {
			if !m.Delete(i) {
				t.Fatalf("Delete(%d) = false", i)
			}
		}
		m.Compact()
		if m.Len() != 200 {
			t.Fatalf("Len = %d, want 200", m.Len())
		}
		for i := range uint64(300) {
			got, ok := m.Get(i)
			if i >= 200 {
				if ok || m.Has(i) {
					t.Fatalf("deleted key %d still present", i)
				}
				continue
			}
			want := fmt.Sprintf("val-%d", i)
			if i < 150 {
				want = fmt.Sprintf("upd-%d", i)
			}
			if !ok || got != want {
				t.Fatalf("Get(%d) = %q, %v; want %q", i, got, ok, want)
			}
			var borrowed string
			if !m.Borrow(i, func(b []byte) { borrowed = string(b) }) || borrowed != want {
				t.Fatalf("Borrow(%d) = %q, want %q", i, borrowed, want)
			}
		}
	})

	t.Run("int32", func(t *testing.T) {
		m, err := NewSlabMapSize[int32, int64](minSlabSize, nil)
		if err != nil {
			t.Fatal(err)
		}
		m.hashMask = 0
		for i := int32(-100); i < 100; i++ {
			if err := m.Put(i, int64(i)*7); err != nil {
				t.Fatal(err)
			}
		}
		for i := int32(-100); i < 0; i++ {
			if !m.Delete(i) {
				t.Fatalf("Delete(%d) = false", i)
			}
		}
		m.Compact()
		if m.Len() != 100 {
			t.Fatalf("Len = %d, want 100", m.Len())
		}
		for i := int32(-100); i < 100; i++ {
			got, ok := m.Get(i)
			if i < 0 {
				if ok {
					t.Fatalf("deleted key %d still present", i)
				}
				continue
			}
			if !ok || got != int64(i)*7 {
				t.Fatalf("Get(%d) = %d, %v", i, got, ok)
			}
			if !m.Borrow(i, func(b []byte) {
				if len(b) != 8 {
					t.Errorf("Borrow(%d) len = %d, want 8", i, len(b))
				}
			}) {
				t.Fatalf("Borrow(%d) missed", i)
			}
		}
	})
}

type alignedStringCodec struct{}

func (alignedStringCodec) Size(s string) int                  { return len(s) }
func (alignedStringCodec) Append(dst []byte, s string) []byte { return append(dst, s...) }
func (alignedStringCodec) Decode(src string) (string, error)  { return strings.Clone(src), nil }
func (alignedStringCodec) AlignCNF() int                      { return 8 }

func TestSlabMapBorrowAlignedCodec(t *testing.T) {
	t.Run("stringKeys", func(t *testing.T) {
		m, err := NewSlabMapSize[string, string](minSlabSize, &MapOptions[string]{Value: alignedStringCodec{}})
		if err != nil {
			t.Fatal(err)
		}
		// Force all keys into one shard so records of varying key and value
		// lengths are packed back-to-back in the same slabs.
		m.hashMask = 0x3f
		keys := make([]string, 0, 256)
		vals := make([]string, 0, 256)
		for i := range 256 {
			kl := 1 + i%8 // key lengths 1..8
			var k string
			switch kl {
			case 1:
				if i >= 26 {
					continue
				}
				k = string(byte('a' + i))
			case 2:
				k = fmt.Sprintf("%02x", i)
			default:
				k = fmt.Sprintf("%02x%s", i, strings.Repeat("k", kl-2))
			}
			v := fmt.Sprintf("val-%d-%s", i, strings.Repeat("v", i%13))
			if err := m.Put(k, v); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
			vals = append(vals, v)
		}
		for i, k := range keys {
			want := vals[i]
			ok := m.Borrow(k, func(b []byte) {
				if string(b) != want {
					t.Errorf("initial: Borrow(%q) = %q, want %q", k, string(b), want)
				}
				if p := uintptr(unsafe.Pointer(&b[0])); p%8 != 0 {
					t.Errorf("initial: Borrow(%q) (keyLen=%d) ptr %#x is not 8-byte aligned", k, len(k), p)
				}
			})
			if !ok {
				t.Fatalf("initial: Borrow(%q) missed", k)
			}
		}
		// Delete half and trigger compaction to verify compacted records stay aligned.
		for i := 0; i < len(keys); i += 2 {
			if !m.Delete(keys[i]) {
				t.Fatalf("Delete(%q) = false", keys[i])
			}
		}
		m.Compact()
		for i := 1; i < len(keys); i += 2 {
			k, want := keys[i], vals[i]
			ok := m.Borrow(k, func(b []byte) {
				if string(b) != want {
					t.Errorf("after Compact: Borrow(%q) = %q, want %q", k, string(b), want)
				}
				if p := uintptr(unsafe.Pointer(&b[0])); p%8 != 0 {
					t.Errorf("after Compact: Borrow(%q) (keyLen=%d) ptr %#x is not 8-byte aligned", k, len(k), p)
				}
			})
			if !ok {
				t.Fatalf("after Compact: Borrow(%q) missed", k)
			}
		}
	})

	t.Run("uint32Keys", func(t *testing.T) {
		m, err := NewSlabMapSize[uint32, string](minSlabSize, &MapOptions[string]{Value: alignedStringCodec{}})
		if err != nil {
			t.Fatal(err)
		}
		for i := uint32(0); i < 64; i++ {
			v := fmt.Sprintf("u32-%d-%s", i, strings.Repeat("y", int(i%7)))
			if err := m.Put(i, v); err != nil {
				t.Fatal(err)
			}
		}
		for i := uint32(0); i < 64; i++ {
			ok := m.Borrow(i, func(b []byte) {
				if p := uintptr(unsafe.Pointer(&b[0])); p%8 != 0 {
					t.Errorf("uint32 key %d: Borrow ptr %#x is not 8-byte aligned", i, p)
				}
			})
			if !ok {
				t.Fatalf("Borrow(%d) missed", i)
			}
		}
	})
}

func BenchmarkSlabMapParallel(b *testing.B) {
	m, _ := NewSlabMap[uint64, int64](nil)
	const n = 100_000
	for i := range uint64(n) {
		m.Put(i, int64(i))
	}
	var seq sync.Mutex
	seed := uint64(0)
	b.RunParallel(func(pb *testing.PB) {
		seq.Lock()
		seed++
		r := rand.New(rand.NewPCG(seed, 1))
		seq.Unlock()
		for pb.Next() {
			k := r.Uint64N(n)
			if r.IntN(20) == 0 {
				m.Put(k, int64(k))
			} else {
				m.Get(k)
			}
		}
	})
}
