package domain_test

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDomainImportsOnlyStdlib enforces AR-1.
//
// The rule is written as a test rather than as a convention because a
// convention is not checkable in review and this one carries the whole
// architecture: the moment the domain imports an HTTP client or a provider SDK,
// the loop stops being testable without them and the ports stop being seams.
//
// harnesssphere-domain holds the same line with two dependencies. Go's standard
// library makes zero reachable, so zero is the bar.
func TestDomainImportsOnlyStdlib(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("import dir: %v", err)
	}
	for _, imp := range append(pkg.Imports, pkg.TestImports...) {
		if isStdlib(imp) {
			continue
		}
		// The package's own test files may import the package under test.
		if strings.HasPrefix(imp, "github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain") {
			continue
		}
		t.Errorf("domain imports %q, which is outside the standard library (AR-1)", imp)
	}
}

// isStdlib reports whether an import path is in the standard library. Stdlib
// paths have no dot in their first segment -- that is the same rule the go tool
// uses to tell a module path from a stdlib one.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// TestAdaptersDoNotImportEachOther enforces AR-4.
//
// Adapters know the domain and nothing else about one another. This is what
// keeps a second provider, a second store, or a second ingress from becoming a
// refactor instead of a file.
func TestAdaptersDoNotImportEachOther(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "adapter"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Skip("no adapters yet")
	}

	const prefix = "github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/"

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		pkg, perr := build.ImportDir(path, 0)
		if perr != nil {
			return nil // not a Go package directory
		}
		self, _ := filepath.Rel(root, path)
		for _, imp := range append(pkg.Imports, pkg.TestImports...) {
			if !strings.HasPrefix(imp, prefix) {
				continue
			}
			other := strings.TrimPrefix(imp, prefix)
			if other == self || strings.HasPrefix(other, self+"/") {
				continue // a sub-package of itself is fine
			}
			t.Errorf("adapter %q imports adapter %q (AR-4)", self, other)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
