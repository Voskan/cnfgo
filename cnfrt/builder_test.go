package cnfrt

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"testing"
	"unsafe"
)

type tnode struct {
	name string
	kids []*tnode
}

// freezeNames is a miniature hand-written analogue of generated Freeze
// code: pass 1 measures, pass 2 emits rows in reverse post-order.
func freezeNames(mode FreezeMode, root *tnode, opts ...FreezeOption) (rows [][]uint32, names []Span32, blob string, err error) {
	defer func() { Recover(recover(), &err) }()
	b := NewBuilder(mode, 1, opts...)
	var measure func(n *tnode)
	measure = func(n *tnode) {
		if n == nil || !b.MeasureEnter(Addr(n), 0) {
			return
		}
		b.MeasureString(n.name)
		for _, k := range n.kids {
			measure(k)
		}
		b.MeasureLeave(Addr(n), 0)
	}
	measure(root)
	b.BeginEmit()
	rows = make([][]uint32, b.Count(0))
	names = make([]Span32, b.Count(0))
	var emit func(n *tnode) uint32
	emit = func(n *tnode) uint32 {
		if n == nil {
			return 0
		}
		if mode != ModeTree {
			if id := b.Lookup(Addr(n), 0); id != 0 {
				return id
			}
		}
		var id uint32
		if b.Preorder() {
			id = b.PreID(0)
			b.Record(Addr(n), 0, id)
		}
		sp := b.String(n.name)
		kids := make([]uint32, len(n.kids))
		for i, k := range n.kids {
			kids[i] = emit(k)
		}
		if !b.Preorder() {
			id = b.PostID(0)
			if mode == ModeDAG {
				b.Record(Addr(n), 0, id)
			}
		}
		rows[id-1] = kids
		names[id-1] = sp
		return id
	}
	if emit(root) != 1 {
		panic("root must receive ID 1")
	}
	return rows, names, b.StrBlob(), nil
}

func TestBuilderDAGTopologicalOrder(t *testing.T) {
	shared := &tnode{name: "shared-child-node"}
	root := &tnode{name: "root", kids: []*tnode{
		{name: "a", kids: []*tnode{shared}},
		{name: "b", kids: []*tnode{shared, {name: "shared-child-node"}}},
		shared,
	}}
	rows, names, blob, err := freezeNames(ModeDAG, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("DAG mode produced %d rows, want 5 (shared node deduplicated)", len(rows))
	}
	for self, kids := range rows {
		for _, k := range kids {
			if k <= uint32(self+1) {
				t.Fatalf("child %d of %d violates childID > selfID", k, self+1)
			}
		}
	}
	if len(blob) != len("shared-child-node")+len("root")+2 {
		t.Fatalf("strings were not deduplicated: %q", blob)
	}
	for _, sp := range names {
		if sp.End() > uint64(len(blob)) {
			t.Fatal("span out of range")
		}
	}
}

func TestBuilderTreeDuplicatesShared(t *testing.T) {
	shared := &tnode{name: "x"}
	root := &tnode{kids: []*tnode{shared, shared}}
	rows, _, _, err := freezeNames(ModeTree, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("tree mode produced %d rows, want 3", len(rows))
	}
}

func TestBuilderCycles(t *testing.T) {
	a := &tnode{name: "a"}
	b := &tnode{name: "b", kids: []*tnode{a}}
	a.kids = []*tnode{b}
	if _, _, _, err := freezeNames(ModeDAG, a); !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("DAG mode: %v", err)
	}
	if _, _, _, err := freezeNames(ModeTree, a); !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("tree mode: %v", err)
	}
	rows, _, _, err := freezeNames(ModeCyclic, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][0] != 2 || rows[1][0] != 1 {
		t.Fatalf("cyclic rows %v", rows)
	}
}

func TestBuilderDepthLimit(t *testing.T) {
	root := &tnode{}
	n := root
	for range 100 {
		k := &tnode{}
		n.kids = []*tnode{k}
		n = k
	}
	if _, _, _, err := freezeNames(ModeDAG, root, WithMaxDepth(50)); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("got %v", err)
	}
	if _, _, _, err := freezeNames(ModeTree, root, WithMaxDepth(50)); !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("got %v", err)
	}
	if _, _, _, err := freezeNames(ModeDAG, root); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderStringModes(t *testing.T) {
	var kids []*tnode
	for i := range 2000 {
		kids = append(kids, &tnode{name: []string{"alpha", "beta", "gamma"}[i%3]})
	}
	root := &tnode{kids: kids}
	for _, tc := range []struct {
		opt  FreezeOption
		want int
	}{
		{func(*FreezeOptions) {}, len("alphabetagamma")},
		{WithExactStringDedup(), len("alphabetagamma")},
		{WithoutStringDedup(), 2000 / 3 * 14},
	} {
		_, names, blob, err := freezeNames(ModeDAG, root, tc.opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) < tc.want || len(blob) > tc.want+14 {
			t.Fatalf("blob length %d, want about %d", len(blob), tc.want)
		}
		for i, sp := range names[1:] {
			_ = i
			if s := blob[sp.Off:sp.End()]; s != "alpha" && s != "beta" && s != "gamma" {
				t.Fatalf("bad string %q", s)
			}
		}
	}
}

func TestBuilderBytesNil(t *testing.T) {
	b := NewBuilder(ModeTree, 0)
	b.MeasureBytes([]byte("abc"))
	b.BeginEmit()
	if !b.Bytes(nil).IsNil() || b.Bytes([]byte{}).IsNil() {
		t.Fatal("nil vs empty bytes")
	}
	if sp := b.Bytes([]byte("abc")); sp.Len != 3 {
		t.Fatal("bytes span")
	}
}

func TestGrowPoolAndHashRegion(t *testing.T) {
	err := func() (err error) {
		defer func() { Recover(recover(), &err) }()
		c := uint32(MaxElems - 1)
		GrowPool(&c, 1)
		GrowPool(&c, 1)
		return nil
	}()
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatal("overflow not detected")
	}
	for n, want := range map[int]int{0: 0, 1: 2, 2: 4, 3: 8, 4: 8, 5: 16} {
		if got := HashRegion(n); got != want {
			t.Errorf("HashRegion(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestVisitedTableTypeDisambiguation(t *testing.T) {
	var v visitedTable
	v.insert(0x1000, 0, 7)
	v.insert(0x1000, 1, 9)
	for i := range 5000 {
		v.insert(uintptr(0x2000+16*i), 0, uint32(i+1))
	}
	if e := v.find(0x1000, 0); e == nil || e.val != 7 {
		t.Fatal("table 0 entry")
	}
	if e := v.find(0x1000, 1); e == nil || e.val != 9 {
		t.Fatal("table 1 entry")
	}
	for i := range 5000 {
		if e := v.find(uintptr(0x2000+16*i), 0); e == nil || e.val != uint32(i+1) {
			t.Fatalf("entry %d lost after growth", i)
		}
	}
}

// TestStrBlobSlack checks that the first pass predicts deduplication by the
// interning cache, so that the string blob is allocated at its final size
// and sealed in place, and that a blob with more than 1/8 of slack (as
// left by exact deduplication, whose first pass counts every string) is
// copied.
func TestStrBlobSlack(t *testing.T) {
	blob := func(strs []string, opts ...FreezeOption) (sealedInPlace bool, s string) {
		b := NewBuilder(ModeDAG, 1, opts...)
		for _, s := range strs {
			b.MeasureString(s)
		}
		b.BeginEmit()
		for _, s := range strs {
			b.String(s)
		}
		buf := unsafe.SliceData(b.strBuf)
		s = b.StrBlob()
		return unsafe.StringData(s) == buf, s
	}
	unique := make([]string, 1000)
	for i := range unique {
		unique[i] = fmt.Sprintf("unique string %06d", i)
	}
	if inPlace, s := blob(unique); !inPlace || len(s) != 1000*20 {
		t.Fatalf("unique strings: sealed in place %v, %d bytes", inPlace, len(s))
	}
	var dup []string // 100 distinct strings, each repeated 10 times
	for range 10 {
		dup = append(dup, unique[:100]...)
	}
	// The cache is lossy: strings evicted while it is still small are stored
	// once more, a bounded number independent of the input size.
	if inPlace, s := blob(dup); !inPlace || len(s) < 100*20 || len(s) > (100+16)*20 {
		t.Fatalf("deduplicated strings: sealed in place %v, %d bytes", inPlace, len(s))
	}
	if inPlace, s := blob(dup, WithExactStringDedup()); inPlace || len(s) != 100*20 {
		t.Fatalf("exactly deduplicated strings: sealed in place %v, %d bytes", inPlace, len(s))
	}
}

func TestStrBlobReleasesTraversalState(t *testing.T) {
	b := NewBuilder(ModeDAG, 1)
	for i := range 1000 {
		b.MeasureEnter(uintptr(i+1)*16, 0)
		b.MeasureLeave(uintptr(i+1)*16, 0)
		b.MeasureString(fmt.Sprintf("%08d", i))
	}
	b.BeginEmit()
	for i := range 1000 {
		b.String(fmt.Sprintf("%08d", i))
	}
	if len(b.visited.slots) < 1000 || b.intern.slots == nil {
		t.Fatal("traversal state missing before StrBlob")
	}
	b.StrBlob()
	if b.visited.slots != nil || b.intern.slots != nil || b.strBuf != nil {
		t.Fatal("StrBlob must release the visited table and the interning cache")
	}
}

// TestInternCachePredictsSecondPass drives the first-pass and second-pass
// caches with the same long sequence of repeated, evicted and colliding
// strings, through several in-place growths, and checks that the measured
// size equals the size of the emitted blob.
func TestInternCachePredictsSecondPass(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	strs := make([]string, 200_000)
	for i := range strs {
		switch r.IntN(4) {
		case 0:
			strs[i] = "hot-" + strconv.Itoa(r.IntN(50))
		case 1:
			strs[i] = "warm-" + strconv.Itoa(r.IntN(20_000))
		default:
			strs[i] = "cold-" + strconv.Itoa(i)
		}
	}
	b := NewBuilder(ModeDAG, 1)
	for _, s := range strs {
		b.MeasureString(s)
	}
	predicted := b.strUpper
	b.BeginEmit()
	for _, s := range strs {
		sp := b.String(s)
		if got := string(b.strBuf[sp.Off:sp.End()]); got != s {
			t.Fatalf("span of %q holds %q", s, got)
		}
	}
	if uint64(len(b.strBuf)) != predicted || cap(b.strBuf) != int(predicted) {
		t.Fatalf("first pass predicted %d bytes; second pass wrote %d into a buffer of %d", predicted, len(b.strBuf), cap(b.strBuf))
	}
	if len(b.intern.slots) < 1<<16 {
		t.Fatalf("cache did not grow: %d slots", len(b.intern.slots))
	}
	t.Logf("%d strings, %d bytes before deduplication, %d after, %d cache slots", len(strs), func() (n int) {
		for _, s := range strs {
			n += len(s)
		}
		return
	}(), predicted, len(b.intern.slots))
}
