package benchmarks

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

// Store is a read-only item cache under measurement.
type Store interface {
	// Read returns the price and a byte-derived digest of the title of the
	// item with the given ID. Every implementation touches the title bytes
	// symmetrically.
	Read(id uint64) (price int64, titleMetric int, ok bool)
}

func titleDigest(t string) int {
	if len(t) == 0 {
		return 0
	}
	return len(t) + int(t[0]) + int(t[len(t)-1])
}

// NativeStore keeps ordinary Go objects.
type NativeStore struct{ C *Catalog }

// Read implements Store.
func (s NativeStore) Read(id uint64) (int64, int, bool) {
	it := s.C.Items[id]
	if it == nil {
		return 0, 0, false
	}
	return it.Price, titleDigest(it.Title), true
}

// CompactStore keeps one Compact value per item in a Go map.
type CompactStore struct{ M map[uint64]CompactItem }

// Read implements Store.
func (s CompactStore) Read(id uint64) (int64, int, bool) {
	c, ok := s.M[id]
	if !ok {
		return 0, 0, false
	}
	return c.Price(), titleDigest(c.Title()), true
}

// SlabStore keeps Compact values in a cnfrt.SlabMap and reads via Get (copying).
type SlabStore struct {
	M *cnfrt.SlabMap[uint64, CompactItem]
}

// Read implements Store.
func (s SlabStore) Read(id uint64) (int64, int, bool) {
	c, ok := s.M.Get(id)
	if !ok {
		return 0, 0, false
	}
	return c.Price(), titleDigest(c.Title()), true
}

// SlabBorrowStore keeps Compact values in a cnfrt.SlabMap and reads zero-copy
// via Borrow and BorrowCompactItemBytes.
type SlabBorrowStore struct {
	M *cnfrt.SlabMap[uint64, CompactItem]
}

// Read implements Store.
func (s SlabBorrowStore) Read(id uint64) (int64, int, bool) {
	var price int64
	var digest int
	ok := s.M.Borrow(id, func(raw []byte) {
		c, err := BorrowCompactItemBytes(raw)
		if err != nil {
			return
		}
		price = c.Price()
		digest = titleDigest(c.Title())
	})
	return price, digest, ok
}

// SnapshotStore reads through a frozen catalog.
type SnapshotStore struct{ S *CatalogSnapshot }

// Read implements Store.
func (s SnapshotStore) Read(id uint64) (int64, int, bool) {
	it, ok := s.S.Root().Items().Get(id)
	if !ok {
		return 0, 0, false
	}
	var digest int
	it.WithTitle(func(t string) { digest = titleDigest(t) })
	return it.Price(), digest, true
}

// Scenarios lists the names accepted by Run.
var Scenarios = []string{
	"native",
	"native-unique",
	"native-gogc400",
	"native-memlimit",
	"compact-map",
	"compact-map-dict",
	"slabmap",
	"slabmap-borrow",
	"slabmap-dict",
	"slabmap-dict-borrow",
	"snapshot",
	"snapshot-mmap",
}

// HighCardScenarios lists the representative scenarios measured under the
// high-cardinality (100% unique Title and SKU) workload.
var HighCardScenarios = []string{
	"native",
	"compact-map",
	"compact-map-dict",
	"slabmap-borrow",
	"snapshot",
}

// Result holds the measurements of one scenario.
type Result struct {
	Scenario string
	N        int
	// Build is the time to construct the store from the native catalog;
	// it is zero for the native scenarios, whose store is the catalog.
	Build time.Duration
	// Heap is the live heap attributable to the store, from
	// /memory/classes/heap/objects:bytes after a full collection. Stores
	// built from the catalog are measured after the catalog is released.
	Heap uint64
	// OffHeap is memory held outside the Go heap (a file mapping).
	OffHeap uint64
	// Scan is /gc/scan/heap:bytes, the heap the collector scanned in the
	// last cycle, minus the baseline before the store was built.
	Scan uint64
	// FullGC is the median duration of five forced collections.
	FullGC time.Duration
	// Read is the mean cost of one random point read.
	Read time.Duration
	// Churn measures a workload that allocates short-lived objects while
	// the store is alive.
	ChurnWall  time.Duration
	ChurnGCs   uint64
	ChurnGCCPU time.Duration
}

// ChurnBytes is the default volume of garbage allocated by the churn
// workload.
const ChurnBytes = 2 << 30

type sample struct {
	heap, scan, gcs uint64
	gcCPU           float64
}

func readSample() sample {
	s := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/scan/heap:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
	}
	metrics.Read(s)
	u := func(i int) uint64 {
		if s[i].Value.Kind() == metrics.KindUint64 {
			return s[i].Value.Uint64()
		}
		return 0
	}
	var gcCPU float64
	if s[3].Value.Kind() == metrics.KindFloat64 {
		gcCPU = s[3].Value.Float64()
	}
	return sample{u(0), u(1), u(2), gcCPU}
}

func settle() sample {
	runtime.GC()
	runtime.GC()
	return readSample()
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// Run builds the store of the named scenario from a catalog of n items and
// measures it; the churn workload allocates churnBytes. dir holds temporary
// files. Measurements are only meaningful in a process that runs nothing
// else, which is how cmd/report invokes it.
func Run(scenario string, n int, churnBytes int64, dir string) (Result, error) {
	return runCatalog(scenario, n, churnBytes, dir, false)
}

// RunHighCard is like Run, but uses MakeHighCardCatalog so that every item's
// Title and SKU are 100% distinct across all n items.
func RunHighCard(scenario string, n int, churnBytes int64, dir string) (Result, error) {
	return runCatalog(scenario, n, churnBytes, dir, true)
}

func runCatalog(scenario string, n int, churnBytes int64, dir string, highCard bool) (Result, error) {
	r := Result{Scenario: scenario, N: n}
	// Restore the collector settings changed by the tuned scenarios.
	gcPercent := debug.SetGCPercent(100)
	debug.SetGCPercent(gcPercent)
	defer debug.SetGCPercent(gcPercent)
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1)) // a negative limit only queries
	base := settle()
	var src *Catalog
	switch {
	case highCard:
		src = MakeHighCardCatalog(n)
	case scenario == "native-unique":
		src = MakeInternedCatalog(n)
	default:
		src = MakeCatalog(n)
	}
	var store Store
	var release func() error
	start := time.Now()
	switch scenario {
	case "native", "native-unique", "native-gogc400", "native-memlimit":
		store = NativeStore{src}
	case "compact-map", "compact-map-dict":
		var dict *cnfrt.Dict
		var packOpts []cnfrt.FreezeOption
		if scenario == "compact-map-dict" {
			dict = cnfrt.NewDict(cnfrt.DefaultDictMaxBytes)
			packOpts = []cnfrt.FreezeOption{cnfrt.WithDict(dict)}
			release = func() error { dict.Close(); return nil }
		}
		m := make(map[uint64]CompactItem, n)
		for id, it := range src.Items {
			c, err := PackItem(it, packOpts...)
			if err != nil {
				if dict != nil {
					dict.Close()
				}
				return r, err
			}
			m[id] = c
		}
		store = CompactStore{m}
	case "slabmap", "slabmap-borrow", "slabmap-dict", "slabmap-dict-borrow":
		var dict *cnfrt.Dict
		var packOpts []cnfrt.FreezeOption
		if scenario == "slabmap-dict" || scenario == "slabmap-dict-borrow" {
			dict = cnfrt.NewDict(cnfrt.DefaultDictMaxBytes)
			packOpts = []cnfrt.FreezeOption{cnfrt.WithDict(dict)}
			release = func() error { dict.Close(); return nil }
		}
		m, err := cnfrt.NewSlabMap[uint64, CompactItem](nil)
		if err != nil {
			if dict != nil {
				dict.Close()
			}
			return r, err
		}
		for id, it := range src.Items {
			c, err := PackItem(it, packOpts...)
			if err == nil {
				err = m.Put(id, c)
			}
			if err != nil {
				if dict != nil {
					dict.Close()
				}
				return r, err
			}
		}
		if scenario == "slabmap-borrow" || scenario == "slabmap-dict-borrow" {
			store = SlabBorrowStore{m}
		} else {
			store = SlabStore{m}
		}
	case "snapshot":
		s, err := FreezeCatalog(src)
		if err != nil {
			return r, err
		}
		store = SnapshotStore{s}
	case "snapshot-mmap":
		s, err := FreezeCatalog(src)
		if err != nil {
			return r, err
		}
		path := filepath.Join(dir, "catalog.cnf")
		if err := WriteCatalogSnapshotAtomic(path, s); err != nil {
			return r, err
		}
		m, err := MapCatalogFile(path, cnfrt.VerifyTrustedHeader)
		if err != nil {
			return r, err
		}
		if st, err := os.Stat(path); err == nil {
			r.OffHeap = uint64(st.Size())
		}
		store = SnapshotStore{m}
		release = func() error { m.Close(); return os.Remove(path) }
	default:
		return r, fmt.Errorf("unknown scenario %q", scenario)
	}
	native := scenario == "native" || scenario == "native-unique" || scenario == "native-gogc400" || scenario == "native-memlimit"
	if native {
		// The native store is the source catalog itself.
		r.Build = 0
	} else {
		r.Build = time.Since(start)
		src = nil
	}
	switch scenario {
	case "native-gogc400":
		// Unbounded GOGC=400: trades up to 5x live heap memory for fewer
		// collections.
		debug.SetGCPercent(400)
	case "native-memlimit":
		// Memory-bounded container tuning: GOGC=400 with a realistic
		// GOMEMLIMIT of 1.5x live heap plus 64 MiB headroom.
		live := settle().heap
		debug.SetGCPercent(400)
		debug.SetMemoryLimit(int64(live)*3/2 + 64<<20)
	}
	after := settle()
	r.Heap = sub(after.heap, base.heap)
	r.Scan = sub(after.scan, base.scan)

	gcs := make([]time.Duration, 5)
	for i := range gcs {
		t0 := time.Now()
		runtime.GC()
		gcs[i] = time.Since(t0)
	}
	slices.Sort(gcs)
	r.FullGC = gcs[len(gcs)/2]

	var err error
	if r.Read, err = measureReads(store, n); err != nil {
		return r, err
	}

	before := readSample()
	t0 := time.Now()
	churn(churnBytes)
	r.ChurnWall = time.Since(t0)
	end := readSample()
	r.ChurnGCs = end.gcs - before.gcs
	r.ChurnGCCPU = time.Duration((end.gcCPU - before.gcCPU) * float64(time.Second))

	runtime.KeepAlive(store)
	runtime.KeepAlive(src)
	if release != nil {
		if err := release(); err != nil {
			return r, err
		}
	}
	return r, nil
}

func measureReads(s Store, n int) (time.Duration, error) {
	const reads = 1 << 20
	rng := rand.New(rand.NewPCG(1, 2))
	ids := make([]uint64, reads)
	for i := range ids {
		ids[i] = rng.Uint64N(uint64(n)) + 1
	}
	var sum int64
	t0 := time.Now()
	for _, id := range ids {
		p, l, ok := s.Read(id)
		if !ok {
			return 0, fmt.Errorf("item %d missing", id)
		}
		sum += p + int64(l)
	}
	d := time.Since(t0) / reads
	if sum == 0 {
		return 0, errors.New("empty reads")
	}
	return d, nil
}

type churnNode struct {
	next    *churnNode
	payload [48]byte
}

var churnSink []*churnNode

// churn allocates total bytes of linked 64-byte objects, keeping a window of
// the most recent 8 MiB reachable, as a request-processing service would.
func churn(total int64) {
	const window = 8 << 20 / 64
	ring := make([]*churnNode, window)
	var prev *churnNode
	for i := range total / 64 {
		n := &churnNode{next: prev}
		if i%16 == 0 {
			n.next = nil
		}
		ring[i%window] = n
		prev = n
	}
	churnSink = ring[:1]
}

// ColdStarts lists the formats accepted by ColdStart.
var ColdStarts = []string{"json", "gob", "cnf-trusted", "cnf-checksum", "cnf-untrusted"}

// WriteColdStartFiles encodes a catalog of n items in every format into dir.
func WriteColdStartFiles(n int, dir string) error {
	c := MakeCatalog(n)
	js, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), js, 0o644); err != nil {
		return err
	}
	var gb bytes.Buffer
	if err := gob.NewEncoder(&gb).Encode(c); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.gob"), gb.Bytes(), 0o644); err != nil {
		return err
	}
	s, err := FreezeCatalog(c)
	if err != nil {
		return err
	}
	return WriteCatalogSnapshotAtomic(filepath.Join(dir, "catalog.cnf"), s)
}

// ColdStart loads the catalog in the given format from dir and reads item
// probe, returning the elapsed time and the size of the file.
func ColdStart(format, dir string, probe uint64) (time.Duration, int64, error) {
	var file string
	switch format {
	case "json":
		file = "catalog.json"
	case "gob":
		file = "catalog.gob"
	default:
		file = "catalog.cnf"
	}
	path := filepath.Join(dir, file)
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	var store Store
	switch format {
	case "json", "gob":
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, 0, err
		}
		var c Catalog
		if format == "json" {
			err = json.Unmarshal(data, &c)
		} else {
			err = gob.NewDecoder(bytes.NewReader(data)).Decode(&c)
		}
		if err != nil {
			return 0, 0, err
		}
		store = NativeStore{&c}
	case "cnf-trusted", "cnf-checksum", "cnf-untrusted":
		mode := map[string]cnfrt.VerifyMode{
			"cnf-trusted":   cnfrt.VerifyTrustedHeader,
			"cnf-checksum":  cnfrt.VerifyChecksum,
			"cnf-untrusted": cnfrt.VerifyUntrusted,
		}[format]
		s, err := MapCatalogFile(path, mode)
		if err != nil {
			return 0, 0, err
		}
		defer s.Close()
		store = SnapshotStore{s}
	default:
		return 0, 0, fmt.Errorf("unknown format %q", format)
	}
	if _, _, ok := store.Read(probe); !ok {
		return 0, 0, fmt.Errorf("item %d missing", probe)
	}
	return time.Since(start), st.Size(), nil
}

// FreezeCost measures FreezeCatalog for a catalog of n items.
type FreezeCost struct {
	N         int
	Time      time.Duration
	Allocated uint64 // bytes allocated during Freeze
	Allocs    uint64 // heap objects allocated during Freeze
	// Peak is the highest heap occupancy observed during Freeze above the
	// occupancy before it (with the source catalog alive), sampled from
	// /memory/classes/heap/objects:bytes every 20µs.
	Peak   uint64
	Tables int // size of the resulting snapshot tables and strings
}

// MeasureFreeze freezes a catalog of n items and reports its cost.
func MeasureFreeze(n int) (FreezeCost, error) {
	c := MakeCatalog(n)
	// The sampler is started before the baseline so that its own
	// allocations are not attributed to Freeze.
	stop := make(chan struct{})
	peak := make(chan uint64)
	ready, start := make(chan struct{}), make(chan struct{})
	go func() {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		tick := time.NewTicker(20 * time.Microsecond)
		defer tick.Stop()
		var hi uint64
		close(ready)
		<-start
		for {
			metrics.Read(s)
			if s[0].Value.Kind() == metrics.KindUint64 {
				hi = max(hi, s[0].Value.Uint64())
			}
			select {
			case <-stop:
				peak <- hi
				return
			case <-tick.C:
			}
		}
	}()
	<-ready
	base := settle().heap
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	close(start)
	t0 := time.Now()
	s, err := FreezeCatalog(c)
	d := time.Since(t0)
	close(stop)
	p := <-peak
	runtime.ReadMemStats(&m1)
	if err != nil {
		return FreezeCost{}, err
	}
	runtime.KeepAlive(c)
	return FreezeCost{N: n, Time: d, Allocated: m1.TotalAlloc - m0.TotalAlloc, Allocs: m1.Mallocs - m0.Mallocs, Peak: sub(p, base), Tables: s.SizeBytes()}, nil
}
