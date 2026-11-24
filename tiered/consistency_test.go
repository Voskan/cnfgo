package tiered

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/Voskan/cnfgo/cnfrt"
)

//go:noinline
func checkptrProbe(b []byte) uint64 {
	return *(*uint64)(unsafe.Pointer(unsafe.SliceData(b)))
}

var instrumented = testing.AllocsPerRun(10, func() {
	var b [8]byte
	checkptrProbe(b[:])
}) != 0

type badBinaryVal struct {
	fail bool
	s    string
}

func (b badBinaryVal) MarshalBinary() ([]byte, error) {
	if b.fail {
		return nil, errors.New("encode error")
	}
	return []byte(b.s), nil
}

func (b *badBinaryVal) UnmarshalBinary(p []byte) error {
	b.s = string(p)
	return nil
}

func TestSnapshotPreservesMaxFlushingAndNoExtraCompactions(t *testing.T) {
	m := mustNew[int, int](t, &Options[int]{
		MinFlushEntries: 10_000,
		MaxFlushing:     2,
	})
	for i := range 500 {
		m.Put(i, i*2)
		if got := m.Len(); got != i+1 {
			t.Fatalf("Len() = %d, want %d", got, i+1)
		}
		v := m.Snapshot()
		if val, ok := v.Get(i); !ok || val != i*2 {
			t.Fatalf("Snapshot().Get(%d) = %d, %v", i, val, ok)
		}
		st := m.Stats()
		if st.Flushing > 2 {
			t.Fatalf("Flushing = %d exceeded MaxFlushing=2", st.Flushing)
		}
		if st.Rotations != 0 || st.Compactions != 0 {
			t.Fatalf("Snapshot/Len triggered rotation or compaction: %+v", st)
		}
	}
	count := 0
	m.Range(func(k, v int) bool {
		if v != k*2 {
			t.Fatalf("Range(%d) = %d", k, v)
		}
		count++
		return true
	})
	if count != 500 {
		t.Fatalf("Range visited %d, want 500", count)
	}
	if st := m.Stats(); st.Rotations != 0 || st.Compactions != 0 {
		t.Fatalf("Range triggered rotation or compaction: %+v", st)
	}
}

func TestApproxLenLastErrorAndFallibleTryPut(t *testing.T) {
	m := mustNew[string, int](t, &Options[int]{MinFlushEntries: 100})
	if got := m.ApproxLen(); got != 0 {
		t.Fatalf("empty ApproxLen = %d", got)
	}
	for i := range 40 {
		m.Put(string(rune('a'+i)), i)
	}
	if got := m.ApproxLen(); got != 40 {
		t.Fatalf("ApproxLen = %d, want 40", got)
	}
	m.Delete("a")
	if got := m.ApproxLen(); got != 39 {
		t.Fatalf("ApproxLen after delete = %d, want 39", got)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := m.ApproxLen(); got != 39 {
		t.Fatalf("ApproxLen after Flush = %d, want 39", got)
	}
	if m.LastError() != nil {
		t.Fatalf("LastError = %v, want nil", m.LastError())
	}

	// FallibleCodec errors are rejected synchronously in TryPut.
	bm := mustNew[string, badBinaryVal](t, nil)
	if err := bm.TryPut("bad", badBinaryVal{fail: true}); err == nil {
		t.Fatal("TryPut accepted unencodable value")
	}
	if err := bm.TryPut("ok", badBinaryVal{s: "good"}); err != nil {
		t.Fatalf("TryPut valid value: %v", err)
	}
	if err := bm.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, ok := bm.Get("ok"); !ok || got.s != "good" {
		t.Fatalf("Get(ok) = %+v, %v", got, ok)
	}
}

func TestCleanGetAndApproxLenZeroAllocs(t *testing.T) {
	src := make(map[uint64]uint64, 1024)
	for i := range uint64(1024) {
		src[i] = i * 3
	}
	base, err := cnfrt.NewFrozenMap(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewFrom(base, nil)
	defer m.Close()

	if instrumented {
		return
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		v, ok := m.Get(42)
		if !ok || v != 126 {
			t.Fatal("mismatch")
		}
	}); allocs != 0 {
		t.Fatalf("TieredMap[uint64,uint64].Get allocated %v times, want 0", allocs)
	}
	if instrumented {
		return
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if m.ApproxLen() != 1024 {
			t.Fatal("ApproxLen mismatch")
		}
	}); allocs != 0 {
		t.Fatalf("TieredMap.ApproxLen allocated %v times, want 0", allocs)
	}
}
