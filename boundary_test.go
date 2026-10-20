package accretion

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbidden is the import prefix this module must never use. It covers
// Continuum (github.com/hazame-hub/sybil) and its sibling modules
// (sybil-conduit, sybil-quasar) alike: Accretion is what Sybil builds on, so a
// dependency the other way round is a cycle waiting to be published.
const forbidden = "github.com/hazame-hub/sybil"

// TestNothingImportsSybil reads the import block of every Go file in the
// module, tests included, and fails on any import of Sybil. A test rather
// than a linter rule so it runs wherever `go test ./...` runs here, with
// nothing to install.
func TestNothingImportsSybil(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata and dot-directories are not part of the build.
			if path != "." && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == forbidden || strings.HasPrefix(p, forbidden+"/") || strings.HasPrefix(p, forbidden+"-") {
				t.Errorf("%s imports %s; nothing in Accretion may import Sybil", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that found nothing would pass vacuously, which is exactly the
	// failure a guard must not have.
	if checked < 2 {
		t.Fatalf("checked %d Go files; the walk is not seeing the module", checked)
	}
}
