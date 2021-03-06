/*
Copyright (c) 2013-2016 the Godepq Authors

Use of this source code is governed by a MIT-style
license that can be found in the LICENSE file or at
https://opensource.org/licenses/MIT.
*/

package deps

import (
	"bytes"
	"errors"
	"fmt"
	"go/build"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Dependencies struct {
	// Map of package -> dependencies.
	Forward Graph
	// Packages which were ignored.
	Ignored Set
	Info    map[Package]*DependencyInfo
}

type DependencyInfo struct {
	LOC int
	// TODO: Add recursive LOC (but don't double count packages)
}

type Condition func(Dependencies) bool

// Resolve resolves import paths to a canonical, absolute form.
// Relative paths are resolved relative to basePath.
// It does not verify that the import is valid.
func Resolve(importPath, basePath string, bctx build.Context) (Package, error) {
	pkg, err := bctx.Import(importPath, basePath, build.FindOnly)
	if err != nil {
		return "", fmt.Errorf("unable to resolve %q: %v", importPath, err)
	}
	return stripVendor(pkg.ImportPath), nil
}

type Builder struct {
	// The base directory for relative imports.
	BaseDir string
	// The roots of the dependency graph (source packages).
	Roots []Package
	// Stop building the graph if ANY conditions are met.
	TerminationConditions []Condition
	// Ignore any packages that match any of these patterns.
	// Tested on the resolved package path.
	Ignored []*regexp.Regexp
	// Include only packages that match any of these patterns.
	// Tested on the resolved package path.
	Included []*regexp.Regexp
	// Whether tests should be included in the dependencies.
	IncludeTests bool
	// Whether to include standard library packages
	IncludeStdlib bool
	// The build context for processing imports.
	BuildContext build.Context

	// Internal
	deps Dependencies
}

func (b *Builder) Build() (Dependencies, error) {
	b.deps = Dependencies{
		Forward: NewGraph(),
		Ignored: NewSet(),
		Info:    make(map[Package]*DependencyInfo),
	}

	err := b.addAllPackages(b.Roots)
	if err == termination {
		err = nil // Ignore termination condition.
	}

	return b.deps, err
}

func (b *Builder) addAllPackages(pkgs []Package) error {
	for _, pkg := range pkgs {
		// TODO: add support for recursive sub-packages.
		includedName, err := b.addPackage(pkg)
		if err != nil {
			return err
		}
		if includedName == "" {
			fmt.Fprintf(os.Stderr, "Warning: ignoring root package %q\n", pkg)
		}
	}
	return nil
}

var termination = errors.New("termination condition met")

// Recursively adds a package to the accumulated dependency graph.
// If the package is not included, includedName will be empty.
func (b *Builder) addPackage(pkgName Package) (includedName Package, err error) {
	// Ignore cgo imports
	if pkgName == "C" {
		return "", nil
	}

	pkg, err := b.BuildContext.Import(string(pkgName), b.BaseDir, 0)
	if err != nil {
		return "", err
	}

	pkgFullName := stripVendor(pkg.ImportPath)
	if !b.isAccepted(pkg) {
		b.deps.Ignored.Insert(pkgFullName)
		return "", nil
	}

	if b.deps.Forward.Has(pkgFullName) {
		// Package was included, but we don't need to walk its deps again.
		return pkgFullName, nil
	}

	// Insert the package.
	b.deps.Forward.Pkg(pkgFullName)

	b.deps.Info[pkgFullName] = &DependencyInfo{
		LOC: b.linesOfCode(pkg),
	}

	for _, condition := range b.TerminationConditions {
		if condition(b.deps) {
			return pkgFullName, termination
		}
	}

	for _, imp := range b.getImports(pkg) {
		includedName, err := b.addPackage(imp)
		if err != nil {
			return pkgFullName, err
		}
		if includedName == "" {
			// Package was not included, skip it.
			continue
		}

		b.deps.Forward.Pkg(pkgFullName).Insert(includedName)
	}

	return pkgFullName, nil
}

func (b *Builder) getImports(pkg *build.Package) []Package {
	allImports := pkg.Imports
	if b.IncludeTests {
		allImports = append(allImports, pkg.TestImports...)
		allImports = append(allImports, pkg.XTestImports...)
	}
	var imports []Package
	found := make(map[string]struct{})
	for _, imp := range allImports {
		if imp == pkg.ImportPath {
			// Don't draw a self-reference when foo_test depends on foo.
			continue
		}
		if _, ok := found[imp]; ok {
			continue
		}
		found[imp] = struct{}{}
		imports = append(imports, Package(imp))
	}
	return imports
}

func (b *Builder) isIgnored(pkg Package) bool {
	for _, r := range b.Ignored {
		if r.MatchString(string(pkg)) {
			return true
		}
	}
	return false
}

func (b *Builder) isIncluded(pkg Package) bool {
	if len(b.Included) == 0 {
		return true
	}
	for _, r := range b.Included {
		if r.MatchString(string(pkg)) {
			return true
		}
	}
	return false
}

// Detects if package name matches search criterias
func (b *Builder) isAccepted(pkg *build.Package) bool {
	pkgFullName := stripVendor(pkg.ImportPath)
	if b.isIgnored(pkgFullName) {
		return false
	}
	if pkg.Goroot && !b.IncludeStdlib {
		return false
	}
	return b.isIncluded(pkgFullName)
}

func StripVendor(pkg Package) (Package, bool) {
	stripped := stripVendor(string(pkg))
	return stripped, stripped != pkg
}

func stripVendor(pkg string) Package {
	const vendor = string(os.PathSeparator) + "vendor" + string(os.PathSeparator)
	if index := strings.LastIndex(string(pkg), vendor); index != -1 {
		return Package(pkg[index+len(vendor):])
	}
	return Package(pkg)
}

func (b *Builder) linesOfCode(pkg *build.Package) int {
	loc := 0
	files := append([]string{}, pkg.GoFiles...)
	// TODO: Should we also include the c source files?
	files = append(files, pkg.CgoFiles...)
	if b.IncludeTests {
		files = append(files, pkg.TestGoFiles...)
		files = append(files, pkg.XTestGoFiles...)
	}
	for _, f := range files {
		l, err := countLines(filepath.Join(pkg.Dir, f))
		if err != nil {
			log.Printf("ERROR: %v", err)
			continue
		}
		loc += l
	}
	return loc
}

func countLines(file string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, 32*1024)
	count := 0
	lineSep := []byte{'\n'}

	for {
		c, err := f.Read(buf)
		count += bytes.Count(buf[:c], lineSep)

		switch {
		case err == io.EOF:
			return count, nil

		case err != nil:
			return count, err
		}
	}
}
