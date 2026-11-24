package testschema

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

func TestMutualRecursionGroupPolicy(t *testing.T) {
	// Acyclic DAG across mutually recursive Group <-> Policy types.
	leaf := &Group{Name: "leaf"}
	p := &Policy{ID: "p1", Groups: []*Group{leaf}}
	rootG := &Group{Name: "root", Policies: []*Policy{p}}
	cfg := &Config{Groups: []*Group{rootG}}

	s, err := FreezeConfig(cfg)
	if err != nil {
		t.Fatalf("FreezeConfig acyclic Group->Policy->Group: %v", err)
	}
	if s.Root().Groups().At(0).Policies().At(0).Groups().At(0).Name() != "leaf" {
		t.Fatal("Group/Policy view traversal mismatch")
	}
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatalf("VerifyUntrusted valid cross-table DAG: %v", err)
	}
	if _, err := s2.UnfreezeConfig(); err != nil {
		t.Fatal(err)
	}

	// Introduce an artificial cross-table cycle (Policy -> root Group) in a
	// DAG-flagged image and verify VerifyUntrusted rejects it via CheckAcyclic.
	cyc := *s
	cyc.pool15 = slices.Clone(s.pool15)
	// Point Policy's Groups span at rootG (node ID 1) instead of leaf (node ID 2).
	replaced := false
	for i := range cyc.pool15 {
		if cyc.pool15[i] == 2 {
			cyc.pool15[i] = 1
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("expected leaf group ID 2 in pool15")
	}
	badData, err := cyc.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalConfigSnapshot(badData, cnfrt.VerifyUntrusted); !errors.Is(err, cnfrt.ErrBoundsViolation) {
		t.Fatalf("artificial cross-table cycle accepted: %v", err)
	}

	// Cyclic Group <-> Policy must be rejected in DAG mode and accepted in Cyclic mode.
	g1 := &Group{Name: "g1"}
	p1 := &Policy{ID: "p1", Groups: []*Group{g1}}
	g1.Policies = []*Policy{p1}
	if _, err := FreezeConfig(&Config{Groups: []*Group{g1}}); !errors.Is(err, cnfrt.ErrCycleDetected) {
		t.Fatalf("cyclic Group/Policy in DAG mode: %v", err)
	}
	cs, err := FreezeCyclicConfig(&Config{Groups: []*Group{g1}})
	if err != nil {
		t.Fatalf("FreezeCyclicConfig: %v", err)
	}
	cdata, _ := cs.MarshalCNF()
	cs2, err := UnmarshalConfigSnapshot(cdata, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatalf("VerifyUntrusted cyclic Group/Policy: %v", err)
	}
	out, err := cs2.UnfreezeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if out.Groups[0].Policies[0].Groups[0] != out.Groups[0] {
		t.Fatal("cyclic Group/Policy identity not preserved")
	}
}

func TestNewMapKeysAndMultiRoot(t *testing.T) {
	hk1 := HostKey{Addr: netip.MustParseAddr("10.0.0.1"), Port: 80, Name: "web"}
	hk2 := HostKey{Addr: netip.MustParseAddr("fe80::1%eth0"), Port: 443, Name: "edge"}
	cfg := &Config{
		Ints: []int64{10, 20, 30},
		ByAddr: map[netip.Addr]string{
			netip.MustParseAddr("10.0.0.1"):     "v4",
			netip.MustParseAddr("fe80::1%eth0"): "zoned",
		},
		ByPrefix: map[netip.Prefix]int32{
			netip.MustParsePrefix("10.0.0.0/8"):    8,
			netip.MustParsePrefix("2001:db8::/32"): 32,
		},
		ByHost: map[HostKey]int64{
			hk1: 100,
			hk2: 200,
		},
	}
	s, err := FreezeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatalf("VerifyUntrusted: %v", err)
	}
	v := s2.Root()
	if got, ok := v.ByAddr().Get(netip.MustParseAddr("fe80::1%eth0")); !ok || got != "zoned" {
		t.Fatalf("ByAddr zoned: %q %v", got, ok)
	}
	if got, ok := v.ByPrefix().Get(netip.MustParsePrefix("2001:db8::/32")); !ok || got != 32 {
		t.Fatalf("ByPrefix: %d %v", got, ok)
	}
	if got, ok := v.ByHost().Get(hk2); !ok || got != 200 {
		t.Fatalf("ByHost hashed struct key: %d %v", got, ok)
	}
	copied := v.Ints().CopyTo(nil)
	if !slices.Equal(copied, []int64{10, 20, 30}) {
		t.Fatalf("CopyTo = %v", copied)
	}

	// Second root (Tree) gets MapTreeFile, WriteTreeSnapshotAtomic, WithRootTreeFaultSafe.
	ts, err := FreezeTreeTree(&Tree{Value: 77, Children: []*Tree{{Value: 88}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tree.cnf")
	if err := WriteTreeSnapshotAtomic(path, ts); err != nil {
		t.Fatal(err)
	}
	mapped, err := MapTreeFile(path, cnfrt.VerifyChecksum)
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.Close()
	if err := mapped.WithRootTreeFaultSafe(func(tv TreeView) error {
		if tv.Value() != 77 || tv.Children().At(0).Value() != 88 {
			t.Fatal("tree root view mismatch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMmapKeepAliveDuringWithAndClosedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.cnf")
	cfg := &Config{
		Name:    "persistent-mmap-payload",
		Created: time.Date(2026, 6, 1, 12, 0, 0, 0, time.FixedZone("MMAP_TZ", 3600)),
	}
	s, err := FreezeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigSnapshotAtomic(path, s); err != nil {
		t.Fatal(err)
	}

	// Open an mmap snapshot, drop all outer references before entering WithName,
	// and force GC inside the callback. KeepAlive must prevent premature Munmap.
	runWithGC := func() string {
		ms, err := MapConfigFile(path, cnfrt.VerifyTrustedHeader)
		if err != nil {
			t.Fatal(err)
		}
		v := ms.Root()
		ms = nil
		var got string
		v.WithName(func(name string) {
			runtime.GC()
			runtime.GC()
			got = string([]byte(name))
		})
		return got
	}
	if got := runWithGC(); got != "persistent-mmap-payload" {
		t.Fatalf("WithName under GC = %q", got)
	}

	// Closed mmap snapshot must return cnfrt.ErrClosed from WriteTo and MarshalCNF.
	ms, err := MapConfigFile(path, cnfrt.VerifyChecksum)
	if err != nil {
		t.Fatal(err)
	}
	_ = ms.Root().Created()
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.MarshalCNF(); !errors.Is(err, cnfrt.ErrClosed) {
		t.Fatalf("MarshalCNF after Close = %v, want ErrClosed", err)
	}
	var buf bytes.Buffer
	if _, err := ms.WriteTo(&buf); !errors.Is(err, cnfrt.ErrClosed) {
		t.Fatalf("WriteTo after Close = %v, want ErrClosed", err)
	}
	// Location cache entry created from the mmap snapshot remains valid after Close.
	if loc := cnfrt.FixedLocation("MMAP_TZ", 3600); loc.String() != "MMAP_TZ" {
		t.Fatalf("location cache after mmap Close = %q", loc.String())
	}
}

func TestInlinedValueStructMeta(t *testing.T) {
	// Meta is used exclusively by value, so it must be inlined into flatConfig,
	// flatEndpoint, and the []flatMeta slice pool rather than getting a node table.
	snapType := reflect.TypeFor[ConfigSnapshot]()
	if _, hasTable := snapType.FieldByName("nodesMeta"); hasTable {
		t.Fatal("expected Meta to be inlined without a nodesMeta table")
	}

	cfg := &Config{
		Name:   "inline-meta",
		Nested: Meta{Key: "env", Value: "prod", Count: 7},
		Metas: []Meta{
			{Key: "k1", Value: "v1", Count: 1},
			{Key: "k2", Value: "v2", Count: 2},
		},
	}
	s, err := FreezeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nv := s.Root().Nested()
	if !nv.IsValid() || nv.Snapshot() != s || nv.Key() != "env" || nv.Value() != "prod" || nv.Count() != 7 {
		t.Fatalf("unexpected Nested MetaView: valid=%v key=%q val=%q count=%d", nv.IsValid(), nv.Key(), nv.Value(), nv.Count())
	}
	unfrozenMeta, err := nv.Unfreeze()
	if err != nil || *unfrozenMeta != cfg.Nested {
		t.Fatalf("MetaView.Unfreeze = %+v, %v; want %+v", unfrozenMeta, err, cfg.Nested)
	}
	if s.Root().Metas().Len() != 2 || s.Root().Metas().At(1).Value() != "v2" {
		t.Fatal("Metas slice view mismatch")
	}

	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s2.UnfreezeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if out.Nested != cfg.Nested || !slices.Equal(out.Metas, cfg.Metas) {
		t.Fatalf("round-trip mismatch: got %+v / %+v", out.Nested, out.Metas)
	}
}

func TestCompactDictDetachAndPersistence(t *testing.T) {
	d := cnfrt.NewDict(1 << 20)
	defer d.Close()

	makeEP := func(uniqueName string) *Endpoint {
		return &Endpoint{
			Name:    uniqueName,
			Port:    8443,
			Weight:  1.5,
			Up:      true,
			Addr:    netip.MustParseAddr("fe80::1%eth0"),
			Prefix:  netip.MustParsePrefix("10.0.0.0/8"),
			AP:      netip.MustParseAddrPort("[fe80::2%lo0]:443"),
			Seen:    time.Date(2026, 7, 1, 9, 30, 0, 0, time.FixedZone("CEST", 7200)),
			Opt:     ptr(int32(99)),
			OptName: ptr("shared-optional-endpoint-label"),
			Arr:     [2]uint16{10, 20},
			Blob:    []byte("shared-binary-blob-stays-local"),
			Tags:    []string{"prometheus.io/scrape", "kubernetes.io/service", "environment=production"},
			Meta:    Meta{Key: "shared-meta-key", Value: "shared-meta-value", Count: 42},
			Fee:     NewMoney(500, "USD"),
		}
	}

	// First Pack observes strings for the first time (seen-twice filter keeps them local).
	c1, err := PackEndpoint(makeEP("ep-unique-0001"), cnfrt.WithDict(d))
	if err != nil {
		t.Fatal(err)
	}
	// Second Pack admits repeated strings into Dict and shrinks the compact encoding.
	c2, err := PackEndpoint(makeEP("ep-unique-0002"), cnfrt.WithDict(d))
	if err != nil {
		t.Fatal(err)
	}
	if !c2.UsesDict() {
		t.Fatal("expected second PackEndpoint with shared Dict to set UsesDict()")
	}
	if len(c2.Raw()) >= len(c1.Raw()) {
		t.Fatalf("expected Dict-backed compact value to be smaller: len(c2)=%d, len(c1)=%d", len(c2.Raw()), len(c1.Raw()))
	}

	// Direct getters, View, ParseCompact, BorrowCompact, and Unpack must all match.
	if c2.Name() != "ep-unique-0002" || !c2.NameEquals("ep-unique-0002") || c2.NameLen() != len("ep-unique-0002") {
		t.Fatalf("direct Name getter mismatch: %q", c2.Name())
	}
	if optName, ok := c2.OptName(); !ok || optName != "shared-optional-endpoint-label" {
		t.Fatalf("direct OptName getter mismatch: %q, %v", optName, ok)
	}
	if c2.Blob() != "shared-binary-blob-stays-local" {
		t.Fatalf("direct Blob getter mismatch: %q", c2.Blob())
	}
	if fee, err := c2.Fee(); err != nil || fee != NewMoney(500, "USD") {
		t.Fatalf("direct Fee getter mismatch: %+v, %v", fee, err)
	}
	if c2.Seen().Location().String() != "CEST" || c2.Addr().Zone() != "eth0" {
		t.Fatalf("direct ext getters mismatch: seen=%v addr=%v", c2.Seen(), c2.Addr())
	}
	v2 := c2.View()
	if v2.Meta().Key() != "shared-meta-key" || v2.Meta().Value() != "shared-meta-value" {
		t.Fatalf("View Meta mismatch: %q=%q", v2.Meta().Key(), v2.Meta().Value())
	}
	if v2.Tags().Len() != 3 || v2.Tags().At(0) != "prometheus.io/scrape" {
		t.Fatal("View Tags mismatch")
	}

	// Untrusted bytes may reference a Dict only when it is explicitly allowed.
	if _, err := ParseCompactEndpoint(c2.Raw(), cnfrt.VerifyUntrusted); !errors.Is(err, cnfrt.ErrDictRequired) {
		t.Fatalf("ParseCompactEndpoint(VerifyUntrusted) without AllowDict = %v, want ErrDictRequired", err)
	}
	other := cnfrt.NewDict(0)
	if _, err := ParseCompactEndpoint(c2.Raw(), cnfrt.VerifyUntrusted, cnfrt.AllowDict(other)); !errors.Is(err, cnfrt.ErrDictRequired) {
		t.Fatalf("ParseCompactEndpoint(VerifyUntrusted) with a foreign Dict = %v, want ErrDictRequired", err)
	}
	other.Close()
	parsed, err := ParseCompactEndpoint(c2.Raw(), cnfrt.VerifyUntrusted, cnfrt.AllowDict(d))
	if err != nil {
		t.Fatalf("ParseCompactEndpoint(VerifyUntrusted, AllowDict) on Dict-backed compact: %v", err)
	}
	if _, err := ParseCompactEndpoint(c2.Raw(), cnfrt.VerifyChecksum); err != nil {
		t.Fatalf("ParseCompactEndpoint(VerifyChecksum) on Dict-backed compact: %v", err)
	}

	// A FrozenMap holding Dict-backed values cannot be persisted.
	fm, err := cnfrt.NewFrozenMap(map[string]CompactEndpoint{"ep2": c2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fm.WriteTo(io.Discard); !errors.Is(err, cnfrt.ErrDictRequired) {
		t.Fatalf("FrozenMap.WriteTo with Dict-backed values = %v, want ErrDictRequired", err)
	}
	if _, err := fm.MarshalCNF(); !errors.Is(err, cnfrt.ErrDictRequired) {
		t.Fatalf("FrozenMap.MarshalCNF with Dict-backed values = %v, want ErrDictRequired", err)
	}
	if !parsed.Equal(c2) {
		t.Fatal("ParseCompactEndpoint returned unequal value")
	}
	unpacked, err := c2.Unpack()
	if err != nil {
		t.Fatal(err)
	}
	if unpacked.Name != "ep-unique-0002" || *unpacked.OptName != "shared-optional-endpoint-label" || unpacked.Meta.Value != "shared-meta-value" {
		t.Fatalf("Unpack mismatch: %+v", unpacked)
	}

	// Standalone .cnf serialization of a Dict-backed snapshot must return ErrDictRequired.
	snapDict, err := c2.Snapshot(cnfrt.VerifyChecksum)
	if err != nil {
		t.Fatal(err)
	}
	if !snapDict.UsesDict() {
		t.Fatal("expected Snapshot.UsesDict() == true")
	}
	if _, err := snapDict.MarshalCNF(); !errors.Is(err, cnfrt.ErrDictRequired) {
		t.Fatalf("MarshalCNF on dict snapshot = %v, want ErrDictRequired", err)
	}
	var wbuf bytes.Buffer
	if _, err := snapDict.WriteTo(&wbuf); !errors.Is(err, cnfrt.ErrDictRequired) {
		t.Fatalf("WriteTo on dict snapshot = %v, want ErrDictRequired", err)
	}

	// Store in SlabMap and read via Borrow.
	sm, err := cnfrt.NewSlabMap[string, CompactEndpoint](nil)
	if err != nil {
		t.Fatal(err)
	}
	sm.Put("ep2", c2)
	if !sm.Borrow("ep2", func(val []byte) {
		b, err := BorrowCompactEndpointBytes(val)
		if err != nil {
			t.Errorf("BorrowCompactEndpoint: %v", err)
			return
		}
		if optName, ok := b.OptName(); !ok || optName != "shared-optional-endpoint-label" {
			t.Errorf("borrowed OptName = %q, %v", optName, ok)
		}
	}) {
		t.Fatal("SlabMap.Borrow missed ep2")
	}

	// Detach materializes all dict strings locally so the detached value survives d.Close().
	detached, err := c2.Detach()
	if err != nil {
		t.Fatal(err)
	}
	if detached.UsesDict() {
		t.Fatal("expected detached CompactEndpoint to have UsesDict() == false")
	}
	again, err := c2.Detach()
	if err != nil || again.Raw() != detached.Raw() {
		t.Fatalf("repeated Detach is not byte-identical (err %v)", err)
	}
	d.Close()

	if _, err := BorrowCompactEndpoint(c2.Raw()); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("BorrowCompactEndpoint after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := ParseCompactEndpoint(c2.Raw(), cnfrt.VerifyUntrusted, cnfrt.AllowDict(d)); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("ParseCompactEndpoint after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := ParseCompactEndpoint(c2.Raw(), cnfrt.VerifyChecksum); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("trusted ParseCompactEndpoint after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := c2.TryView(); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("TryView after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := c2.Snapshot(cnfrt.VerifyTrustedHeader); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("Snapshot after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := c2.Unpack(); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("Unpack after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := c2.Detach(); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("Detach after Dict.Close = %v, want ErrUnknownDict", err)
	}
	if _, err := PackEndpoint(makeEP("after-close"), cnfrt.WithDict(d)); !errors.Is(err, cnfrt.ErrUnknownDict) {
		t.Fatalf("PackEndpoint with a closed Dict = %v, want ErrUnknownDict", err)
	}
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s after Dict.Close did not panic", name)
			}
		}()
		fn()
	}
	mustPanic("View", func() { c2.View() })
	mustPanic("OptName", func() { _, _ = c2.OptName() })
	// A view built before Close holds the dictionary chunks it captured and
	// keeps reading them; only new decodes are refused.
	if key := v2.Meta().Key(); key != "shared-meta-key" {
		t.Fatalf("view built before Dict.Close reads Key %q", key)
	}

	// Detached copy continues to validate, read, and serialize to .cnf cleanly.
	if _, err := ParseCompactEndpoint(detached.Raw(), cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("ParseCompactEndpoint(detached) after Dict.Close: %v", err)
	}
	if optName, ok := detached.OptName(); !ok || optName != "shared-optional-endpoint-label" {
		t.Fatalf("detached OptName = %q, %v", optName, ok)
	}
	detSnap, err := detached.Snapshot(cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := detSnap.MarshalCNF(); err != nil {
		t.Fatalf("MarshalCNF on detached snapshot: %v", err)
	}
}

func TestCompactDetachOptionsPreserveDefaultsAndClearDict(t *testing.T) {
	d := cnfrt.NewDict(64 << 10)
	defer d.Close()

	makeEP := func(name string) *Endpoint {
		return &Endpoint{
			Name:    name,
			OptName: ptr("shared-detach-opt-label"),
			Seen:    time.Date(2026, 7, 1, 9, 30, 0, 0, time.Local),
			Tags:    []string{"shared-detach-tag-1", "shared-detach-tag-2"},
			Meta:    Meta{Key: "shared-key", Value: "shared-val"},
		}
	}
	if _, err := PackEndpoint(makeEP("warm"), cnfrt.WithDict(d)); err != nil {
		t.Fatal(err)
	}
	const seed = uint64(0x1122334455667788)
	c, err := PackEndpoint(makeEP("target"), cnfrt.WithDict(d), cnfrt.WithHashSeed(seed), cnfrt.WithPreserveLocal())
	if err != nil {
		t.Fatal(err)
	}
	if !c.UsesDict() {
		t.Fatal("expected packed value to use Dict")
	}

	// Passing WithDict to Detach must still produce a self-contained value.
	detWithDict, err := c.Detach(cnfrt.WithDict(d))
	if err != nil {
		t.Fatalf("Detach(WithDict): %v", err)
	}
	if detWithDict.UsesDict() {
		t.Fatal("Detach(WithDict) returned a value with UsesDict() == true")
	}

	// Passing unrelated options must keep the original seed and PreserveLocal.
	det1, err := c.Detach(cnfrt.WithExactStringDedup())
	if err != nil {
		t.Fatalf("Detach(WithExactStringDedup): %v", err)
	}
	det2, err := c.Detach(cnfrt.WithExactStringDedup())
	if err != nil {
		t.Fatal(err)
	}
	if det1.Raw() != det2.Raw() {
		t.Fatal("Detach with options lost determinism")
	}
	if det1.Seen().Location() != time.Local {
		t.Fatalf("Detach with options lost time.Local: got %v", det1.Seen().Location())
	}
}

func TestUntrustedAllowDictConcurrentChunkGrowth(t *testing.T) {
	d := cnfrt.NewDict(4 << 20)
	defer d.Close()

	// Seed a few strings so initial CompactEndpoint uses the dictionary.
	base := &Endpoint{
		Name: "ep-initial",
		Tags: []string{"shared-concurrent-tag-a", "shared-concurrent-tag-b"},
		Meta: Meta{Key: "shared-concurrent-key", Value: "shared-concurrent-val"},
	}
	if _, err := PackEndpoint(base, cnfrt.WithDict(d)); err != nil {
		t.Fatal(err)
	}
	valid, err := PackEndpoint(base, cnfrt.WithDict(d))
	if err != nil {
		t.Fatal(err)
	}
	if !valid.UsesDict() {
		t.Fatal("expected valid CompactEndpoint to use Dict")
	}

	var stop atomic.Bool
	var writerWG, readerWG sync.WaitGroup
	// Concurrent writer continuously growing the dictionary across multiple chunks.
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		pad := strings.Repeat("z", 128)
		for i := 0; !stop.Load() && i < 20000; i++ {
			s := pad + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
			d.Intern(s)
			d.Intern(s)
		}
	}()

	// Concurrent readers packing newly-chunked Compact values and verifying them
	// with VerifyUntrusted + AllowDict must never panic even while chunks grow.
	for r := range 4 {
		readerWG.Add(1)
		go func(id int) {
			defer readerWG.Done()
			for i := range 200 {
				ep := &Endpoint{
					Name: "ep-reader",
					Tags: []string{"shared-concurrent-tag-a", strings.Repeat("z", 128) + string(rune('a'+(i+id)%26)) + "aa"},
					Meta: Meta{Key: "shared-concurrent-key", Value: "shared-concurrent-val"},
				}
				c, err := PackEndpoint(ep, cnfrt.WithDict(d))
				if err != nil {
					t.Errorf("PackEndpoint: %v", err)
					return
				}
				if _, err := ParseCompactEndpoint(c.Raw(), cnfrt.VerifyUntrusted, cnfrt.AllowDict(d)); err != nil {
					t.Errorf("ParseCompactEndpoint(VerifyUntrusted, AllowDict): %v", err)
					return
				}
				if _, err := ParseCompactEndpoint(valid.Raw(), cnfrt.VerifyUntrusted, cnfrt.AllowDict(d)); err != nil {
					t.Errorf("ParseCompactEndpoint(valid): %v", err)
					return
				}
			}
		}(r)
	}
	readerWG.Wait()
	stop.Store(true)
	writerWG.Wait()
}
