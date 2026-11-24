package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// generateArgs returns the arguments of the //go:generate cnfgo directive
// in the Go files of dir.
func generateArgs(t *testing.T, dir string) [][]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	var out [][]string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			rest, ok := strings.CutPrefix(line, "//go:generate go run github.com/Voskan/cnfgo/cmd/cnfgo ")
			if ok {
				out = append(out, strings.Fields(rest))
			}
		}
	}
	return out
}

// TestGolden regenerates every checked-in generated file of the module and
// requires the result to be identical.
func TestGolden(t *testing.T) {
	root := filepath.Join("..", "..")
	var dirs []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if err == nil && !d.IsDir() && strings.HasSuffix(path, "_cnf.gen.go") {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if len(dirs) == 0 {
		t.Fatal("no generated files found")
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		directives := generateArgs(t, dir)
		if len(directives) == 0 {
			t.Errorf("%s: no go:generate directive", dir)
		}
		for _, args := range directives {
			path, src, err := build(append(args, dir))
			if err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(src, want) {
				t.Errorf("%s is stale; run go generate ./...", path)
			}
			checkNoUnsafe(t, path, src)
		}
	}
}

func checkNoUnsafe(t *testing.T, name string, src []byte) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, imp := range f.Imports {
		if imp.Path.Value == `"unsafe"` {
			t.Errorf("%s imports unsafe", name)
		}
	}
	full, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(full, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "unsafe" {
				t.Errorf("%s references unsafe.%s", name, sel.Sel.Name)
			}
		}
		return true
	})
}

// tempModule writes a module that depends on this checkout and contains
// one package with the given source file.
func tempModule(t *testing.T, src string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command not available")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gomod := "module example.com/m\n\ngo 1.26\n\nrequire github.com/Voskan/cnfgo v0.0.0\n\nreplace github.com/Voskan/cnfgo => " + repo + "\n"
	for name, content := range map[string]string{"go.mod": gomod, "types.go": src} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func goCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

const prefixSource = `package m

import "time"

type Node struct {
	Name     string
	Children []*Node
	Attrs    map[string]int32 ` + "`cnf:\",hash\"`" + `
	At       time.Time
	Score    *float64
	Ignored  chan int ` + "`cnf:\"-\"`" + `
}
`

const prefixTest = `package m

import "testing"

func TestGenerated(t *testing.T) {
	shared := &Node{Name: "leaf"}
	root := &Node{Name: "root", Children: []*Node{shared, shared}, Attrs: map[string]int32{"a": 1}}
	s, err := FreezeXNode(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.LenNode() != 3 {
		t.Fatalf("-tree must duplicate shared nodes: %d", s.LenNode())
	}
	v := s.Root()
	if v.Children().At(1).Name() != "leaf" {
		t.Fatal("children")
	}
	if a, ok := v.Attrs().Get("a"); !ok || a != 1 {
		t.Fatal("attrs")
	}
	var _ XNodeView = v
	var _ XStringInt32HashMapView = v.Attrs()
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalXNodeSnapshot(data, 2); err != nil {
		t.Fatal(err)
	}
}
`

func TestPrefixAndTree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, prefixSource)
	if err := os.WriteFile(filepath.Join(dir, "m_test.go"), []byte(prefixTest), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{"-type=Node", "-prefix=X", "-tree", "-out=node_cnf.gen.go", dir})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

func TestDiagnostics(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

type Any interface{ M() }

type BadKey struct{ P *int }

type Bad struct {
	C          chan int
	F          func()
	I          Any
	K          map[BadKey]int
	E          interface{}
	PP         **int
	Tag        int            `+"`cnf:\",bogus\"`"+`
	BadHashTag map[string]int `+"`cnf:\"hash\"`"+`
	H          []int          `+"`cnf:\",hash\"`"+`
	Anon       struct{ X int }
	Mu         sync.Mutex
	N          atomic.Int64
	UP         unsafe.Pointer
	U          uintptr
}
`)
	_, _, err := build([]string{"-type=Bad,Missing", dir})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"type Missing not found",
		"Bad.C: type chan int is not supported",
		"Bad.F: type func() is not supported",
		"interface Any has no //cnf:implementations directive",
		"map key type",
		"Bad.E: interface type interface{} is not supported",
		"pointer to *int is not supported",
		`unknown cnf tag option "bogus"`,
		`use ` + "`cnf:\",hash\"`",
		"the hash option applies only to map fields",
		"anonymous struct type",
		"Bad.Mu: type Mutex from package sync has unexported fields; register a -codec for it",
		"Bad.N: type Int64 from package sync/atomic has unexported fields",
		"Bad.UP: unsupported basic type unsafe.Pointer",
		"Bad.U: unsupported basic type uintptr",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing diagnostic %q in:\n%v", want, err)
		}
	}
}

func TestParseCodec(t *testing.T) {
	c, err := parseCodec("example.com/a/b.T=example.com/c.Adapter[T,*T]")
	if err != nil {
		t.Fatal(err)
	}
	if c.target != "example.com/a/b.T" || c.adapterPath != "example.com/c" || c.adapterName != "Adapter" || len(c.typeArgs) != 2 || c.stem != "BT" {
		t.Fatalf("parsed %+v", c)
	}
	for _, bad := range []string{"x", "a.T=", "=b.C", "a.T=b.C[T", "a.T=nodot", "T=b.C"} {
		if _, err := parseCodec(bad); err == nil {
			t.Errorf("parseCodec(%q) succeeded", bad)
		}
	}
}

// TestFlatLayoutPortable checks that every generated flat type has the same
// size and field offsets on all supported architectures, which the runtime
// init check can only verify on the architecture it runs on.
func TestFlatLayoutPortable(t *testing.T) {
	lp, err := loadPackage(filepath.Join("..", "..", "internal", "testschema"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	type layout struct {
		size    int64
		offsets []int64
	}
	measure := func(arch string, st *types.Struct) layout {
		sizes := types.SizesFor("gc", arch)
		fields := make([]*types.Var, st.NumFields())
		for i := range fields {
			fields[i] = st.Field(i)
		}
		return layout{sizes.Sizeof(st), sizes.Offsetsof(fields)}
	}
	n := 0
	for _, name := range lp.pkg.Scope().Names() {
		if !strings.HasPrefix(name, "flat") {
			continue
		}
		st, ok := lp.pkg.Scope().Lookup(name).Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		n++
		want := measure("amd64", st)
		for _, arch := range []string{"386", "arm", "arm64", "wasm", "mips", "s390x", "riscv64"} {
			if got := measure(arch, st); !reflect.DeepEqual(got, want) {
				t.Errorf("%s on %s: %+v, amd64: %+v", name, arch, got, want)
			}
		}
	}
	if n < 10 {
		t.Fatalf("only %d flat types found", n)
	}
}

// TestCompactBoundsChecks compiles the test schema with bounds-check
// diagnostics and requires fixed-offset Compact getters to perform at most
// one bounds check each.
func TestCompactBoundsChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("recompiles a package")
	}
	const pkg = "github.com/Voskan/cnfgo/internal/testschema"
	cmd := exec.Command("go", "build", "-o", os.DevNull, "-gcflags="+pkg+"=-d=ssa/check_bce/debug=1", pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	file := filepath.Join("..", "..", "internal", "testschema", "config_cnf.gen.go")
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	span := map[string][2]int{}
	for i, l := range lines {
		rest, ok := strings.CutPrefix(l, "func (c CompactEndpoint) ")
		if !ok {
			continue
		}
		name := rest[:strings.IndexByte(rest, '(')]
		end := i
		for lines[end] != "}" {
			end++
		}
		span[name] = [2]int{i + 1, end + 1}
	}
	checks := map[string]int{}
	for _, l := range strings.Split(string(out), "\n") {
		_, rest, ok := strings.Cut(l, "config_cnf.gen.go:")
		if !ok || !strings.Contains(rest, "Found Is") {
			continue
		}
		var line int
		fmt.Sscanf(rest, "%d:", &line)
		for name, r := range span {
			if r[0] <= line && line <= r[1] {
				checks[name]++
			}
		}
	}
	for _, name := range []string{"Port", "Weight", "Up", "Opt", "Arr", "Name", "NameEquals", "Blob", "BlobEquals", "OptName"} {
		if _, ok := span[name]; !ok {
			t.Fatalf("getter %s not found", name)
		}
		if checks[name] > 1 {
			t.Errorf("CompactEndpoint.%s performs %d bounds checks, want at most 1", name, checks[name])
		}
	}
	for _, name := range []string{"cnfExt", "Seen", "Addr", "Prefix"} {
		if _, ok := span[name]; !ok {
			t.Fatalf("method %s not found", name)
		}
		if checks[name] > 2 {
			t.Errorf("CompactEndpoint.%s performs %d bounds checks, want at most 2", name, checks[name])
		}
	}
	if _, ok := span["cnfTime"]; !ok {
		t.Fatal("helper cnfTime not found")
	}
	// cnfTime takes a dynamic offset (1 extra check) and inlines cnfExt on
	// the cold non-UTC branch (2 checks).
	if checks["cnfTime"] > 3 {
		t.Errorf("CompactEndpoint.cnfTime performs %d bounds checks, want at most 3", checks["cnfTime"])
	}
}

// TestInlining compiles the test schema and the runtime with inlining
// diagnostics and requires the hot accessors of generated views and Compact
// values to stay within the inliner budget, and Dict.MustGet, whose panic
// path would otherwise be duplicated at every call site, to stay out of line.
func TestInlining(t *testing.T) {
	if testing.Short() {
		t.Skip("recompiles packages")
	}
	const pkg = "github.com/Voskan/cnfgo/internal/testschema"
	const rt = "github.com/Voskan/cnfgo/cnfrt"
	cmd := exec.Command("go", "build", "-o", os.DevNull, "-gcflags="+pkg+"=-m=2", "-gcflags="+rt+"=-m=2", pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	can := map[string]bool{}
	cannot := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(l, ": can inline "); ok {
			name, _, _ := strings.Cut(rest, " ")
			can[name] = true
		} else if _, rest, ok := strings.Cut(l, ": cannot inline "); ok {
			name, reason, _ := strings.Cut(rest, ": ")
			cannot[name] = reason
		}
	}
	for _, name := range []string{
		"HashUint64", "HashUint32", "U32S",
		"(*ConfigSnapshot).str", "(*ConfigSnapshot).rawStr",
		"ConfigView.Name", "ConfigView.NameEquals", "ConfigView.AppendName", "ConfigView.WithName",
		"ConfigView.Blob", "ConfigView.BlobEquals", "ConfigView.AppendBlob", "ConfigView.WithBlob",
		"MetaView.Key", "StringSliceView.At", "StringSliceView.EqualsAt",
		"CompactEndpoint.Port", "CompactEndpoint.Seen",
		"CompactEndpoint.Addr", "CompactEndpoint.cnfExt",
		"BorrowCompactEndpoint", "BorrowCompactEndpointBytes",
		"(*CompactEndpoint).DecodeCNF", "compactEndpointCodec.Decode",
	} {
		if !can[name] {
			t.Errorf("%s is not inlinable: %s", name, cannot[name])
		}
	}
	if r := cannot["(*Dict).MustGet"]; !strings.Contains(r, "noinline") {
		t.Errorf("(*Dict).MustGet must be marked go:noinline (can inline: %v)", can["(*Dict).MustGet"])
	}
}

// TestEdgeCases exercises generator edge cases: colliding map/slice view
// names, map[K]*Scalar unfreezing, empty structs, type-alias roots,
// zero-length struct arrays, struct/array map keys, and versioned (/v2)
// codec import paths.
func TestEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

import "example.com/m/codec/v2"

type Empty struct{}

type Item struct {
	ID int64
}

type item struct {
	Label string
}

type MyMap map[string]int
type MyBytes []byte

type PairKey struct {
	Tag string
	Seq int32
}

type Root struct {
	ByVal   map[string]Item
	ByPtr   map[string]*Item
	OptMap  map[string]*int64
	NamedM  MyMap
	RawM    map[string]int
	NamedB  []MyBytes
	RawB    [][]byte
	Blank   Empty
	Lower   item
	ZeroArr [0]Item
	ArrKey  map[[2]uint16]string
	StructK map[PairKey]int64 `+"`cnf:\",hash\"`"+`
	Custom  codec.Val
}

type RootAlias = Root
`)
	codecDir := filepath.Join(dir, "codec", "v2")
	if err := os.MkdirAll(codecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codecSrc := `package codec

type Val struct{ S string }

type Adapter struct{}

func (Adapter) Size(v Val) int              { return len(v.S) }
func (Adapter) Append(dst []byte, v Val) []byte { return append(dst, v.S...) }
func (Adapter) Decode(src string) (Val, error)  { return Val{S: src}, nil }
`
	if err := os.WriteFile(filepath.Join(codecDir, "codec.go"), []byte(codecSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	testSrc := `package m

import (
	"testing"

	"example.com/m/codec/v2"
	"github.com/Voskan/cnfgo/cnfrt"
)

func TestEdgeCasesRoundTrip(t *testing.T) {
	n := int64(42)
	src := &Root{
		ByVal:   map[string]Item{"a": {ID: 1}},
		ByPtr:   map[string]*Item{"b": {ID: 2}},
		OptMap:  map[string]*int64{"x": &n, "nil": nil},
		NamedM:  MyMap{"k": 7},
		RawM:    map[string]int{"k": 8},
		NamedB:  []MyBytes{MyBytes("hi")},
		RawB:    [][]byte{[]byte("lo")},
		Lower:   item{Label: "low"},
		ArrKey:  map[[2]uint16]string{{1, 2}: "pair"},
		StructK: map[PairKey]int64{{Tag: "t", Seq: 9}: 99},
		Custom:  codec.Val{S: "ok"},
	}
	s, err := FreezeRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	v := s.Root()
	if got, ok := v.ByVal().Get("a"); !ok || got.ID() != 1 {
		t.Fatal("ByVal")
	}
	if got, ok := v.ByPtr().Get("b"); !ok || got.ID() != 2 {
		t.Fatal("ByPtr")
	}
	if got, ok := v.OptMap().Get("x"); !ok || !got.Valid || got.Value != 42 {
		t.Fatal("OptMap")
	}
	if got, ok := v.ArrKey().Get([2]uint16{1, 2}); !ok || got != "pair" {
		t.Fatal("ArrKey")
	}
	if got, ok := v.StructK().Get(PairKey{Tag: "t", Seq: 9}); !ok || got != 99 {
		t.Fatal("StructK")
	}
	if got, err := v.Custom(); err != nil || got.S != "ok" {
		t.Fatalf("Custom: %+v, %v", got, err)
	}
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := UnmarshalRootSnapshot(data, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s2.UnfreezeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if *out.OptMap["x"] != 42 || out.OptMap["nil"] != nil || out.StructK[PairKey{Tag: "t", Seq: 9}] != 99 {
		t.Fatalf("unfreeze mismatch: %+v", out)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "edge_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{
		"-type=RootAlias",
		"-codec=example.com/m/codec/v2.Val=example.com/m/codec/v2.Adapter",
		"-out=root_cnf.gen.go",
		dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

// TestSchemaHashIncludesCodecAdapter checks that two schemas that differ
// only in the adapter registered for a type have different hashes.
func TestSchemaHashIncludesCodecAdapter(t *testing.T) {
	lp, err := loadPackage(filepath.Join("..", "..", "internal", "testschema"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	const money = "github.com/Voskan/cnfgo/internal/testschema.Money="
	const quantity = "-codec=github.com/Voskan/cnfgo/internal/testschema.Quantity=github.com/Voskan/cnfgo/cnfrt/codecs/k8s.Quantity[T,*T]"
	hash := func(adapter string) uint64 {
		t.Helper()
		sc, err := analyze(lp, options{
			types:  []string{"Config", "Tree"},
			codecs: []string{money + adapter, strings.TrimPrefix(quantity, "-codec=")},
		})
		if err != nil {
			t.Fatal(err)
		}
		return sc.hash
	}
	a := hash("github.com/Voskan/cnfgo/internal/testschema.MoneyCodec")
	b := hash("github.com/Voskan/cnfgo/internal/testschema.OtherMoneyCodec")
	c := hash("example.com/other/testschema.MoneyCodec")
	if a == b || a == c || b == c {
		t.Fatalf("schema hash ignores the codec adapter: %#x %#x %#x", a, b, c)
	}
	if a != hash("github.com/Voskan/cnfgo/internal/testschema.MoneyCodec") {
		t.Fatal("schema hash is not deterministic")
	}
}

// TestSelfRecursiveCompact verifies that a self-recursive root packed into a
// slim Compact value stores root[0] inline and root[1:] in its extra-root
// table section, round-tripping through View, ParseCompact, and Unpack.
func TestSelfRecursiveCompact(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

type Chain struct {
	Name string
	Val  int64
	Next *Chain
}
`)
	testSrc := `package m

import (
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

func TestChainCompact(t *testing.T) {
	src := &Chain{Name: "a", Val: 1, Next: &Chain{Name: "b", Val: 2, Next: &Chain{Name: "c", Val: 3}}}
	c, err := PackChain(src)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "a" || c.Val() != 1 {
		t.Fatalf("direct getters: %q %d", c.Name(), c.Val())
	}
	v := c.View()
	if v.Next().Name() != "b" || v.Next().Next().Name() != "c" || v.Next().Next().Next().IsValid() {
		t.Fatal("chain view mismatch")
	}
	if _, err := ParseCompactChain(c.Raw(), cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("ParseCompactChain(VerifyUntrusted): %v", err)
	}
	out, err := c.Unpack()
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "a" || out.Next.Name != "b" || out.Next.Next.Val != 3 || out.Next.Next.Next != nil {
		t.Fatalf("unpack mismatch: %+v", out)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "chain_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{
		"-type=Chain",
		"-compact=Chain",
		"-out=chain_cnf.gen.go",
		dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

// TestUnexportedFieldNames generates Freeze and Compact code for a type
// whose unexported fields share names with identifiers that older
// generated code used internally, and checks that the package compiles and
// round-trips.
func TestUnexportedFieldNames(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

import (
	"net/netip"
	"time"
)

type Rec struct {
	id    int64
	snap  string
	val   []byte
	raw   string
	str   *string
	ext   []string
	time  time.Time
	addr  netip.Addr
	u32   uint32
	check bool
	load  map[string]int32
}
`)
	testSrc := `package m

import (
	"net/netip"
	"testing"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

func TestRec(t *testing.T) {
	s := "opt"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := &Rec{
		id: 7, snap: "snap", val: []byte("val"), raw: "raw", str: &s,
		ext: []string{"e1", "e2"}, time: at, addr: netip.MustParseAddr("10.0.0.1"),
		u32: 32, check: true, load: map[string]int32{"k": 9},
	}
	c, err := PackRec(src)
	if err != nil {
		t.Fatal(err)
	}
	if c.id() != 7 || c.snap() != "snap" || c.val() != "val" || c.raw() != "raw" || c.u32() != 32 || !c.check() {
		t.Fatal("compact getters mismatch")
	}
	if !c.time().Equal(at) || c.addr() != netip.MustParseAddr("10.0.0.1") {
		t.Fatal("compact time/addr mismatch")
	}
	if p, ok := c.str(); !ok || p != "opt" {
		t.Fatalf("compact str = %q, %v", p, ok)
	}
	v := c.View()
	if v.ext().Len() != 2 || v.ext().At(1) != "e2" {
		t.Fatal("view ext mismatch")
	}
	if n, ok := v.load().Get("k"); !ok || n != 9 {
		t.Fatal("view load mismatch")
	}
	if _, err := ParseCompactRec(c.Raw(), cnfrt.VerifyUntrusted); err != nil {
		t.Fatal(err)
	}
	snap, err := FreezeRec(src)
	if err != nil {
		t.Fatal(err)
	}
	if r := snap.Root(); r.id() != 7 || r.raw() != "raw" || !r.check() {
		t.Fatal("snapshot view mismatch")
	}
	out, err := c.Unpack()
	if err != nil {
		t.Fatal(err)
	}
	if out.id != 7 || *out.str != "opt" || string(out.val) != "val" || out.load["k"] != 9 || !out.time.Equal(at) {
		t.Fatalf("unpack mismatch: %+v", out)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "rec_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{"-type=Rec", "-compact=Rec", "-out=rec_cnf.gen.go", dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

// TestReservedNamesFailGeneration checks that schema errors about reserved
// names are reported by the analyzer and make the command exit non-zero
// without writing output.
func TestReservedNamesFailGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

type Bad struct {
	cnfSnap int
	Raw     string
	Name    string
}
`)
	cmd := exec.Command("go", "run", "github.com/Voskan/cnfgo/cmd/cnfgo", "-type=Bad", "-compact=Bad", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("generator succeeded on a schema with reserved names:\n%s", out)
	}
	for _, want := range []string{
		"field name cnfSnap is reserved",
		"generated getter Raw collides with a method of CompactBad",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing diagnostic %q in:\n%s", want, out)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.gen.go")); len(matches) != 0 {
		t.Fatalf("generator wrote %v despite errors", matches)
	}
}

// TestSchemaHashIndependentOfPackagePath generates the same schema in two
// packages with different import paths and requires equal schema hashes,
// so values remain readable after a package is moved or a module gains a
// major version suffix.
func TestSchemaHashIndependentOfPackagePath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds temporary modules")
	}
	const src = `package %s

import "time"

type Kind uint8

type Item struct {
	Name string
	Kind Kind
	At   time.Time
	Tags map[string]Kind
	Next *Item
	Sub  []Sub
}

type Sub struct{ V int32 }
`
	hash := func(module, pkg string) uint64 {
		t.Helper()
		dir := tempModule(t, fmt.Sprintf(src, pkg))
		gomod := filepath.Join(dir, "go.mod")
		b, err := os.ReadFile(gomod)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gomod, []byte(strings.Replace(string(b), "module example.com/m", "module "+module, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		lp, err := loadPackage(dir, "", "")
		if err != nil {
			t.Fatal(err)
		}
		sc, err := analyze(lp, options{types: []string{"Item"}, compact: []string{"Item"}})
		if err != nil {
			t.Fatal(err)
		}
		return sc.hash
	}
	a := hash("example.com/pkga", "pkga")
	b := hash("example.org/pkgb/v2", "pkgb")
	if a != b {
		t.Fatalf("schema hash depends on the package path: %#x != %#x", a, b)
	}
}

// TestSchemaHashIndependentOfPrefix checks that adding -prefix to avoid Go
// identifier collisions does not change SchemaHash for identical layouts.
func TestSchemaHashIndependentOfPrefix(t *testing.T) {
	lp, err := loadPackage(filepath.Join("..", "..", "internal", "testschema"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	baseOpts := options{
		types:   []string{"Config", "Tree"},
		compact: []string{"Endpoint"},
		codecs: []string{
			"github.com/Voskan/cnfgo/internal/testschema.Money=github.com/Voskan/cnfgo/internal/testschema.MoneyCodec",
			"github.com/Voskan/cnfgo/internal/testschema.Quantity=github.com/Voskan/cnfgo/cnfrt/codecs/k8s.Quantity[T,*T]",
		},
	}
	scNoPrefix, err := analyze(lp, baseOpts)
	if err != nil {
		t.Fatal(err)
	}
	prefixedOpts := baseOpts
	prefixedOpts.prefix = "Alpha"
	scPrefixed, err := analyze(lp, prefixedOpts)
	if err != nil {
		t.Fatal(err)
	}
	if scNoPrefix.hash != scPrefixed.hash {
		t.Fatalf("schema hash changed with -prefix: none=%#x Alpha=%#x", scNoPrefix.hash, scPrefixed.hash)
	}
}

// TestRecursiveMapLargeKeys verifies that recursive maps with >= 256 entries at
// multiple levels preserve key caching order between pass 1 and pass 2 for both
// sorted and hash-indexed maps.
func TestRecursiveMapLargeKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

type RecMap struct {
	Tag    string
	Sorted map[string]*RecMap
	Hashed map[string]*RecMap `+"`cnf:\",hash\"`"+`
}
`)
	testSrc := `package m

import (
	"fmt"
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

func TestRecMapLarge(t *testing.T) {
	childS := &RecMap{Tag: "childS", Sorted: make(map[string]*RecMap, 300)}
	for i := range 300 {
		childS.Sorted[fmt.Sprintf("cs-%04d", i)] = &RecMap{Tag: fmt.Sprintf("ls-%d", i)}
	}
	childH := &RecMap{Tag: "childH", Hashed: make(map[string]*RecMap, 300)}
	for i := range 300 {
		childH.Hashed[fmt.Sprintf("ch-%04d", i)] = &RecMap{Tag: fmt.Sprintf("lh-%d", i)}
	}
	root := &RecMap{
		Tag:    "root",
		Sorted: make(map[string]*RecMap, 300),
		Hashed: make(map[string]*RecMap, 300),
	}
	for i := range 299 {
		root.Sorted[fmt.Sprintf("ps-%04d", i)] = &RecMap{Tag: "p"}
		root.Hashed[fmt.Sprintf("ph-%04d", i)] = &RecMap{Tag: "p"}
	}
	root.Sorted["ps-0000"] = childS
	root.Hashed["ph-0000"] = childH

	s, err := FreezeRecMap(root)
	if err != nil {
		t.Fatalf("FreezeRecMap: %v", err)
	}
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := UnmarshalRecMapSnapshot(data, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatalf("VerifyUntrusted: %v", err)
	}
	cs, ok := s2.Root().Sorted().Get("ps-0000")
	if !ok || cs.Sorted().Len() != 300 {
		t.Fatalf("sorted child mismatch: ok=%v len=%d", ok, cs.Sorted().Len())
	}
	ch, ok := s2.Root().Hashed().Get("ph-0000")
	if !ok || ch.Hashed().Len() != 300 {
		t.Fatalf("hashed child mismatch: ok=%v len=%d", ok, ch.Hashed().Len())
	}
	c, err := PackRecMap(root)
	if err != nil {
		t.Fatalf("PackRecMap: %v", err)
	}
	if _, err := ParseCompactRecMap(c.Raw(), cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("ParseCompactRecMap: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "recmap_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{"-type=RecMap", "-compact=RecMap", "-out=recmap_cnf.gen.go", dir})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

// TestUntrustedZonedAddrWorkBudget checks that VerifyUntrusted debits
// workBudget for IPv6 zone lengths on netip.Addr and netip.AddrPort map keys,
// rejecting crafted quadratic amplification while accepting valid maps.
func TestUntrustedZonedAddrWorkBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

import "net/netip"

type HashedZoneMap struct {
	M map[netip.AddrPort]int32 `+"`cnf:\",hash\"`"+`
}

type ZoneMaps struct {
	Sorted []map[netip.Addr]int32
	Hashed []HashedZoneMap
}
`)
	testSrc := `package m

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

func TestZonedWorkBudget(t *testing.T) {
	zoneA := strings.Repeat("a", 16<<10) + "1"
	zoneB := strings.Repeat("a", 16<<10) + "2"
	a1 := netip.MustParseAddr("fe80::1").WithZone(zoneA)
	a2 := netip.MustParseAddr("fe80::1").WithZone(zoneB)
	ap1 := netip.AddrPortFrom(a1, 443)
	ap2 := netip.AddrPortFrom(a2, 443)

	// A single map with long zones must pass validation.
	validSnap, err := FreezeZoneMaps(&ZoneMaps{
		Sorted: []map[netip.Addr]int32{{a1: 1, a2: 2}},
		Hashed: []HashedZoneMap{{M: map[netip.AddrPort]int32{ap1: 1, ap2: 2}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	validData, err := validSnap.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalZoneMapsSnapshot(validData, cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("valid long-zone map rejected: %v", err)
	}

	// Reusing two long zones across thousands of maps must hit workBudget.
	manySorted := make([]map[netip.Addr]int32, 3000)
	for i := range manySorted {
		manySorted[i] = map[netip.Addr]int32{a1: 1, a2: 2}
	}
	s1, err := FreezeZoneMaps(&ZoneMaps{Sorted: manySorted})
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s1.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalZoneMapsSnapshot(b1, cnfrt.VerifyUntrusted); !errors.Is(err, cnfrt.ErrBoundsViolation) {
		t.Fatalf("quadratic sorted zoned maps accepted: %v", err)
	}

	manyHashed := make([]HashedZoneMap, 3000)
	for i := range manyHashed {
		manyHashed[i] = HashedZoneMap{M: map[netip.AddrPort]int32{ap1: 1, ap2: 2}}
	}
	s2, err := FreezeZoneMaps(&ZoneMaps{Hashed: manyHashed})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s2.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalZoneMapsSnapshot(b2, cnfrt.VerifyUntrusted); !errors.Is(err, cnfrt.ErrBoundsViolation) {
		t.Fatalf("quadratic hashed zoned maps accepted: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "zone_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{"-type=ZoneMaps", "-out=zonemaps_cnf.gen.go", dir})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

// TestMultiplePrefixesMapKeyHelpers verifies that two schemas generated into
// the same package with different -prefix flags compile cleanly even when both
// define maps with struct and scalar-array keys.
func TestMultiplePrefixesMapKeyHelpers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

type Pair struct {
	K string
	V int32
}

type SchemaA struct {
	ByPair  map[Pair]int32
	ByFloat map[[2]float64]int32 `+"`cnf:\",hash\"`"+`
}

type SchemaB struct {
	ByPair  map[Pair]int32       `+"`cnf:\",hash\"`"+`
	ByFloat map[[2]float64]int32
}
`)
	testSrc := `package m

import "testing"

func TestCoexist(t *testing.T) {
	sa, err := FreezeAlphaSchemaA(&SchemaA{
		ByPair:  map[Pair]int32{{K: "a", V: 1}: 10},
		ByFloat: map[[2]float64]int32{{1.5, 2.5}: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := FreezeBetaSchemaB(&SchemaB{
		ByPair:  map[Pair]int32{{K: "b", V: 2}: 30},
		ByFloat: map[[2]float64]int32{{3.5, 4.5}: 40},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := sa.Root().ByPair().Get(Pair{K: "a", V: 1}); !ok || v != 10 {
		t.Fatalf("Alpha ByPair: %d %v", v, ok)
	}
	if v, ok := sb.Root().ByFloat().Get([2]float64{3.5, 4.5}); !ok || v != 40 {
		t.Fatalf("Beta ByFloat: %d %v", v, ok)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "coexist_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		typ, prefix, out string
	}{
		{"SchemaA", "Alpha", "alpha_cnf.gen.go"},
		{"SchemaB", "Beta", "beta_cnf.gen.go"},
	} {
		path, src, err := build([]string{"-type=" + spec.typ, "-prefix=" + spec.prefix, "-out=" + spec.out, dir})
		if err != nil {
			t.Fatal(err)
		}
		checkNoUnsafe(t, path, src)
		if err := os.WriteFile(path, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

// TestZeroLengthArraysAndInlineSizeCap verifies zero-length arrays in structs,
// slices, optionals, maps and Compact roots without init() size panics, and
// confirms value structs larger than maxInlineStructBytes remain table nodes.
func TestZeroLengthArraysAndInlineSizeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

type SmallVal struct {
	A int64
	B int64
}

type BigVal struct {
	Data [20]int64
}

type ZeroLeading struct {
	A [0]int64
	B uint8
}

type ZeroTrailing struct {
	B uint8
	A [0]uint8
}

type ZeroStrings struct {
	A [0]string
}

type Root struct {
	Lead   ZeroLeading
	Trail  ZeroTrailing
	Strs   ZeroStrings
	Slice  [][0]int64
	Opt    *[0]int64
	ZeroKV map[[0]int64][0]string
	Small  SmallVal
	Big    BigVal
}
`)
	testSrc := `package m

import (
	"reflect"
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

func TestZeroArraysAndInlineCap(t *testing.T) {
	snapType := reflect.TypeFor[RootSnapshot]()
	if _, hasSmall := snapType.FieldByName("nodesSmallVal"); hasSmall {
		t.Fatal("SmallVal (16 B) should be inlined")
	}
	if _, hasBig := snapType.FieldByName("nodesBigVal"); !hasBig {
		t.Fatal("BigVal (160 B > 128 B) must stay in its own node table")
	}

	var z [0]int64
	var big BigVal
	big.Data[19] = 99
	src := &Root{
		Lead:   ZeroLeading{B: 7},
		Trail:  ZeroTrailing{B: 9},
		Slice:  [][0]int64{{}, {}, {}},
		Opt:    &z,
		ZeroKV: map[[0]int64][0]string{{}: {}},
		Small:  SmallVal{A: 1, B: 2},
		Big:    big,
	}
	s, err := FreezeRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := UnmarshalRootSnapshot(data, cnfrt.VerifyUntrusted)
	if err != nil {
		t.Fatalf("VerifyUntrusted: %v", err)
	}
	v := s2.Root()
	if v.Lead().B() != 7 || v.Trail().B() != 9 || v.Strs().A().Len() != 0 || v.Strs().A().IsNil() {
		t.Fatal("zero array field mismatch")
	}
	if v.Slice().Len() != 3 || v.Slice().At(2) != ([0]int64{}) {
		t.Fatal("slice of [0]int64 mismatch")
	}
	if _, ok := v.Opt(); !ok {
		t.Fatal("optional *[0]int64 should be present")
	}
	if _, ok := v.ZeroKV().Get([0]int64{}); !ok {
		t.Fatal("map[[0]int64][0]string lookup missed")
	}
	if v.Small().A() != 1 || v.Big().Data()[19] != 99 {
		t.Fatal("SmallVal/BigVal mismatch")
	}
	out, err := s2.UnfreezeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Slice) != 3 || out.Opt == nil || out.Big.Data[19] != 99 || len(out.ZeroKV) != 1 {
		t.Fatalf("unfreeze mismatch: %+v", out)
	}

	c, err := PackRoot(src)
	if err != nil {
		t.Fatalf("PackRoot: %v", err)
	}
	if _, ok := c.Opt(); !ok {
		t.Fatal("Compact Opt *[0]int64 mismatch")
	}
	if _, err := ParseCompactRoot(c.Raw(), cnfrt.VerifyUntrusted); err != nil {
		t.Fatalf("ParseCompactRoot: %v", err)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "zero_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{"-type=Root", "-compact=Root", "-out=root_cnf.gen.go", dir})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}

func TestCompactScalarArrays(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a temporary module")
	}
	dir := tempModule(t, `package m

type UserID int64
type Flag bool

type Record struct {
	IDs    [4]UserID
	Flags  [3]Flag
	Page   [4096]byte
	BigIDs [100]UserID
	OptBig *[80]UserID
}
`)
	testSrc := `package m

import "testing"

func TestCompactScalarArrayGetters(t *testing.T) {
	var page [4096]byte
	for i := range page {
		page[i] = byte(i*31 + 7)
	}
	var big [100]UserID
	for i := range big {
		big[i] = UserID(int64(i)*1000 - 500)
	}
	var opt [80]UserID
	for i := range opt {
		opt[i] = UserID(i + 1)
	}
	src := &Record{
		IDs:    [4]UserID{10, -20, 30, 40},
		Flags:  [3]Flag{true, false, true},
		Page:   page,
		BigIDs: big,
		OptBig: &opt,
	}
	c, err := PackRecord(src)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.IDs(); got != src.IDs {
		t.Fatalf("IDs() = %v, want %v", got, src.IDs)
	}
	if got := c.Flags(); got != src.Flags {
		t.Fatalf("Flags() = %v, want %v", got, src.Flags)
	}
	if got := c.Page(); got != src.Page {
		t.Fatal("Page() mismatch")
	}
	if got := c.BigIDs(); got != src.BigIDs {
		t.Fatal("BigIDs() mismatch")
	}
	if got, ok := c.OptBig(); !ok || got != opt {
		t.Fatalf("OptBig() ok=%v mismatch", ok)
	}
	var zero CompactRecord
	if zero.IDs() != ([4]UserID{}) || zero.Page() != ([4096]byte{}) || zero.BigIDs() != ([100]UserID{}) {
		t.Fatal("zero CompactRecord returned non-zero array")
	}
	if _, ok := zero.OptBig(); ok {
		t.Fatal("zero CompactRecord OptBig reported present")
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "arr_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	path, src, err := build([]string{"-type=Record", "-compact=Record", "-out=record_cnf.gen.go", dir})
	if err != nil {
		t.Fatal(err)
	}
	checkNoUnsafe(t, path, src)
	if !strings.Contains(string(src), "for i := range out") || !strings.Contains(string(src), "copy(out[:], raw)") {
		t.Fatalf("expected loop/copy for large scalar arrays in generated code")
	}
	if len(src) > 64<<10 {
		t.Fatalf("generated code for [4096]byte and [100]UserID is too large (%d bytes)", len(src))
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	goCmd(t, dir, "vet", "./...")
	goCmd(t, dir, "test", "./...")
}
