package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// listedPackage is the subset of `go list -json` output used by the loader.
type listedPackage struct {
	ImportPath string
	Dir        string
	Name       string
	GoFiles    []string
	Export     string
	ImportMap  map[string]string
	Error      *struct{ Err string }
	DepOnly    bool
}

// loadedPackage is a type-checked target package.
type loadedPackage struct {
	fset     *token.FileSet
	pkg      *types.Package
	files    []*ast.File
	dir      string
	pkgNames map[string]string // import path -> declared Go package name
	// typeErrs are tolerated type errors in the target package (typically
	// references to declarations that have not been generated yet). They
	// are reported only if analysis of the requested types fails.
	typeErrs []error
}

// loadPackage type-checks the package in dir. Dependencies are imported
// from compiler export data located with `go list -export`, so no
// third-party loader is needed. Files named skipFile (the generator's own
// previous output) are ignored, and type errors in the target package are
// tolerated as long as the requested types resolve, because user code may
// already reference not-yet-generated declarations.
func loadPackage(dir string, tags string, skipFile string) (*loadedPackage, error) {
	args := []string{"list", "-e", "-json=ImportPath,Dir,Name,GoFiles,Export,ImportMap,Error,DepOnly", "-export", "-deps"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, stderr.String())
	}
	exports := map[string]string{}
	pkgNames := map[string]string{}
	var target *listedPackage
	dec := json.NewDecoder(&stdout)
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list output: %v", err)
		}
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
		if p.ImportPath != "" && p.Name != "" {
			pkgNames[p.ImportPath] = p.Name
		}
		if !p.DepOnly {
			cp := p
			target = &cp
		}
	}
	if target == nil {
		return nil, fmt.Errorf("no package found in %s", dir)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range target.GoFiles {
		if name == skipFile {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(target.Dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("package in %s has no Go files", dir)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		if mapped, ok := target.ImportMap[path]; ok {
			path = mapped
		}
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(file)
	}
	var typeErrs []error
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "gc", lookup),
		Error:    func(err error) { typeErrs = append(typeErrs, err) },
	}
	pkg, _ := conf.Check(target.ImportPath, fset, files, nil)
	if pkg == nil {
		return nil, errors.Join(typeErrs...)
	}
	return &loadedPackage{
		fset:     fset,
		pkg:      pkg,
		files:    files,
		dir:      target.Dir,
		pkgNames: pkgNames,
		typeErrs: typeErrs,
	}, nil
}

// resolvePkgName returns the Go package name of importPath, consulting the
// dependency table first, then `go list`, and finally deriving a valid
// identifier from the path (skipping major-version suffixes like /v2).
func (lp *loadedPackage) resolvePkgName(importPath string) string {
	if lp != nil {
		if name, ok := lp.pkgNames[importPath]; ok {
			return name
		}
		if lp.dir != "" {
			cmd := exec.Command("go", "list", "-e", "-f", "{{.Name}}", importPath)
			cmd.Dir = lp.dir
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.Output(); err == nil {
				if name := strings.TrimSpace(string(out)); name != "" {
					if lp.pkgNames == nil {
						lp.pkgNames = map[string]string{}
					}
					lp.pkgNames[importPath] = name
					return name
				}
			}
		}
	}
	segs := strings.Split(importPath, "/")
	last := segs[len(segs)-1]
	if isVersionSegment(last) && len(segs) >= 2 {
		last = segs[len(segs)-2]
	}
	return sanitizeIdent(last)
}

func isVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// typeDoc returns the doc comment lines attached to the declaration of the
// named type in the package's files.
func (lp *loadedPackage) typeDoc(name string) []string {
	for _, f := range lp.files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if ts.Name.Name != name {
					continue
				}
				var lines []string
				for _, cg := range []*ast.CommentGroup{gd.Doc, ts.Doc, ts.Comment} {
					if cg == nil {
						continue
					}
					for _, c := range cg.List {
						lines = append(lines, c.Text)
					}
				}
				return lines
			}
		}
	}
	return nil
}
