package main

import (
	"errors"
	"fmt"
	"go/types"
	"strings"

	"github.com/Voskan/cnfgo/cnfrt"
)

// schemaVersion is mixed into every schema hash; it changes whenever the
// generated representation changes incompatibly.
const schemaVersion = "cnfgo/schema/v3"

// options are the generator settings taken from the command line.
type options struct {
	types   []string
	compact []string
	codecs  []string
	tree    bool
	prefix  string
}

// builtinCodecs are codecs registered without a -codec flag. Their targets
// are only matched by name, so no dependency on the target module is added.
var builtinCodecs = []string{
	"k8s.io/apimachinery/pkg/api/resource.Quantity=github.com/Voskan/cnfgo/cnfrt/codecs/k8s.Quantity[T,*T]",
}

// parseCodec parses a -codec value of the form
//
//	importpath.Type=importpath.Adapter[args]
//
// where args, if present, is a comma-separated type argument list in which
// T stands for the target type.
func parseCodec(spec string) (*codec, error) {
	target, adapter, ok := strings.Cut(spec, "=")
	if !ok || target == "" || adapter == "" {
		return nil, fmt.Errorf("invalid -codec %q: want importpath.Type=importpath.Adapter", spec)
	}
	c := &codec{target: strings.TrimSpace(target)}
	adapter = strings.TrimSpace(adapter)
	if i := strings.IndexByte(adapter, '['); i >= 0 {
		if !strings.HasSuffix(adapter, "]") {
			return nil, fmt.Errorf("invalid -codec adapter %q", adapter)
		}
		c.typeArgs = strings.Split(adapter[i+1:len(adapter)-1], ",")
		adapter = adapter[:i]
	}
	dot := strings.LastIndexByte(adapter, '.')
	if dot <= 0 || strings.LastIndexByte(adapter, '/') > dot {
		return nil, fmt.Errorf("invalid -codec adapter %q: want importpath.Name", adapter)
	}
	c.adapterPath, c.adapterName = adapter[:dot], adapter[dot+1:]
	tdot := strings.LastIndexByte(c.target, '.')
	if tdot <= 0 {
		return nil, fmt.Errorf("invalid -codec target %q: want importpath.Type", c.target)
	}
	c.stem = sanitize(c.target[strings.LastIndexByte(c.target[:tdot], '/')+1:])
	return c, nil
}

// adapterExpr returns the Go expression of the adapter type of c for a
// target type whose expression is target.
func (sc *schema) adapterExpr(c *codec, target string) string {
	expr := c.adapterName
	if c.adapterPath != sc.pkg.Path() {
		pkgName := sanitizeIdent(sc.lp.resolvePkgName(c.adapterPath))
		expr = sc.imports.add(c.adapterPath, pkgName) + "." + c.adapterName
	}
	if len(c.typeArgs) > 0 {
		args := make([]string, len(c.typeArgs))
		for i, a := range c.typeArgs {
			a = strings.TrimSpace(a)
			switch {
			case a == "T":
				a = target
			case a == "*T":
				a = "*" + target
			}
			args[i] = a
		}
		expr += "[" + strings.Join(args, ", ") + "]"
	}
	return expr
}

func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || (b.Len() > 0 && '0' <= r && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "codec"
	}
	return b.String()
}

// analyze builds the schema for the requested root types.
func analyze(lp *loadedPackage, o options) (*schema, error) {
	sc := &schema{
		pkg:            lp.pkg,
		prefix:         o.prefix,
		byKey:          map[string]*shape{},
		nodeByKey:      map[string]*node{},
		poolByKey:      map[string]*pool{},
		ifaceByKey:     map[string]*iface{},
		codecReg:       map[string]*codec{},
		imports:        newImportSet(lp.pkg),
		lp:             lp,
		treeDefault:    o.tree,
		usedNames:      map[string]bool{},
		usedCanonNames: map[string]bool{},
		usedNodeNames:  map[string]bool{},
	}
	for _, spec := range append(append([]string(nil), builtinCodecs...), o.codecs...) {
		c, err := parseCodec(spec)
		if err != nil {
			return nil, err
		}
		sc.codecReg[c.target] = c
	}
	lookup := func(name string) *node {
		obj, ok := lp.pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			sc.errorf("type %s not found in package %s", name, lp.pkg.Path())
			return nil
		}
		t := types.Unalias(obj.Type())
		if _, isStruct := t.Underlying().(*types.Struct); !isStruct {
			sc.errorf("type %s is not a struct type", name)
			return nil
		}
		if named, ok := t.(*types.Named); ok && named.TypeParams() != nil {
			sc.errorf("type %s is generic; instantiate it in a named type", name)
			return nil
		}
		return sc.nodeOf(t, name)
	}
	isRoot := map[*node]bool{}
	for _, name := range append(append([]string(nil), o.types...), o.compact...) {
		if n := lookup(name); n != nil && !isRoot[n] {
			isRoot[n] = true
			n.isRoot = true
			sc.roots = append(sc.roots, n)
		}
	}
	for _, name := range o.compact {
		if n := lookup(name); n != nil {
			n.isRoot = true
			sc.compacts = append(sc.compacts, n)
		}
	}
	sc.drain()
	sc.checkCompactNames()
	if len(sc.roots) == 0 && len(sc.errs) == 0 {
		sc.errorf("no root types given")
	}
	if len(sc.nodes) > 1<<16 {
		sc.errorf("too many node types (%d)", len(sc.nodes))
	}
	if len(sc.errs) > 0 {
		return nil, sc.failure()
	}
	sc.finalize()
	sc.hash = schemaHash(sc)
	return sc, nil
}

// failure combines the analysis errors with tolerated type errors, which are
// often their cause.
func (sc *schema) failure() error {
	var errs []error
	for _, e := range sc.errs {
		errs = append(errs, errors.New(e))
	}
	for i, e := range sc.lp.typeErrs {
		if i == 10 {
			errs = append(errs, fmt.Errorf("(%d more type errors)", len(sc.lp.typeErrs)-i))
			break
		}
		errs = append(errs, fmt.Errorf("type error: %v", e))
	}
	return errors.Join(errs...)
}

// localQualifier returns a function that removes the qualifier of the
// schema's own package from type strings, keeping the import paths of all
// other packages. It only matches the path at an identifier boundary, so a
// package whose path merely ends with the schema package's path is kept.
func (sc *schema) localQualifier() func(string) string {
	path := sc.pkg.Path() + "."
	isPathByte := func(c byte) bool {
		return c == '.' || c == '/' || c == '-' || c == '_' || c == '~' || c == '+' ||
			'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c >= 0x80
	}
	return func(s string) string {
		var b strings.Builder
		for {
			i := strings.Index(s, path)
			for i > 0 && isPathByte(s[i-1]) {
				j := strings.Index(s[i+1:], path)
				if j < 0 {
					i = -1
					break
				}
				i += 1 + j
			}
			if i < 0 {
				b.WriteString(s)
				return b.String()
			}
			b.WriteString(s[:i])
			s = s[i+len(path):]
		}
	}
}

// schemaHash returns a hash of the canonical description of the flat
// representation: table and pool order, every flat struct with its field
// types and offsets, the shape of every pool element and the adapter of
// every codec. Types declared in the schema's own package are named without
// their import path, so moving the package (or a module major version
// bump) keeps the hash; types of other packages keep their full path. Two
// schemas with the same hash read each other's images.
func schemaHash(sc *schema) uint64 {
	local := sc.localQualifier()
	canonFlat := make(map[string]string, len(sc.flats))
	for _, fs := range sc.flats {
		canonFlat[fs.name] = fs.canonName
	}
	flatName := func(expr string) string {
		if c, ok := canonFlat[expr]; ok {
			return c
		}
		return expr
	}
	var b strings.Builder
	b.WriteString(schemaVersion)
	b.WriteByte('\n')
	for _, n := range sc.tableNodes() {
		fmt.Fprintf(&b, "table %d %s\n", n.tbl, flatName(n.flatName))
	}
	for _, n := range sc.inlineNodes() {
		fmt.Fprintf(&b, "inline %d %s\n", n.inlineID, flatName(n.flatName))
	}
	for _, r := range sc.roots {
		fmt.Fprintf(&b, "root %d\n", r.tbl)
	}
	for _, p := range sc.pools {
		fmt.Fprintf(&b, "pool %d %s %s %d/%d\n", p.idx, local(p.key), flatName(p.flatExpr), p.size, p.align)
	}
	for _, fs := range sc.flats {
		fmt.Fprintf(&b, "flat %s %d/%d {", flatName(fs.name), fs.size, fs.align)
		for _, f := range fs.fields {
			fmt.Fprintf(&b, " %s:%s@%d", f.name, flatName(f.expr), f.off)
		}
		b.WriteString(" }\n")
	}
	for _, n := range sc.nodes {
		fmt.Fprintf(&b, "node %s {", n.name)
		for _, f := range n.fields {
			fmt.Fprintf(&b, " %s:%s", f.flatName, local(f.sh.key))
		}
		b.WriteString(" }\n")
	}
	// A codec payload is opaque to the schema, so the identity of the
	// adapter that wrote it is part of the schema: images written with a
	// different adapter for the same type are rejected.
	for _, sh := range sc.shapes {
		if sh.kind == kCodec {
			c := sh.codec
			args := make([]string, len(c.typeArgs))
			for i, a := range c.typeArgs {
				args[i] = strings.TrimSpace(a)
			}
			fmt.Fprintf(&b, "codec %s target=%s adapter=%s[%s]\n", local(sh.key), local(c.target), local(c.adapterPath+"."+c.adapterName), strings.Join(args, ","))
		}
	}
	for _, ifc := range sc.ifaces {
		fmt.Fprintf(&b, "iface %s {", ifc.name)
		for _, im := range ifc.impls {
			fmt.Fprintf(&b, " %d:%v", im.node.tbl, im.ptr)
		}
		b.WriteString(" }\n")
	}
	h := cnfrt.HashString(b.String(), 0)
	if h == 0 {
		h = 1
	}
	return h
}
