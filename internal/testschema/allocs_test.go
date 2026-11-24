package testschema

import (
	"runtime"
	"runtime/metrics"
	"testing"
	"time"
)

var (
	sinkInt    int64
	sinkString string
	sinkBool   bool
	sinkTime   time.Time
)

func assertNoAllocs(t *testing.T, what string, f func()) {
	t.Helper()
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	if n := testing.AllocsPerRun(100, f); n != 0 {
		t.Errorf("%s: %v allocs/op, want 0", what, n)
	}
}

func TestViewAllocs(t *testing.T) {
	s := mustFreeze(t, fixture())
	v := s.Root()
	assertNoAllocs(t, "scalar and string getters", func() {
		sinkInt = v.Version() + int64(v.Port())
		sinkString = v.Name()
		sinkBool = v.NameEquals("edge-proxy")
		sinkTime = v.Created()
		_ = v.Addr()
		_ = v.Endpoint()
		_, _ = v.Opt()
	})
	assertNoAllocs(t, "slice iteration", func() {
		for s := range v.Tags().All() {
			sinkInt += int64(len(s))
		}
		for u := range v.Users().All() {
			sinkInt += u.ID()
		}
		for i := range v.Grid().Len() {
			for x := range v.Grid().At(i).All() {
				sinkInt += int64(x)
			}
		}
	})
	assertNoAllocs(t, "map lookups and iteration", func() {
		_, sinkBool = v.Labels().Get("tier")
		_, sinkBool = v.Scores().Get("y")
		u, _ := v.ByID().Get(3)
		sinkString = u.Manager().Name()
		for k, val := range v.Labels().All() {
			sinkInt += int64(len(k) + len(val))
		}
	})
	assertNoAllocs(t, "interfaces and graph navigation", func() {
		r, _ := v.Shape().AsRect()
		sinkInt += int64(r.W())
		sinkString = v.Owner().Manager().Manager().Name()
	})
}

func TestCompactAllocs(t *testing.T) {
	c, err := PackEndpoint(&fixture().Ends[0])
	if err != nil {
		t.Fatal(err)
	}
	assertNoAllocs(t, "compact getters", func() {
		sinkString = c.Name()
		sinkInt = int64(c.Port())
		sinkTime = c.Seen()
		_ = c.Addr()
		_ = c.AP()
		_ = c.Prefix()
		_, sinkBool = c.Opt()
		_ = c.Arr()
	})
}

func scanHeapBytes() uint64 {
	s := []metrics.Sample{{Name: "/gc/scan/heap:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// TestSnapshotIsNoscan freezes a graph of 200,000 nodes and checks that
// keeping the snapshot alive adds almost nothing to the scannable heap.
func TestSnapshotIsNoscan(t *testing.T) {
	if testing.Short() {
		t.Skip("large allocation")
	}
	const n = 200_000
	cfg := &Config{}
	for i := range n {
		cfg.Users = append(cfg.Users, &User{ID: int64(i), Name: "user", Friends: []*User{}})
		cfg.Tags = append(cfg.Tags, "tag")
	}
	snap := mustFreeze(t, cfg)
	cfg = nil
	runtime.GC()
	scan := scanHeapBytes()
	if scan == 0 {
		t.Skip("/gc/scan/heap:bytes not available")
	}
	t.Logf("scannable heap with a %d-node snapshot alive: %d bytes (tables: %d KiB)", n, scan, snap.SizeBytes()/1024)
	if scan > uint64(snap.SizeBytes())/8 {
		t.Errorf("scannable heap is %d bytes; snapshot tables must not be scanned", scan)
	}
	runtime.KeepAlive(snap)
}
