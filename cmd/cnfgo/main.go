// Command cnfgo generates pointer-free snapshot code for Go struct types.
//
// Usage:
//
//	//go:generate cnfgo -type=Config[,Other] [flags] [dir]
//
// For each root type T it writes, into <dir>/<t>_cnf.gen.go, a snapshot
// type whose node tables and pools contain no Go pointers, functions
// FreezeT, FreezeTreeT and FreezeCyclicT, read-only View types, Unfreeze
// support, .cnf persistence (WriteTo, MarshalCNF, MapTFile, UnmarshalTSnapshot)
// and, with -compact, a 16-byte CompactT value type.
//
// Flags:
//
//	-type list      comma-separated root struct types (required unless -compact is set)
//	-compact list   comma-separated types that also get a Compact value type
//	-codec spec     register an opaque type: importpath.Type=importpath.Adapter[args];
//	                the adapter implements cnfrt.Codec for the type and T in args
//	                stands for the target type (repeatable)
//	-tree           make FreezeT treat graphs as trees (no pointer deduplication)
//	-prefix name    prefix for every generated exported identifier
//	-out file       output file name (default <first type>_cnf.gen.go)
//	-tags list      build tags used when loading the package
//
// Generated files never import package unsafe.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func main() {
	log := func(err error) {
		fmt.Fprintf(os.Stderr, "cnfgo: %v\n", err)
		os.Exit(1)
	}
	if err := run(os.Args[1:]); err != nil {
		log(err)
	}
}

func run(args []string) error {
	path, src, err := build(args)
	if err != nil {
		return err
	}
	return os.WriteFile(path, src, 0o644)
}

// build parses the command line, analyzes the package and returns the
// output path and generated source.
func build(args []string) (string, []byte, error) {
	fs := flag.NewFlagSet("cnfgo", flag.ContinueOnError)
	typeList := fs.String("type", "", "comma-separated root struct types")
	compactList := fs.String("compact", "", "comma-separated types that also get a Compact value type")
	var codecs listFlag
	fs.Var(&codecs, "codec", "importpath.Type=importpath.Adapter[args] (repeatable)")
	tree := fs.Bool("tree", false, "freeze graphs as trees by default")
	prefix := fs.String("prefix", "", "prefix for generated exported identifiers")
	out := fs.String("out", "", "output file name")
	tags := fs.String("tags", "", "build tags used when loading the package")
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	o := options{
		types:   splitList(*typeList),
		compact: splitList(*compactList),
		codecs:  codecs,
		tree:    *tree,
		prefix:  *prefix,
	}
	if len(o.types) == 0 && len(o.compact) == 0 {
		fs.Usage()
		return "", nil, fmt.Errorf("-type is required")
	}
	if o.prefix != "" && sanitizeIdent(o.prefix) != o.prefix {
		return "", nil, fmt.Errorf("invalid -prefix %q", o.prefix)
	}
	dir := "."
	switch fs.NArg() {
	case 0:
	case 1:
		dir = fs.Arg(0)
	default:
		return "", nil, fmt.Errorf("at most one package directory may be given")
	}
	name := *out
	if name == "" {
		first := o.types
		if len(first) == 0 {
			first = o.compact
		}
		name = strings.ToLower(first[0]) + "_cnf.gen.go"
	}
	if filepath.Base(name) != name {
		return "", nil, fmt.Errorf("-out must be a file name in the package directory, not %q", name)
	}
	lp, err := loadPackage(dir, *tags, name)
	if err != nil {
		return "", nil, err
	}
	sc, err := analyze(lp, o)
	if err != nil {
		return "", nil, err
	}
	// The header records the flags but not the package directory, so the
	// output does not depend on where the generator was invoked from.
	src, err := generate(sc, "cnfgo "+strings.Join(args[:len(args)-fs.NArg()], " "))
	if err != nil {
		return "", nil, err
	}
	if len(sc.errs) > 0 {
		return "", nil, sc.failure()
	}
	return filepath.Join(lp.dir, name), src, nil
}
