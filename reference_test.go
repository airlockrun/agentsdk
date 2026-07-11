package agentsdk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readCapBytes mirrors sol's read-tool output cap (50 KiB). Every reference doc must
// fit in a single read so an agent that ignores the "read more with offset"
// footer still receives the whole file.
const readCapBytes = 50 * 1024

// TestReferenceDocsFitReadCapAndAreReferenced guards the REFERENCE.md split: the root
// reference and every companion under reference/ must each fit the read cap, every
// companion must be pointed at from REFERENCE.md (or an agent reading only the root
// never discovers it), and REFERENCE.md must not point at a companion that doesn't
// exist. Any of these drifting fails the build.
func TestReferenceDocsFitReadCapAndAreReferenced(t *testing.T) {
	root, err := os.ReadFile("REFERENCE.md")
	if err != nil {
		t.Fatal(err)
	}
	rootStr := string(root)

	if len(root) > readCapBytes {
		t.Errorf("REFERENCE.md is %d bytes, over the %d read cap — move a section into reference/", len(root), readCapBytes)
	}

	entries, err := os.ReadDir("reference")
	if err != nil {
		t.Fatalf("reference/ companion dir: %v", err)
	}

	var found int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		found++
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > readCapBytes {
			t.Errorf("reference/%s is %d bytes, over the %d read cap", e.Name(), info.Size(), readCapBytes)
		}
		ref := "/libs/agentsdk/reference/" + e.Name()
		if !strings.Contains(rootStr, ref) {
			t.Errorf("reference/%s exists but REFERENCE.md has no pointer to %s — an agent reading only REFERENCE.md will never find it", e.Name(), ref)
		}
	}
	if found == 0 {
		t.Fatal("no companion docs found under reference/ — expected the deep-dive files")
	}

	// No dangling pointers: every /libs/agentsdk/reference/<name>.md the root names
	// must resolve to a real file.
	re := regexp.MustCompile(`/libs/agentsdk/reference/([a-z0-9-]+\.md)`)
	for _, m := range re.FindAllStringSubmatch(rootStr, -1) {
		if _, err := os.Stat(filepath.Join("reference", m[1])); err != nil {
			t.Errorf("REFERENCE.md references /libs/agentsdk/reference/%s but no such file exists under reference/", m[1])
		}
	}
}

// readReferenceDocs returns the concatenated text of REFERENCE.md and every reference/*.md.
func readReferenceDocs(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	root, err := os.ReadFile("REFERENCE.md")
	if err != nil {
		t.Fatal(err)
	}
	b.Write(root)
	entries, err := os.ReadDir("reference")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		c, err := os.ReadFile(filepath.Join("reference", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteByte('\n')
		b.Write(c)
	}
	return b.String()
}

// agentsdkAPI parses the package's own .go sources and returns the set of
// exported package-level identifiers (any `agentsdk.X` must be one of these —
// Go only lets you reach exported funcs/types/vars/consts through the package
// qualifier) and the list of exported Register* methods on *Agent.
func agentsdkAPI(t *testing.T) (symbols map[string]bool, registerMethods []string) {
	t.Helper()
	symbols = map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Parse regardless of build tags so build-constrained files still
		// contribute symbols (a superset is correct for an existence check).
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					if d.Name.IsExported() {
						symbols[d.Name.Name] = true
					}
					continue
				}
				if recvIsAgent(d.Recv) && strings.HasPrefix(d.Name.Name, "Register") && d.Name.IsExported() {
					registerMethods = append(registerMethods, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							symbols[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								symbols[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return symbols, registerMethods
}

// recvIsAgent reports whether a method receiver is Agent or *Agent.
func recvIsAgent(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) != 1 {
		return false
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == "Agent"
}

// TestReferenceDocsReferenceRealSymbols asserts every `agentsdk.X` mentioned in the
// docs is a real exported package-level identifier. This catches a doc that
// keeps naming a symbol after it's renamed or removed. (It checks existence,
// not signatures — the snippets are illustrative and not compilable.)
func TestReferenceDocsReferenceRealSymbols(t *testing.T) {
	symbols, _ := agentsdkAPI(t)
	docs := readReferenceDocs(t)

	re := regexp.MustCompile(`agentsdk\.([A-Z][A-Za-z0-9_]*)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(docs, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !symbols[name] {
			t.Errorf("docs reference agentsdk.%s, which is not an exported package-level identifier", name)
		}
	}
}

// TestReferenceDocsCoverRegisterAPIs asserts every Register* method on *Agent is
// referenced somewhere in the docs. The Register* surface is the capability
// contract the reference must cover; a new RegisterFoo with no docs fails here.
// (Full API coverage is intentionally not enforced — most exported symbols are
// curated out of the agent-facing reference.)
func TestReferenceDocsCoverRegisterAPIs(t *testing.T) {
	_, registerMethods := agentsdkAPI(t)
	if len(registerMethods) == 0 {
		t.Fatal("found no Register* methods on *Agent — parser change?")
	}
	docs := readReferenceDocs(t)
	for _, m := range registerMethods {
		if !strings.Contains(docs, m) {
			t.Errorf("Register API %q is not documented in REFERENCE.md or any reference/*.md companion", m)
		}
	}
}
