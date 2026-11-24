package unsafemmap_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/Voskan/cnfgo/cnfrt"
	"github.com/Voskan/cnfgo/internal/testschema"
	"github.com/Voskan/cnfgo/unsafemmap"
)

func writeFile(t testing.TB, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.cnf")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenClose(t *testing.T) {
	want := bytes.Repeat([]byte("cnfgo"), 1000)
	m, err := unsafemmap.Open(writeFile(t, want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Bytes(), want) || m.Len() != len(want) {
		t.Fatal("mapped contents differ")
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "js" && !m.IsMapped() {
		t.Fatal("expected a memory mapping")
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = m.Close() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if !m.Closed() || m.Bytes() != nil {
		t.Fatal("closed mapping still exposes its data")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := unsafemmap.Open(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := unsafemmap.Open(writeFile(t, nil)); !errors.Is(err, unsafemmap.ErrEmptyFile) {
		t.Errorf("empty file: %v", err)
	}
}

func TestCast(t *testing.T) {
	type rec struct {
		A uint64
		B uint32
		C uint16
		D int8
		E uint8
	}
	src := []rec{{1, 2, 3, -4, 5}, {^uint64(0), 7, 8, 9, 10}}
	raw := cnfrt.AppendSliceLE(nil, src)
	for _, shift := range []int{0, 1, 3} {
		buf := cnfrt.AlignedCopy(append(make([]byte, shift), raw...))
		in := cnfrt.SectionInfo{Off: uint64(shift), Len: uint64(len(raw)), Count: 2}
		got, err := unsafemmap.Cast[rec](buf, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != src[0] || got[1] != src[1] {
			t.Fatalf("shift %d: %+v", shift, got)
		}
		aliased := unsafe.Pointer(unsafe.SliceData(got)) == unsafe.Pointer(&buf[shift])
		if want := shift == 0 && cnfrt.NativeLittleEndian; aliased != want {
			t.Errorf("shift %d: aliased = %v, want %v", shift, aliased, want)
		}
	}
	for _, in := range []cnfrt.SectionInfo{
		{Off: 0, Len: uint64(len(raw)), Count: 3},
		{Off: 0, Len: uint64(len(raw)) - 1, Count: 2},
		{Off: 8, Len: uint64(len(raw)), Count: 2},
		{Off: ^uint64(0), Len: 16, Count: 1},
	} {
		if _, err := unsafemmap.Cast[rec](raw, in); !errors.Is(err, cnfrt.ErrBoundsViolation) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if s := unsafemmap.String([]byte("hello"), cnfrt.SectionInfo{Off: 1, Len: 3}); s != "ell" {
		t.Fatalf("String = %q", s)
	}
}

func TestFaultSafe(t *testing.T) {
	page := os.Getpagesize()
	path := writeFile(t, bytes.Repeat([]byte{1}, 4*page))
	m, err := unsafemmap.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsMapped() {
		m.Close()
		t.Skip("no memory mapping on this platform")
	}
	data := m.Bytes()
	if runtime.GOOS == "linux" {
		// Reading pages beyond the end of a truncated file raises SIGBUS.
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
		err := unsafemmap.FaultSafe(func() error {
			_ = data[3*page]
			return nil
		})
		if !errors.Is(err, cnfrt.ErrMmapFault) {
			t.Fatalf("read past a truncated file: %v", err)
		}
	}
	// Reading an unmapped region raises SIGSEGV.
	m.Close()
	var sink byte
	err = unsafemmap.FaultSafe(func() error {
		sink = data[page]
		return nil
	})
	if !errors.Is(err, cnfrt.ErrMmapFault) {
		t.Fatalf("read of unmapped memory: %v (byte %d)", err, sink)
	}
	boom := errors.New("boom")
	if err := unsafemmap.FaultSafe(func() error { return boom }); err != boom {
		t.Fatal("errors must pass through")
	}
	defer func() {
		if r := recover(); r != "not a fault" {
			t.Fatalf("recovered %v", r)
		}
	}()
	unsafemmap.FaultSafe(func() error { panic("not a fault") })
}

func TestAttachCleanup(t *testing.T) {
	m, err := unsafemmap.Open(writeFile(t, []byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	owner := new([64]byte)
	unsafemmap.AttachCleanup(owner, m)
	owner = nil
	for i := 0; i < 100 && !m.Closed(); i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if !m.Closed() {
		t.Fatal("cleanup did not close the mapping")
	}
}

func TestStringsSurviveClose(t *testing.T) {
	cfg := &testschema.Config{Name: "survivor", Tags: []string{"tag-one"}}
	s, err := testschema.FreezeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.cnf")
	if err := testschema.WriteConfigSnapshotAtomic(path, s); err != nil {
		t.Fatal(err)
	}
	m, err := testschema.MapConfigFile(path, cnfrt.VerifyChecksum)
	if err != nil {
		t.Fatal(err)
	}
	name, tag := m.Root().Name(), m.Root().Tags().At(0)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if name != "survivor" || tag != "tag-one" {
		t.Fatalf("strings changed after Close: %q %q", name, tag)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("reading a closed snapshot must panic, not read unmapped memory")
			}
		}()
		_ = m.Root().Name()
	}()
}

func TestFrozenMapImage(t *testing.T) {
	src := map[string]int64{}
	for i := range 5000 {
		src["key-"+string(rune('a'+i%26))+string(rune(i))] = int64(i) * 3
	}
	fm, err := cnfrt.NewFrozenMap(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, err := fm.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, img)
	for _, mode := range []cnfrt.VerifyMode{cnfrt.VerifyTrustedHeader, cnfrt.VerifyChecksum, cnfrt.VerifyUntrusted} {
		got, m, err := unsafemmap.OpenFrozenMap[string, int64](path, mode, &cnfrt.MapOptions[int64]{CloneStrings: true})
		if err != nil {
			t.Fatalf("%v: %v", mode, err)
		}
		if got.Len() != len(src) {
			t.Fatalf("Len = %d", got.Len())
		}
		for k, v := range src {
			if g, ok := got.Get(k); !ok || g != v {
				t.Fatalf("%v: Get(%q) = %d, %v", mode, k, g, ok)
			}
		}
		if m != nil {
			m.Close()
		}
	}
	if _, err := unsafemmap.FrozenMapFromImage[string, string](img, cnfrt.VerifyTrustedHeader, nil); !errors.Is(err, cnfrt.ErrSchemaMismatch) {
		t.Fatalf("wrong value type accepted: %v", err)
	}
}

// resign recomputes every section checksum, the directory checksum and the
// header checksum of img so that mutations reach the semantic validators.
func resign(img []byte) {
	if len(img) < cnfrt.HeaderSize {
		return
	}
	n := uint64(binary.LittleEndian.Uint32(img[12:]))
	if n > uint64(len(img)-cnfrt.HeaderSize)/cnfrt.DirEntrySize {
		return
	}
	dir := img[cnfrt.HeaderSize : cnfrt.HeaderSize+n*cnfrt.DirEntrySize]
	for i := range n {
		e := dir[i*cnfrt.DirEntrySize:]
		off, ln := binary.LittleEndian.Uint64(e[8:]), binary.LittleEndian.Uint64(e[16:])
		if off <= uint64(len(img)) && ln <= uint64(len(img))-off {
			binary.LittleEndian.PutUint32(e[28:], cnfrt.CRC32C(img[off:off+ln]))
		}
	}
	binary.LittleEndian.PutUint32(img[56:], cnfrt.CRC32C(dir))
	binary.LittleEndian.PutUint32(img[60:], cnfrt.CRC32C(img[:60]))
}

func seedImages(f *testing.F) {
	ptr := func(v int64) *int64 { return &v }
	alice := &testschema.User{ID: 1, Name: "alice"}
	bob := &testschema.User{ID: 2, Name: "bob", Manager: alice, Friends: []*testschema.User{alice}}
	cfg := &testschema.Config{
		Name: "seed", Tags: []string{"x", "y"}, Owner: bob, Users: []*testschema.User{alice, bob},
		Labels: map[string]string{"a": "b"}, Scores: map[string]float64{"s": 1}, Opt: ptr(0),
		ByID: map[int64]*testschema.User{1: alice}, Shapes: []testschema.Shape{testschema.Circle{R: 1}, &testschema.Rect{}},
		Grid: [][]int16{{1}, nil}, Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("X", 60)),
	}
	s, err := testschema.FreezeConfig(cfg)
	if err != nil {
		f.Fatal(err)
	}
	img, _ := s.MarshalCNF()
	f.Add(img, uint16(0), byte(0), false)
	f.Add(img, uint16(len(img)/2), byte(0xff), true)
	f.Add(img, uint16(20), byte(1), true)

	root := &testschema.Tree{Value: 1}
	root.Children = []*testschema.Tree{{Value: 2, Parent: root}}
	ts, err := testschema.FreezeCyclicTree(root)
	if err != nil {
		f.Fatal(err)
	}
	timg, _ := ts.MarshalCNF()
	f.Add(timg, uint16(70), byte(1), true)

	fm, err := cnfrt.NewFrozenMap(map[string]int64{"a": 1, "b": 2, "c": 3}, nil)
	if err != nil {
		f.Fatal(err)
	}
	fimg, _ := fm.MarshalCNF()
	f.Add(fimg, uint16(200), byte(4), true)
	f.Add(fimg, uint16(8), byte(0xff), true)
}

// FuzzVerifyUntrusted feeds mutated snapshot and FrozenMap images to the
// untrusted loaders. Mutations flip one byte (optionally re-signing the
// image so that checksums no longer reject it); accepted images are then
// traversed completely. No input may cause a panic, a fault or unbounded
// recursion.
func FuzzVerifyUntrusted(f *testing.F) {
	seedImages(f)
	f.Fuzz(func(t *testing.T, img []byte, pos uint16, xor byte, sign bool) {
		img = bytes.Clone(img)
		if len(img) > 0 {
			img[int(pos)%len(img)] ^= xor
		}
		if sign {
			resign(img)
		}
		if s, err := testschema.UnmarshalConfigSnapshot(img, cnfrt.VerifyUntrusted); err == nil {
			traverse(s)
		}
		if m, err := unsafemmap.FrozenMapFromImage[string, int64](img, cnfrt.VerifyUntrusted, nil); err == nil {
			for k, v := range m.All() {
				if g, ok := m.Get(k); !ok || g != v {
					t.Fatalf("validated map lookup of %q: %d, %v; iteration %d", k, g, ok, v)
				}
			}
			m.Get("missing")
		}
		// Header-only modes must reject or accept without panicking too.
		if s, err := testschema.UnmarshalConfigSnapshot(img, cnfrt.VerifyChecksum); err == nil {
			_ = s.Root().IsValid()
		}
	})
}

func traverse(s *testschema.ConfigSnapshot) {
	if v := s.RootConfig(); v.IsValid() {
		v.Unfreeze()
		for u := range v.Users().All() {
			if !u.IsValid() {
				continue
			}
			_ = u.Name()
			for f := range u.Friends().All() {
				if mgr := f.Manager(); f.IsValid() && mgr.IsValid() {
					_ = mgr.Name()
				}
			}
		}
		for k, val := range v.Labels().All() {
			v.Labels().Get(k)
			_ = val
		}
		for k := range v.Scores().Keys() {
			v.Scores().Get(k)
		}
		for k := range v.ByID().Keys() {
			v.ByID().Get(k)
		}
		for sh := range v.Shapes().All() {
			sh.AsCircle()
			sh.AsRect()
		}
		_ = v.Created()
		_ = v.Endpoint()
	}
	if v := s.RootTree(); v.IsValid() {
		v.Unfreeze()
	}
	if v := s.RootEndpoint(); v.IsValid() {
		v.Unfreeze()
	}
}

func benchmarkMap(b *testing.B, mode cnfrt.VerifyMode) {
	cfg := &testschema.Config{}
	for i := range 100_000 {
		cfg.Users = append(cfg.Users, &testschema.User{ID: int64(i), Name: "user-name"})
		cfg.Tags = append(cfg.Tags, "tag")
	}
	s, err := testschema.FreezeConfig(cfg)
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(b.TempDir(), "bench.cnf")
	if err := testschema.WriteConfigSnapshotAtomic(path, s); err != nil {
		b.Fatal(err)
	}
	st, _ := os.Stat(path)
	for b.Loop() {
		m, err := testschema.MapConfigFile(path, mode)
		if err != nil {
			b.Fatal(err)
		}
		if m.Root().Users().Len() != 100_000 {
			b.Fatal("bad snapshot")
		}
		m.Close()
	}
	b.ReportMetric(float64(st.Size())/(1<<20), "file-MiB")
}

func BenchmarkMapTrustedHeader(b *testing.B) { benchmarkMap(b, cnfrt.VerifyTrustedHeader) }
func BenchmarkMapChecksum(b *testing.B)      { benchmarkMap(b, cnfrt.VerifyChecksum) }
func BenchmarkMapUntrusted(b *testing.B)     { benchmarkMap(b, cnfrt.VerifyUntrusted) }

func makeEndpoints(t testing.TB, n int) map[string]testschema.CompactEndpoint {
	t.Helper()
	out := make(map[string]testschema.CompactEndpoint, n)
	for i := range n {
		name := "endpoint-" + string(rune('a'+i%26)) + "-" + string(rune('A'+i%26))
		e := &testschema.Endpoint{
			Name: name, Port: uint16(8000 + i), Up: i%2 == 0, Tags: []string{"tag", name},
			Blob: []byte(name), Fee: testschema.NewMoney(int64(i), "EUR"),
		}
		c, err := testschema.PackEndpoint(e)
		if err != nil {
			t.Fatal(err)
		}
		out["k"+name+string(rune(i))] = c
	}
	return out
}

// useEndpoint reads every field of c, directly and through its view.
func useEndpoint(c testschema.CompactEndpoint) error {
	_, _, _, _ = c.Name(), c.Port(), c.Up(), c.Blob()
	_, _, _ = c.Addr(), c.AP(), c.Seen()
	if _, err := c.Fee(); err != nil {
		return err
	}
	_, err := c.View().Unfreeze()
	return err
}

func TestOpenFrozenMapValuesOutliveClose(t *testing.T) {
	strs := map[string]string{}
	for i := range 2000 {
		strs["key-"+string(rune(i+'0'))] = "value-" + string(rune(i+'0'))
	}
	fs, err := cnfrt.NewFrozenMap(strs, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := fs.MarshalCNF()
	sm, smap, err := unsafemmap.OpenFrozenMap[string, string](writeFile(t, img), cnfrt.VerifyChecksum, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotStrs := map[string]string{}
	for k, v := range sm.All() {
		gotStrs[k] = v
	}
	eps := makeEndpoints(t, 300)
	fe, err := cnfrt.NewFrozenMap(eps, nil)
	if err != nil {
		t.Fatal(err)
	}
	eimg, _ := fe.MarshalCNF()
	em, emap, err := unsafemmap.OpenFrozenMap[string, testschema.CompactEndpoint](writeFile(t, eimg), cnfrt.VerifyChecksum, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotEps := map[string]testschema.CompactEndpoint{}
	for k := range eps {
		v, ok := em.Get(k)
		if !ok {
			t.Fatalf("Get(%q) failed", k)
		}
		gotEps[k] = v
	}
	if err := smap.Close(); err != nil {
		t.Fatal(err)
	}
	if err := emap.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	err = unsafemmap.FaultSafe(func() error {
		for k, v := range gotStrs {
			if strs[k] != v {
				t.Errorf("string %q changed after Close: %q", k, v)
			}
		}
		for k, v := range gotEps {
			if !v.Equal(eps[k]) {
				t.Errorf("compact value %q changed after Close", k)
			}
			if err := useEndpoint(v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("values read before Close alias the released mapping: %v", err)
	}
}

func TestOpenFrozenMapReleasedWhenUnreachable(t *testing.T) {
	fm, err := cnfrt.NewFrozenMap(map[int64]int64{1: 2, 3: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := fm.MarshalCNF()
	path := writeFile(t, img)
	open := func() *unsafemmap.Mapping {
		m, mp, err := unsafemmap.OpenFrozenMap[int64, int64](path, cnfrt.VerifyTrustedHeader, nil)
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := m.Get(3); !ok || v != 4 {
			t.Fatal("Get(3)")
		}
		return mp
	}
	mp := open()
	for i := 0; i < 100 && !mp.Closed(); i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if !mp.Closed() {
		t.Fatal("mapping of an unreachable FrozenMap was not released")
	}
}

func TestFrozenMapFromImageUntrustedCopiesInput(t *testing.T) {
	fm, err := cnfrt.NewFrozenMap(map[string]int64{"a": 1, "b": 2, "c": 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := fm.MarshalCNF()
	m, err := unsafemmap.FrozenMapFromImage[string, int64](img, cnfrt.VerifyUntrusted, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range img {
		img[i] = 0xff
	}
	for k, want := range map[string]int64{"a": 1, "b": 2, "c": 3} {
		if v, ok := m.Get(k); !ok || v != want {
			t.Fatalf("Get(%q) = %d, %v after the input was overwritten", k, v, ok)
		}
	}
}

// TestFrozenMapUntrustedCompactValues corrupts every byte of the inline
// prefix of a stored Compact value. VerifyUntrusted must either reject the
// image or return a map whose values are safe to use in full.
func TestFrozenMapUntrustedCompactValues(t *testing.T) {
	eps := makeEndpoints(t, 1)
	fm, err := cnfrt.NewFrozenMap(eps, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := fm.MarshalCNF()
	var raw string
	for _, c := range eps {
		raw = c.Raw()
	}
	at := bytes.Index(img, []byte(raw))
	if at < 0 {
		t.Fatal("value not found in image")
	}
	rejected := 0
	for pos := 1; pos < 64; pos++ {
		bad := bytes.Clone(img)
		bad[at+pos] ^= 0x5a
		resign(bad)
		m, err := unsafemmap.FrozenMapFromImage[string, testschema.CompactEndpoint](bad, cnfrt.VerifyUntrusted, nil)
		if err != nil {
			rejected++
			continue
		}
		for _, v := range m.All() {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("byte %d: validated value panics: %v", pos, r)
					}
				}()
				useEndpoint(v)
			}()
		}
	}
	if rejected == 0 {
		t.Fatal("no corruption of the compact prefix was rejected")
	}
}

var staticImage [1 << 16]byte

// TestUnmarshalBorrowedMemoryWithLeakDetector loads snapshots zero-copy over
// memory the Go heap does not own, a file mapping and a static array, while
// the leak detector is enabled. The detector must not attach heap-only
// sensors to such memory.
func TestUnmarshalBorrowedMemoryWithLeakDetector(t *testing.T) {
	cfg := &testschema.Config{Name: "borrowed-memory-snapshot", Tags: []string{"a-long-enough-tag-value"}}
	s, err := testschema.FreezeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := s.MarshalCNF()
	if len(img) > len(staticImage) {
		t.Fatalf("image of %d bytes does not fit", len(img))
	}
	m, err := unsafemmap.Open(writeFile(t, img))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	n := copy(staticImage[:], img)
	cnfrt.EnableLeakDetector(time.Millisecond, func(r cnfrt.LeakReport) {
		t.Errorf("unexpected report %+v", r)
	})
	defer cnfrt.DisableLeakDetector()
	for name, data := range map[string][]byte{"mapping": m.Bytes(), "static": staticImage[:n]} {
		got, err := testschema.UnmarshalConfigSnapshot(data, cnfrt.VerifyChecksum)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.RootConfig().Name() != cfg.Name {
			t.Fatalf("%s: wrong root", name)
		}
	}
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
}

func TestFrozenMapWithFaultSafe(t *testing.T) {
	fm, err := cnfrt.NewFrozenMap(map[string]string{"k": string(bytes.Repeat([]byte{'v'}, 3*os.Getpagesize()))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := fm.MarshalCNF()
	m, mapping, err := unsafemmap.OpenFrozenMap[string, string](writeFile(t, img), cnfrt.VerifyChecksum, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !mapping.IsMapped() {
		mapping.Close()
		t.Skip("no memory mapping on this platform")
	}
	err = m.WithFaultSafe(func(m *cnfrt.FrozenMap[string, string]) error {
		if v, ok := m.Get("k"); !ok || len(v) != 3*os.Getpagesize() {
			t.Errorf("Get = %d bytes, %v", len(v), ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Probing a map whose mapping was released faults; WithFaultSafe turns
	// the fault into ErrMmapFault.
	if err := mapping.Close(); err != nil {
		t.Fatal(err)
	}
	err = m.WithFaultSafe(func(m *cnfrt.FrozenMap[string, string]) error {
		m.Get("k")
		return nil
	})
	if !errors.Is(err, cnfrt.ErrMmapFault) {
		t.Fatalf("WithFaultSafe after Close = %v, want ErrMmapFault", err)
	}
}
