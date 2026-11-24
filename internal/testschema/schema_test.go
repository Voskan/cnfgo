package testschema

import (
	"bytes"
	"errors"
	"math"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/Voskan/cnfgo/cnfrt"
)

func ptr[T any](v T) *T { return &v }

func fixture() *Config {
	alice := &User{ID: 1, Name: "alice", Roles: map[string][]string{"admin": {"read", "write"}, "none": {}}}
	bob := &User{ID: 2, Name: "bob", Manager: alice, Friends: []*User{alice}}
	carol := &User{ID: 3, Name: "carol", Manager: bob, Friends: []*User{alice, bob, nil}}
	created := time.Date(2026, 3, 1, 12, 30, 45, 123456789, time.UTC)
	return &Config{
		Name:     "edge-proxy",
		Version:  -42,
		Enabled:  true,
		Ratio:    math.Copysign(0, -1),
		Weight:   1.5,
		Small:    -7,
		Port:     8443,
		Count:    1<<32 - 3,
		Kind:     3,
		Label:    "blue",
		Cplx:     complex(1.25, -2.5),
		Blob:     []byte{},
		Created:  created,
		Addr:     netip.MustParseAddr("2001:db8::1"),
		Prefix:   netip.MustParsePrefix("10.0.0.0/8"),
		Endpoint: netip.MustParseAddrPort("[fe80::1%eth0]:443"),
		Opt:      ptr(int64(0)),
		OptStr:   ptr(""),
		OptTime:  ptr(created.Add(time.Hour)),
		OptBool:  nil,
		Matrix:   [4]int32{1, -2, 3, math.MinInt32},
		Flags:    [3]bool{true, false, true},
		Tags:     []string{"a", "", "edge-proxy", "a"},
		Ints:     nil,
		Nested:   Meta{Key: "k", Value: "v", Count: 9},
		Owner:    carol,
		Users:    []*User{alice, bob, carol, alice},
		Metas:    []Meta{},
		Labels:   map[string]string{"app": "proxy", "tier": "edge", "": "empty"},
		Scores:   map[string]float64{"x": 1, "y": math.Inf(-1), "z": 0.5},
		ByID:     map[int64]*User{1: alice, 2: bob, 3: carol, 4: nil},
		Weights:  map[float64]string{-1: "neg", 0: "zero", 2.5: "pos"},
		Toggles:  map[bool]int32{true: 1, false: -1},
		Opts:     []*int64{nil, ptr(int64(0)), ptr(int64(7))},
		Shape:    &Rect{W: 2, H: 3},
		Shapes:   []Shape{Circle{R: 1}, nil, &Rect{W: 1, H: 1}, (*Rect)(nil)},
		Pairs:    [2]Meta{{Key: "p0"}, {Key: "p1", Count: 1}},
		Grid:     [][]int16{{1, 2}, nil, {}, {3}},
		Price:    NewMoney(1999, "EUR"),
		Limit:    MilliQuantity(1500),
		MaybeFee: ptr(NewMoney(5, "USD")),
		Ends: []Endpoint{{
			Name: "e0", Port: 80, Addr: netip.MustParseAddr("192.0.2.1"),
			Seen: created, Tags: []string{"t"}, Opt: ptr(int32(-1)),
		}},
		Skip: "not frozen",
	}
}

// expected returns cfg as Unfreeze must reproduce it.
func expected(cfg *Config) *Config {
	c := *cfg
	c.Skip = ""
	return &c
}

func mustFreeze(t testing.TB, cfg *Config) *ConfigSnapshot {
	t.Helper()
	s, err := FreezeConfig(cfg)
	if err != nil {
		t.Fatalf("FreezeConfig: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	cfg := fixture()
	for name, freeze := range map[string]func(*Config, ...cnfrt.FreezeOption) (*ConfigSnapshot, error){
		"dag": FreezeConfig, "tree": FreezeTreeConfig, "cyclic": FreezeCyclicConfig,
	} {
		t.Run(name, func(t *testing.T) {
			for _, opts := range [][]cnfrt.FreezeOption{nil, {cnfrt.WithExactStringDedup()}, {cnfrt.WithoutStringDedup()}} {
				s, err := freeze(cfg, opts...)
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.UnfreezeConfig()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, expected(cfg)) {
					t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, expected(cfg))
				}
				if err := s.validate(); err != nil {
					t.Fatalf("validate: %v", err)
				}
			}
		})
	}
}

func TestNilVersusEmpty(t *testing.T) {
	s := mustFreeze(t, fixture())
	v := s.Root()
	switch {
	case v.BlobIsNil() || v.BlobLen() != 0:
		t.Error("empty Blob must not be nil")
	case v.Tags().IsNil() || v.Tags().Len() != 4:
		t.Error("Tags")
	case !v.Ints().IsNil():
		t.Error("nil Ints must be nil")
	case v.Metas().IsNil() || v.Metas().Len() != 0:
		t.Error("empty Metas must not be nil")
	}
	g := v.Grid()
	if g.Len() != 4 || g.At(1).IsNil() != true || g.At(2).IsNil() || g.At(2).Len() != 0 {
		t.Error("Grid nil/empty distinction lost")
	}
	if o, ok := v.Opt(); !ok || o != 0 {
		t.Errorf("Opt = %d, %v; want 0, true", o, ok)
	}
	if o, ok := v.OptStr(); !ok || o != "" {
		t.Errorf("OptStr = %q, %v", o, ok)
	}
	if _, ok := v.OptBool(); ok {
		t.Error("OptBool must be unset")
	}
	opts := slices.Collect(v.Opts().All())
	if opts[0].Valid || !opts[1].Valid || opts[1].Value != 0 || opts[2].Value != 7 {
		t.Errorf("Opts = %+v", opts)
	}
	if math.Signbit(v.Ratio()) != true {
		t.Error("value -0.0 must keep its sign bit")
	}

	empty := &Config{}
	s = mustFreeze(t, empty)
	got, err := s.UnfreezeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, empty) {
		t.Fatalf("zero Config round trip: %+v", got)
	}
	r := s.Root()
	if r.Owner() != (UserView{}) || r.Owner().IsValid() || r.Shape().IsNil() != true || !r.Labels().IsNil() {
		t.Error("nil references must produce zero views")
	}
}

func TestViews(t *testing.T) {
	cfg := fixture()
	s := mustFreeze(t, cfg)
	v := s.Root()
	if v.Name() != "edge-proxy" || !v.NameEquals("edge-proxy") || v.NameLen() != 10 || string(v.AppendName([]byte("x"))) != "xedge-proxy" {
		t.Error("Name accessors")
	}
	v.WithName(func(n string) {
		if n != "edge-proxy" {
			t.Error("WithName")
		}
	})
	if v.Version() != -42 || !v.Enabled() || v.Weight() != 1.5 || v.Small() != -7 || v.Port() != 8443 ||
		v.Count() != 1<<32-3 || v.Kind() != 3 || v.Label() != "blue" || v.Cplx() != complex(1.25, -2.5) {
		t.Error("scalar getters")
	}
	if !v.Created().Equal(cfg.Created) || v.Addr() != cfg.Addr || v.Prefix() != cfg.Prefix || v.Endpoint() != cfg.Endpoint {
		t.Error("time and netip getters")
	}
	if v.Endpoint().Addr().Zone() != "eth0" {
		t.Error("zone lost")
	}
	if v.Matrix() != cfg.Matrix || v.Flags() != cfg.Flags {
		t.Error("arrays")
	}
	if got := slices.Collect(v.Tags().All()); !slices.Equal(got, cfg.Tags) || !v.Tags().EqualsAt(2, "edge-proxy") {
		t.Errorf("Tags = %q", got)
	}
	if v.Nested().Key() != "k" || v.Nested().Count() != 9 {
		t.Error("Nested")
	}
	owner := v.Owner()
	if owner.Name() != "carol" || owner.Manager().Name() != "bob" || owner.Manager().Manager().Name() != "alice" ||
		owner.Manager().Manager().Manager().IsValid() {
		t.Error("Owner chain")
	}
	if f := owner.Friends(); f.Len() != 3 || f.At(2).IsValid() || f.At(0).NodeID() != v.Users().At(0).NodeID() {
		t.Error("shared nodes must have the same ID")
	}
	if val, ok := v.Labels().Get("tier"); !ok || val != "edge" {
		t.Error("Labels.Get")
	}
	if val, ok := v.Labels().Get(""); !ok || val != "empty" {
		t.Error("Labels.Get empty key")
	}
	if _, ok := v.Labels().Get("missing"); ok {
		t.Error("Labels.Get missing")
	}
	if keys := slices.Collect(v.Labels().Keys()); !slices.IsSorted(keys) || len(keys) != 3 {
		t.Errorf("keys %q", keys)
	}
	for k, want := range cfg.Scores {
		if got, ok := v.Scores().Get(k); !ok || got != want {
			t.Errorf("Scores[%q] = %v, %v", k, got, ok)
		}
	}
	if _, ok := v.Scores().Get("nope"); ok {
		t.Error("hashed miss")
	}
	if u, ok := v.ByID().Get(2); !ok || u.Name() != "bob" {
		t.Error("ByID[2]")
	}
	if u, ok := v.ByID().Get(4); !ok || u.IsValid() {
		t.Error("ByID[4] must be present and nil")
	}
	if _, ok := v.Weights().Get(math.NaN()); ok {
		t.Error("NaN lookups must miss")
	}
	if w, ok := v.Weights().Get(math.Copysign(0, -1)); !ok || w != "zero" {
		t.Error("-0.0 key must find 0")
	}
	if x, ok := v.Toggles().Get(false); !ok || x != -1 {
		t.Error("bool keys")
	}
	if r, ok := v.Shape().AsRect(); !ok || r.W() != 2 || r.H() != 3 {
		t.Error("Shape")
	}
	sh := v.Shapes()
	if c, ok := sh.At(0).AsCircle(); !ok || c.R() != 1 || !sh.At(1).IsNil() || sh.At(3).Tag() != 2 {
		t.Error("Shapes")
	}
	if r, ok := sh.At(3).AsRect(); !ok || r.IsValid() {
		t.Error("typed nil *Rect must be a nil view with tag 2")
	}
	if m, err := v.Price(); err != nil || m != NewMoney(1999, "EUR") || v.PriceRaw() != "1999 EUR" {
		t.Error("codec Price")
	}
	if q, err := v.Limit(); err != nil || q.MilliValue() != 1500 || v.LimitRaw() != "1500m" {
		t.Error("k8s Quantity codec")
	}
	if fee, ok := v.MaybeFee(); !ok {
		t.Error("MaybeFee")
	} else if m, err := fee.Decode(); err != nil || m.Units() != 5 {
		t.Error("MaybeFee decode")
	}
	if e := v.Ends().At(0); e.Name() != "e0" || e.Seen() != cfg.Ends[0].Seen {
		t.Error("Ends")
	}
	if n := s.LenUser(); n != 3 {
		t.Errorf("LenUser = %d, want 3 (deduplicated)", n)
	}
	tree, err := FreezeTreeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tree.LenUser() <= 3 {
		t.Errorf("tree freeze must duplicate shared users, got %d", tree.LenUser())
	}
}

func TestSharingPreserved(t *testing.T) {
	cfg := fixture()
	got, err := mustFreeze(t, cfg).UnfreezeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Users[0] != got.Users[3] || got.Users[1].Manager != got.Users[0] || got.Owner != got.Users[2] || got.ByID[1] != got.Users[0] {
		t.Fatal("pointer identity not preserved")
	}
}

func TestCycles(t *testing.T) {
	a := &User{Name: "a"}
	b := &User{Name: "b", Friends: []*User{a}}
	a.Friends = []*User{b}
	cfg := &Config{Owner: a}
	if _, err := FreezeConfig(cfg); !errors.Is(err, cnfrt.ErrCycleDetected) {
		t.Fatalf("FreezeConfig on a cycle: %v", err)
	}
	if _, err := FreezeTreeConfig(cfg, cnfrt.WithMaxDepth(64)); !errors.Is(err, cnfrt.ErrCycleDetected) {
		t.Fatalf("FreezeTreeConfig on a cycle: %v", err)
	}
	s, err := FreezeCyclicConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Cyclic() || s.LenUser() != 2 {
		t.Fatalf("cyclic snapshot: cyclic=%v users=%d", s.Cyclic(), s.LenUser())
	}
	o := s.Root().Owner()
	if o.Friends().At(0).Friends().At(0).NodeID() != o.NodeID() {
		t.Fatal("cycle not preserved in views")
	}
	got, err := s.UnfreezeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner.Friends[0].Friends[0] != got.Owner {
		t.Fatal("cycle not preserved by Unfreeze")
	}
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}

	root := &Tree{Value: 1}
	child := &Tree{Value: 2, Parent: root}
	root.Children = []*Tree{child, {Value: 3, Parent: root}}
	if _, err := FreezeTree(root); !errors.Is(err, cnfrt.ErrCycleDetected) {
		t.Fatalf("FreezeTree: %v", err)
	}
	ts, err := FreezeCyclicTree(root)
	if err != nil {
		t.Fatal(err)
	}
	tv := ts.RootTree()
	if tv.Children().At(1).Parent().NodeID() != tv.NodeID() || tv.Children().At(0).Value() != 2 {
		t.Fatal("tree back-pointers")
	}
	data, err := ts.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("cyclic image must validate: %v", err)
	}
}

func TestFreezeErrors(t *testing.T) {
	if _, err := FreezeConfig(nil); !errors.Is(err, cnfrt.ErrNilRoot) {
		t.Errorf("nil root: %v", err)
	}
	if _, err := FreezeConfig(&Config{Weights: map[float64]string{math.NaN(): "x"}}); !errors.Is(err, cnfrt.ErrNaNKey) {
		t.Errorf("NaN key: %v", err)
	}
	type other struct{ Shape }
	if _, err := FreezeConfig(&Config{Shape: other{}}); !errors.Is(err, cnfrt.ErrUnsupportedType) {
		t.Errorf("unlisted implementation: %v", err)
	}
	deep := &User{}
	for range 100 {
		deep = &User{Manager: deep}
	}
	if _, err := FreezeConfig(&Config{Owner: deep}, cnfrt.WithMaxDepth(50)); !errors.Is(err, cnfrt.ErrCapacityExceeded) {
		t.Errorf("depth limit: %v", err)
	}
}

func TestChildIDsGreaterThanParent(t *testing.T) {
	s := mustFreeze(t, fixture())
	for i, u := range s.nodesUser {
		if u.FManager != 0 && u.FManager <= uint32(i+1) {
			t.Fatalf("user %d references manager %d", i+1, u.FManager)
		}
	}
	if s.rootID != 1 {
		t.Fatalf("root ID %d, want 1", s.rootID)
	}
}

func TestTimeLocations(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tz database unavailable")
	}
	times := []time.Time{
		time.Date(2026, 7, 1, 9, 0, 0, 5, ny),
		time.Date(2026, 7, 1, 9, 0, 0, 0, time.FixedZone("XYZ", -3*3600-1800)),
		time.Date(2026, 7, 1, 9, 0, 0, 0, time.FixedZone("", 3600)),
		{},
		time.Unix(-1e12, 999).UTC(),
	}
	for _, tm := range times {
		s := mustFreeze(t, &Config{Created: tm, OptTime: &tm})
		check := func(s *ConfigSnapshot) {
			got := s.Root().Created()
			_, gotOff := got.Zone()
			wantName, wantOff := tm.Zone()
			gotName, _ := got.Zone()
			if !got.Equal(tm) || gotOff != wantOff || gotName != wantName || got.Location().String() != tm.Location().String() || got.IsZero() != tm.IsZero() {
				t.Errorf("time %v decoded as %v", tm, got)
			}
		}
		check(s)
		data, err := s.MarshalCNF()
		if err != nil {
			t.Fatal(err)
		}
		s2, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted)
		if err != nil {
			t.Fatal(err)
		}
		check(s2)
	}
}

func TestMarshalModes(t *testing.T) {
	s := mustFreeze(t, fixture())
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if n, err := s.WriteTo(&buf); err != nil || n != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("WriteTo and MarshalCNF disagree: %v", err)
	}
	want := expected(fixture())
	for _, mode := range []cnfrt.VerifyMode{cnfrt.VerifyTrustedHeader, cnfrt.VerifyChecksum, cnfrt.VerifyUntrusted} {
		// Misaligned input exercises the copying path of section casts.
		mis := append([]byte{0}, data...)[1:]
		for _, in := range [][]byte{cnfrt.AlignedCopy(data), mis} {
			s2, err := UnmarshalConfigSnapshot(in, mode)
			if err != nil {
				t.Fatalf("%v: %v", mode, err)
			}
			got, err := s2.UnfreezeConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%v: image round trip mismatch", mode)
			}
		}
	}
	bad := slices.Clone(data)
	bad[len(bad)-1] ^= 0xff
	if _, err := UnmarshalConfigSnapshot(bad, cnfrt.VerifyChecksum); err == nil {
		// The last byte may be section padding; flip a byte of the string blob instead.
		in, _ := cnfrt.FindSection(mustInfos(t, data), cnfrt.SectionStrBlob, 0)
		bad = slices.Clone(data)
		bad[in.Off] ^= 0xff
		if _, err := UnmarshalConfigSnapshot(bad, cnfrt.VerifyChecksum); !errors.Is(err, cnfrt.ErrChecksumMismatch) {
			t.Fatalf("checksum mode accepted corrupt data: %v", err)
		}
	}
}

func mustInfos(t *testing.T, data []byte) []cnfrt.SectionInfo {
	_, infos, err := cnfrt.ParseHeader(data, cnfrt.VerifyTrustedHeader, SchemaHashConfig)
	if err != nil {
		t.Fatal(err)
	}
	return infos
}

func TestSchemaMismatch(t *testing.T) {
	data, err := mustFreeze(t, fixture()).MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	h, infos, _ := cnfrt.ParseHeader(data, cnfrt.VerifyTrustedHeader, 0)
	h.SchemaHash++
	secs := make([]cnfrt.Section, len(infos))
	for i, in := range infos {
		secs[i] = cnfrt.Section{Kind: in.Kind, ElemAlign: in.ElemAlign, Tag: in.Tag, Count: in.Count, Data: cnfrt.SectionData(data, in)}
	}
	other, err := cnfrt.AppendImage(nil, h, secs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalConfigSnapshot(other, cnfrt.VerifyTrustedHeader); !errors.Is(err, cnfrt.ErrSchemaMismatch) {
		t.Fatalf("schema mismatch not detected: %v", err)
	}
}

// TestUntrustedRejectsCorruption re-signs corrupted images (so only the
// semantic validator can catch the damage) and checks that VerifyUntrusted
// either rejects them or yields a snapshot that is safe to traverse.
func TestUntrustedRejectsCorruption(t *testing.T) {
	data, err := mustFreeze(t, fixture()).MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	h, infos, _ := cnfrt.ParseHeader(data, cnfrt.VerifyTrustedHeader, 0)
	rng := rand.New(rand.NewPCG(1, 2))
	rejected := 0
	for i := range 3000 {
		secs := make([]cnfrt.Section, len(infos))
		for j, in := range infos {
			secs[j] = cnfrt.Section{Kind: in.Kind, ElemAlign: in.ElemAlign, Tag: in.Tag, Count: in.Count, Data: slices.Clone(cnfrt.SectionData(data, in))}
		}
		sec := &secs[rng.IntN(len(secs))]
		for range 1 + rng.IntN(3) {
			if len(sec.Data) > 0 {
				sec.Data[rng.IntN(len(sec.Data))] ^= byte(1 << rng.IntN(8))
			}
		}
		img, err := cnfrt.AppendImage(nil, h, secs)
		if err != nil {
			t.Fatal(err)
		}
		s, err := UnmarshalConfigSnapshot(img, cnfrt.VerifyUntrusted)
		if err != nil {
			rejected++
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d: validated snapshot panicked: %v", i, r)
				}
			}()
			walk(s)
		}()
	}
	if rejected == 0 {
		t.Fatal("no corruption was detected")
	}
	t.Logf("%d of 3000 corrupted images rejected", rejected)
}

// walk reads every part of a validated snapshot.
func walk(s *ConfigSnapshot) {
	if v := s.RootConfig(); v.IsValid() {
		if _, err := v.Unfreeze(); err != nil {
			_ = err // codec errors are allowed for corrupt payloads
		}
	}
	if v := s.RootTree(); v.IsValid() {
		_, _ = v.Unfreeze()
	}
	for i := range s.LenUser() {
		u := s.view3(uint32(i + 1))
		_ = u.Name()
		for r, roles := range u.Roles().All() {
			_ = r
			for range roles.All() {
			}
		}
	}
	v := s.RootConfig()
	for k := range v.Scores().Keys() {
		v.Scores().Get(k)
	}
	for k := range v.ByID().Keys() {
		v.ByID().Get(k)
	}
	v.Labels().Get("tier")
	v.Weights().Get(2.5)
}

func TestMapFile(t *testing.T) {
	cfg := fixture()
	s := mustFreeze(t, cfg)
	path := filepath.Join(t.TempDir(), "config.cnf")
	if err := WriteConfigSnapshotAtomic(path, s); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	for _, mode := range []cnfrt.VerifyMode{cnfrt.VerifyTrustedHeader, cnfrt.VerifyChecksum, cnfrt.VerifyUntrusted} {
		m, err := MapConfigFile(path, mode)
		if err != nil {
			t.Fatalf("%v: %v", mode, err)
		}
		if m.IsMmap() != (mode != cnfrt.VerifyUntrusted) {
			t.Fatalf("%v: IsMmap = %v", mode, m.IsMmap())
		}
		name := m.Root().Name()
		got, err := m.UnfreezeConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected(cfg)) {
			t.Fatalf("%v: mapped round trip mismatch", mode)
		}
		err = m.WithRootFaultSafe(func(v ConfigView) error {
			if v.Port() != 8443 {
				return errors.New("bad port")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		// Strings obtained from a mapped snapshot are copies and survive Close.
		runtime.GC()
		if name != "edge-proxy" {
			t.Fatalf("string did not survive Close: %q", name)
		}
	}
	if _, err := MapConfigFile(filepath.Join(t.TempDir(), "missing.cnf"), cnfrt.VerifyTrustedHeader); err == nil {
		t.Fatal("missing file accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.cnf")
	os.WriteFile(empty, nil, 0o644)
	if _, err := MapConfigFile(empty, cnfrt.VerifyTrustedHeader); err == nil {
		t.Fatal("empty file accepted")
	}
}

func TestMappedCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.cnf")
	if err := WriteConfigSnapshotAtomic(path, mustFreeze(t, fixture())); err != nil {
		t.Fatal(err)
	}
	m, err := MapConfigFile(path, cnfrt.VerifyChecksum)
	if err != nil {
		t.Fatal(err)
	}
	mapping := m.mapping
	m = nil
	for i := 0; i < 50 && !mapping.Closed(); i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if !mapping.Closed() {
		t.Fatal("mapping not released after its snapshot became unreachable")
	}
}

func TestCompact(t *testing.T) {
	if unsafe.Sizeof(CompactEndpoint{}) != 16 {
		t.Fatalf("CompactEndpoint is %d bytes", unsafe.Sizeof(CompactEndpoint{}))
	}
	seen := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("CET", 3600))
	ep := &Endpoint{
		Name: "api", Port: 443, Weight: -0.25, Up: true,
		Addr: netip.MustParseAddr("fe80::2%en0"), Prefix: netip.MustParsePrefix("2001:db8::/32"),
		AP: netip.MustParseAddrPort("[fe80::3%lo0]:8080"), Seen: seen, Opt: ptr(int32(0)),
		Arr: [2]uint16{7, 65535}, Blob: []byte("xyz"), Tags: []string{"a", "b"},
		Meta: Meta{Key: "m"}, Fee: NewMoney(3, "GBP"),
	}
	c, err := PackEndpoint(ep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "api" || c.Port() != 443 || c.Weight() != -0.25 || !c.Up() || c.Addr() != ep.Addr ||
		c.Prefix() != ep.Prefix || c.AP() != ep.AP || c.Arr() != ep.Arr || c.Blob() != "xyz" {
		t.Error("compact getters")
	}
	if got := c.Seen(); !got.Equal(seen) || got.Location().String() != "CET" {
		t.Errorf("Seen = %v", got)
	}
	if o, ok := c.Opt(); !ok || o != 0 {
		t.Error("Opt")
	}
	if _, ok := c.OptName(); ok {
		t.Error("OptName must be unset")
	}
	if m, err := c.Fee(); err != nil || m != ep.Fee {
		t.Error("Fee")
	}
	if c.View().Tags().Len() != 2 || c.View().Meta().Key() != "m" {
		t.Error("View")
	}
	got, err := c.Unpack()
	if err != nil || !reflect.DeepEqual(normalizeEndpoint(got), normalizeEndpoint(ep)) {
		t.Fatalf("Unpack: %+v, %v", got, err)
	}
	for _, mode := range []cnfrt.VerifyMode{cnfrt.VerifyTrustedHeader, cnfrt.VerifyChecksum, cnfrt.VerifyUntrusted} {
		p, err := ParseCompactEndpoint(c.Raw(), mode)
		if err != nil || !p.Equal(c) {
			t.Fatalf("%v: %v", mode, err)
		}
	}
	if _, err := ParseCompactEndpoint("garbage", cnfrt.VerifyUntrusted); err == nil {
		t.Fatal("garbage accepted")
	}
	raw := []byte(c.Raw())
	raw[2] ^= 1 // corrupt the inline root record
	if _, err := ParseCompactEndpoint(string(raw), cnfrt.VerifyUntrusted); err == nil {
		t.Fatal("inline record mismatch accepted")
	}
	cDet1, err := PackEndpoint(ep, cnfrt.WithDeterministicSeed())
	if err != nil {
		t.Fatal(err)
	}
	cDet2, err := PackEndpoint(ep, cnfrt.WithDeterministicSeed())
	if err != nil {
		t.Fatal(err)
	}
	if !cDet1.Equal(cDet2) {
		t.Error("packing with WithDeterministicSeed must be deterministic")
	}
	if !c.NameEquals("api") || c.NameEquals("other") || c.NameLen() != 3 {
		t.Error("compact string helpers")
	}
	if !c.BlobEquals("xyz") || c.BlobLen() != 3 || c.BlobIsNil() {
		t.Error("compact bytes helpers")
	}
	borrowed, err := BorrowCompactEndpoint(c.Raw())
	if err != nil || !borrowed.Equal(c) || borrowed.Port() != 443 {
		t.Fatalf("BorrowCompactEndpoint: %v", err)
	}
	var zero CompactEndpoint
	if !zero.IsZero() || zero.Name() != "" || zero.View().IsValid() {
		t.Error("zero Compact")
	}

	// Compact values work as FrozenMap values through their SelfCodec.
	codec, err := cnfrt.DefaultCodec[CompactEndpoint]()
	if err != nil {
		t.Fatal(err)
	}
	fm, err := cnfrt.NewFrozenMap(map[string]CompactEndpoint{"api": c}, &cnfrt.MapOptions[CompactEndpoint]{Value: codec})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := fm.Get("api"); !ok || v.Port() != 443 {
		t.Fatal("FrozenMap of Compact")
	}
}

func normalizeEndpoint(e *Endpoint) *Endpoint {
	c := *e
	c.Seen = c.Seen.UTC()
	return &c
}

func TestLeakDetectorIntegration(t *testing.T) {
	reports := make(chan cnfrt.LeakReport, 16)
	cnfrt.EnableLeakDetector(time.Millisecond, func(r cnfrt.LeakReport) { reports <- r })
	defer cnfrt.DisableLeakDetector()
	s := mustFreeze(t, fixture())
	s.MarkRetired()
	select {
	case r := <-reports:
		if r.Kind != cnfrt.LeakSnapshotRetained || r.Label != "ConfigSnapshot" {
			t.Fatalf("unexpected report %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retired snapshot that is still referenced was not reported")
	}
	runtime.KeepAlive(s)
}

// TestUntrustedRejectsArtificialCycles builds DAG images whose node
// references were rewritten into cycles (1 -> 2 -> 1 within a table, and a
// self-reference) and requires VerifyUntrusted to reject them, while the
// same image flagged as cyclic is accepted and can be unfrozen.
func TestUntrustedRejectsArtificialCycles(t *testing.T) {
	alice := &User{Name: "alice"}
	bob := &User{Name: "bob", Manager: alice}
	s := mustFreeze(t, &Config{Owner: bob})
	if len(s.nodesUser) != 2 || s.nodesUser[0].FManager != 2 {
		t.Fatalf("unexpected layout: %+v", s.nodesUser)
	}
	for name, mutate := range map[string]func(c *ConfigSnapshot){
		"two-cycle": func(c *ConfigSnapshot) { c.nodesUser[1].FManager = 1 },
		"self":      func(c *ConfigSnapshot) { c.nodesUser[0].FManager = 1 },
		"dangling":  func(c *ConfigSnapshot) { c.nodesUser[0].FManager = 3 },
	} {
		c := *s
		c.nodesUser = slices.Clone(s.nodesUser)
		mutate(&c)
		data, err := c.MarshalCNF()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted); !errors.Is(err, cnfrt.ErrBoundsViolation) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "dangling" {
			continue
		}
		c.flags |= cnfrt.FlagCyclicAllowed
		data, _ = c.MarshalCNF()
		cs, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted)
		if err != nil {
			t.Fatalf("%s flagged cyclic: %v", name, err)
		}
		if _, err := cs.UnfreezeConfig(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestUntrustedRejectsNonCanonical mutates a snapshot into states whose
// references are all in bounds but whose values a frozen graph never
// contains: boolean bytes other than 0 and 1, duplicate-collapsing boolean
// map keys, and hash indexes that cannot locate their keys.
func TestUntrustedRejectsNonCanonical(t *testing.T) {
	cfg := fixture()
	cfg.Enabled = true
	yes := true
	cfg.OptBool = &yes
	cfg.Toggles = map[bool]int32{false: 1, true: 2}
	cfg.Scores = map[string]float64{}
	for i := range 40 {
		cfg.Scores["score-"+strconv.Itoa(i)] = float64(i)
	}
	s := mustFreeze(t, cfg)
	root := s.rootID - 1
	scores := s.nodesConfig[root].FScores
	idx := scores.Idx
	for name, mutate := range map[string]func(c *ConfigSnapshot){
		"bool field":    func(c *ConfigSnapshot) { c.nodesConfig[root].FEnabled = 2 },
		"bool array":    func(c *ConfigSnapshot) { c.nodesConfig[root].FFlags[1] = 7 },
		"optional bool": func(c *ConfigSnapshot) { c.nodesConfig[root].FOptBool = 0xff },
		"bool map key":  func(c *ConfigSnapshot) { c.pool10[1].K = 5 },
		"full hash index": func(c *ConfigSnapshot) {
			for i := range idx.Len {
				if c.pool6[idx.Off+i] == 0 {
					c.pool6[idx.Off+i] = 1
				}
			}
		},
		"misplaced hash slot": func(c *ConfigSnapshot) {
			// Move one entry to the empty slot farthest from its position.
			region := c.pool6[idx.Off : idx.Off+idx.Len]
			from := slices.IndexFunc(region, func(s uint32) bool { return s != 0 })
			for d := len(region) / 2; d < len(region); d++ {
				to := (from + d) % len(region)
				if region[to] == 0 {
					region[to], region[from] = region[from], 0
					return
				}
			}
			t.Fatal("no empty slot")
		},
	} {
		c := *s
		c.nodesConfig = slices.Clone(s.nodesConfig)
		c.pool6 = slices.Clone(s.pool6)
		c.pool10 = slices.Clone(s.pool10)
		mutate(&c)
		data, err := c.MarshalCNF()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted); !errors.Is(err, cnfrt.ErrBoundsViolation) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	data, _ := s.MarshalCNF()
	if _, err := UnmarshalConfigSnapshot(data, cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("unmodified snapshot rejected: %v", err)
	}
}
