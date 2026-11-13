// Package arch enforces architectural boundaries, Sans-I/O & The Elm Architecture
// (TEA) formal-verification constraints, Phantom Types, Refinement Types (篩型),
// Single Source of Truth (SSOT) state normalization, and strict identifier
// naming / export visibility conventions across the antic repository.
package arch

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

const modulePrefix = "github.com/RNG999/antic"

// Pure stdlib packages permitted inside internal/tea/... packages.
// Any package capable of I/O, OS interaction, networking, randomness,
// concurrency, reflection, or unsafe memory manipulation is excluded.
var allowedPureStdlib = map[string]bool{
	"cmp":       true,
	"errors":    true,
	"fmt":       true,
	"math":      true,
	"math/bits": true,
	"net/netip": true,
	"path":      true,
	"slices":    true,
	"strconv":   true,
	"strings":   true,
	"time":      true, // types only; time.Now/Since/Until/Sleep are banned by AST check
}

// Host architectural layers (strictly ordered: lower rank cannot import higher rank).
//
//	Rank -2 (pure types):  internal/types
//	Rank -1 (pure domain): internal/proto, internal/domain
//	Rank 0  (pure TEA):    internal/tea/*
//	Rank 0  (pure spec):   internal/spec (top-level specification; test oracle only)
//	Rank 1  (host domain): internal/host/config, internal/host/share
//	Rank 2  (host infra):  internal/host/chv, internal/host/systemd, internal/host/transport,
//	                       internal/host/egress, internal/host/instance, internal/host/nixbuild
//	Rank 3  (orchestrator): internal/host/guest
//	Rank 4  (cli):          internal/host/cli
func packageLayerRank(relPkg string) (domain string, rank int, ok bool) {
	switch {
	case relPkg == "internal/types":
		return "pure", -2, true
	case relPkg == "internal/proto":
		return "pure", -2, true
	case relPkg == "internal/domain":
		return "pure", -1, true
	case strings.HasPrefix(relPkg, "internal/tea/"):
		return "pure", 0, true
	case relPkg == "internal/spec":
		return "pure", 0, true
	case relPkg == "internal/spec/refine":
		// Test-only: checks that internal/tea/operation refines internal/spec.
		return "pure", 0, true
	case relPkg == "internal/host/config" || relPkg == "internal/host/share":
		return "host", 1, true
	case relPkg == "internal/host/chv",
		relPkg == "internal/host/systemd",
		relPkg == "internal/host/transport",
		relPkg == "internal/host/egress",
		relPkg == "internal/host/instance",
		relPkg == "internal/host/nixbuild":
		return "host", 2, true
	case relPkg == "internal/host/guest":
		return "host", 3, true
	case relPkg == "internal/host/cli":
		return "host", 4, true
	case relPkg == "internal/guest/session":
		return "guest", 1, true
	case relPkg == "internal/guest/agent":
		return "guest", 2, true
	case relPkg == "internal/arch":
		return "arch", 99, true
	default:
		return "", 0, false
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "../.."))
}

// TestLayerDependencies verifies that all internal packages obey the layered
// dependency DAG and that host/guest/pure boundaries are never crossed.
func TestLayerDependencies(t *testing.T) {
	root := repoRoot(t)
	pkgs := discoverPackages(t, root, "internal")

	for _, relPkg := range pkgs {
		dom, rank, ok := packageLayerRank(relPkg)
		if !ok {
			t.Errorf("unclassified package %q: register its architectural layer in internal/arch/arch_test.go", relPkg)
			continue
		}
		if dom == "arch" {
			continue
		}

		fset := token.NewFileSet()
		files := parseNonTestFiles(t, fset, filepath.Join(root, relPkg))
		for _, f := range files {
			for _, imp := range f.Imports {
				impPath := strings.Trim(imp.Path.Value, `"`)
				if impPath == modulePrefix {
					// Root package embeds guest flake; only nixbuild may import it.
					if relPkg != "internal/host/nixbuild" {
						t.Errorf("%s: only internal/host/nixbuild may import root package %s", relPkg, modulePrefix)
					}
					continue
				}
				if !strings.HasPrefix(impPath, modulePrefix+"/") {
					continue
				}
				targetRel := strings.TrimPrefix(impPath, modulePrefix+"/")
				// The specification is written independently of the implementation
				// so that it can be its oracle: it may not import internal/tea, and
				// no implementation package may import it (only tests do).
				if relPkg == "internal/spec" && strings.HasPrefix(targetRel, "internal/tea/") {
					t.Errorf("independence violation: internal/spec imports implementation model %s", targetRel)
					continue
				}
				if targetRel == "internal/spec" {
					t.Errorf("independence violation: %s imports internal/spec (only tests may use the specification)", relPkg)
					continue
				}
				targetDom, targetRank, ok := packageLayerRank(targetRel)
				if !ok {
					t.Errorf("%s imports unclassified package %s", relPkg, targetRel)
					continue
				}

				if dom == "pure" && targetDom != "pure" {
					t.Errorf("layer violation: pure package %s imports non-pure package %s", relPkg, targetRel)
					continue
				}
				if dom == "host" && targetDom == "guest" {
					t.Errorf("boundary violation: host package %s imports guest package %s", relPkg, targetRel)
					continue
				}
				if dom == "guest" && targetDom == "host" {
					t.Errorf("boundary violation: guest package %s imports host package %s", relPkg, targetRel)
					continue
				}
				if targetRank > rank {
					t.Errorf("layer violation: %s (rank %d) imports higher-layer %s (rank %d)",
						relPkg, rank, targetRel, targetRank)
				}
			}
		}
	}
}

// TestTEAPurityAndFormalConstraints enforces Sans-I/O, The Elm Architecture (TEA),
// and formal-verification (IronFleet / Verdi / TLA+) constraints on every package
// under internal/tea/.
func TestTEAPurityAndFormalConstraints(t *testing.T) {
	root := repoRoot(t)
	teaRoot := filepath.Join(root, "internal/tea")
	if _, err := os.Stat(teaRoot); err != nil {
		t.Fatalf("expected internal/tea directory to exist: %v", err)
	}
	teaPkgs := discoverPackages(t, root, "internal/tea")
	if len(teaPkgs) == 0 {
		t.Fatal("expected at least one TEA package under internal/tea/")
	}

	t.Run("internal/types", func(t *testing.T) {
		checkTypesPackage(t, root)
	})
	t.Run("internal/domain", func(t *testing.T) {
		checkDomainPackage(t, root)
	})
	t.Run("internal/spec", func(t *testing.T) {
		checkSpecPackage(t, root)
	})

	for _, relPkg := range teaPkgs {
		t.Run(relPkg, func(t *testing.T) {
			checkTEAPackage(t, root, relPkg)
		})
	}
}

func checkPureFiles(t *testing.T, root, relPkg string) (*token.FileSet, []*ast.File, *types.Package, *types.Info) {
	t.Helper()
	pkgDir := filepath.Join(root, relPkg)
	fset := token.NewFileSet()
	files := parseNonTestFiles(t, fset, pkgDir)
	if len(files) == 0 {
		t.Fatalf("%s: no non-test Go files found", relPkg)
	}

	// 1. Check imports against the pure Sans-I/O whitelist.
	for _, f := range files {
		for _, imp := range f.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(impPath, modulePrefix+"/") {
				targetRel := strings.TrimPrefix(impPath, modulePrefix+"/")
				if relPkg == "internal/types" {
					t.Errorf("%s: internal/types is the root type-combinator package and may not import any internal package (%q)",
						posStr(fset, imp.Pos()), impPath)
					continue
				}
				if targetRel != "internal/proto" && targetRel != "internal/types" && targetRel != "internal/domain" && !strings.HasPrefix(targetRel, "internal/tea/") {
					t.Errorf("%s: forbidden internal import %q in pure package %s", posStr(fset, imp.Pos()), impPath, relPkg)
				}
				continue
			}
			if !allowedPureStdlib[impPath] {
				t.Errorf("%s: forbidden import %q in pure package %s (allowed stdlib: %v)",
					posStr(fset, imp.Pos()), impPath, relPkg, allowedKeys(allowedPureStdlib))
			}
		}
	}

	// 2. Type-check the package.
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	conf := types.Config{Importer: newRepoImporter(t, root)}
	typedPkg, err := conf.Check(modulePrefix+"/"+relPkg, fset, files, info)
	if err != nil {
		t.Fatalf("%s: typecheck failed: %v", relPkg, err)
	}

	// 3. AST & type-based Sans-I/O and Formal Methods checks:
	for _, f := range files {
		baseName := filepath.Base(fset.Position(f.Pos()).Filename)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok == token.VAR {
					t.Errorf("%s: package-level 'var' declaration is forbidden in pure package %s",
						posStr(fset, d.Pos()), relPkg)
				}
			case *ast.FuncDecl:
				if d.Name.Name == "init" && d.Recv == nil {
					t.Errorf("%s: init() function is forbidden in pure package %s", posStr(fset, d.Pos()), relPkg)
				}
				if d.Recv != nil {
					for _, field := range d.Recv.List {
						if _, isPtr := field.Type.(*ast.StarExpr); isPtr {
							t.Errorf("%s: pointer receiver on %s is forbidden in pure package %s (use value receivers)",
								posStr(fset, field.Pos()), d.Name.Name, relPkg)
						}
					}
				}
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			switch stmt := n.(type) {
			case *ast.ForStmt:
				t.Errorf("%s: general 'for' loop is forbidden in pure package %s (use bounded 'for ... range' over a slice to guarantee termination)",
					posStr(fset, stmt.Pos()), relPkg)
			case *ast.GoStmt:
				t.Errorf("%s: 'go' statement is forbidden in pure package %s", posStr(fset, stmt.Pos()), relPkg)
			case *ast.ChanType:
				t.Errorf("%s: 'chan' type is forbidden in pure package %s", posStr(fset, stmt.Pos()), relPkg)
			case *ast.SelectStmt:
				t.Errorf("%s: 'select' statement is forbidden in pure package %s", posStr(fset, stmt.Pos()), relPkg)
			case *ast.RangeStmt:
				if tv, ok := info.Types[stmt.X]; ok {
					if _, isMap := tv.Type.Underlying().(*types.Map); isMap {
						t.Errorf("%s: ranging over map (%s) is forbidden in pure package %s",
							posStr(fset, stmt.Pos()), tv.Type, relPkg)
					}
				}
			case *ast.CallExpr:
				if id, ok := stmt.Fun.(*ast.Ident); ok && id.Name == "panic" {
					t.Errorf("%s: panic() is forbidden in pure package %s (functions must be total)", posStr(fset, stmt.Pos()), relPkg)
				}
				// GDP Axiom restriction: types.Axiom[...]() may only be called inside internal/domain/proofs.go.
				if callCallsTypesAxiom(stmt, info) {
					if relPkg != "internal/domain" || baseName != "proofs.go" {
						t.Errorf("%s: types.Axiom may ONLY be called inside internal/domain/proofs.go (Prove* functions)",
							posStr(fset, stmt.Pos()))
					}
				}
				if sel, ok := stmt.Fun.(*ast.SelectorExpr); ok {
					if xid, ok := sel.X.(*ast.Ident); ok && xid.Name == "time" {
						switch sel.Sel.Name {
						case "Now", "Since", "Until", "Sleep", "After", "Tick":
							t.Errorf("%s: time.%s is forbidden in pure package %s",
								posStr(fset, stmt.Pos()), sel.Sel.Name, relPkg)
						}
					}
				}
			}
			return true
		})
	}

	// 4. Total Functional Programming / Finite Termination (Turner 2004):
	//    Verify that the intra-package call graph is a strict DAG (no direct or mutual recursion).
	//    Combined with banning general 'for' loops (allowing only bounded 'for ... range' over slices),
	//    call-graph acyclicity syntactically guarantees that every pure function terminates in finite steps.
	checkCallGraphAcyclic(t, fset, files, typedPkg, info, relPkg)

	return fset, files, typedPkg, info
}

func checkCallGraphAcyclic(t *testing.T, fset *token.FileSet, files []*ast.File, typedPkg *types.Package, info *types.Info, relPkg string) {
	t.Helper()
	funcKey := func(obj types.Object) string {
		fn, ok := obj.(*types.Func)
		if !ok || fn.Pkg() != typedPkg {
			return ""
		}
		sig, ok := fn.Type().(*types.Signature)
		if ok && sig.Recv() != nil {
			return sig.Recv().Type().String() + "." + fn.Name()
		}
		return fn.Name()
	}

	adj := make(map[string][]string)
	posMap := make(map[string]token.Pos)
	var nodes []string

	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			defObj := info.Defs[fd.Name]
			caller := funcKey(defObj)
			if caller == "" {
				continue
			}
			if _, exists := posMap[caller]; !exists {
				nodes = append(nodes, caller)
				posMap[caller] = fd.Name.Pos()
			}
			var callees []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var id *ast.Ident
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					id = fn
				case *ast.SelectorExpr:
					id = fn.Sel
				case *ast.IndexExpr:
					if i, ok := fn.X.(*ast.Ident); ok {
						id = i
					} else if s, ok := fn.X.(*ast.SelectorExpr); ok {
						id = s.Sel
					}
				case *ast.IndexListExpr:
					if i, ok := fn.X.(*ast.Ident); ok {
						id = i
					} else if s, ok := fn.X.(*ast.SelectorExpr); ok {
						id = s.Sel
					}
				}
				if id != nil {
					if callee := funcKey(info.Uses[id]); callee != "" {
						if !slices.Contains(callees, callee) {
							callees = append(callees, callee)
						}
					}
				}
				return true
			})
			adj[caller] = callees
		}
	}

	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(nodes))
	var stack []string
	var dfs func(u string)
	dfs = func(u string) {
		state[u] = visiting
		stack = append(stack, u)
		for _, v := range adj[u] {
			switch state[v] {
			case unvisited:
				dfs(v)
			case visiting:
				idx := slices.Index(stack, v)
				cycle := append(slices.Clone(stack[idx:]), v)
				t.Errorf("%s: recursion cycle %s in pure package %s violates Total Functional Programming / Finite Termination (pure call graph must be a strict DAG)",
					posStr(fset, posMap[u]), strings.Join(cycle, " -> "), relPkg)
			}
		}
		stack = stack[:len(stack)-1]
		state[u] = visited
	}

	for _, u := range nodes {
		if state[u] == unvisited {
			dfs(u)
		}
	}
}

func callCallsTypesAxiom(call *ast.CallExpr, info *types.Info) bool {
	expr := call.Fun
	if idx, ok := expr.(*ast.IndexExpr); ok {
		expr = idx.X
	} else if idxList, ok := expr.(*ast.IndexListExpr); ok {
		expr = idxList.X
	}
	switch fn := expr.(type) {
	case *ast.Ident:
		if obj, ok := info.Uses[fn]; ok && obj.Pkg() != nil && obj.Pkg().Path() == modulePrefix+"/internal/types" && obj.Name() == "Axiom" {
			return true
		}
	case *ast.SelectorExpr:
		if obj, ok := info.Uses[fn.Sel]; ok && obj.Pkg() != nil && obj.Pkg().Path() == modulePrefix+"/internal/types" && obj.Name() == "Axiom" {
			return true
		}
	}
	return false
}

// checkTypesPackage verifies that internal/types strictly separates type-theory
// primitives by file and that all fields of core generic structs are unexported
// (unforgeable).
func checkTypesPackage(t *testing.T, root string) {
	fset, files, typedPkg, _ := checkPureFiles(t, root, "internal/types")

	// Exact mapping of allowed files -> allowed type declarations in internal/types.
	allowedTypesByFile := map[string]map[string]bool{
		"adt.go":        {"Option": true, "NonEmpty": true, "Result": true},
		"quotient.go":   {"KeyExtractor": true, "SortedSet": true},
		"phantom.go":    {"Phantom": true, "Indexed": true},
		"refinement.go": {"Predicate": true, "Refined": true},
		"gdp.go":        {"Proof": true},
	}

	seenFiles := map[string]bool{}
	for _, f := range files {
		base := filepath.Base(fset.Position(f.Pos()).Filename)
		seenFiles[base] = true
		allowedTypes, ok := allowedTypesByFile[base]
		if !ok {
			t.Errorf("internal/types/%s: unexpected file in internal/types (allowed files: adt.go, quotient.go, phantom.go, refinement.go, gdp.go)", base)
			continue
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if ts.TypeParams == nil || ts.TypeParams.NumFields() == 0 {
						t.Errorf("%s: type %s in internal/types/%s is not generic; domain-specific types belong in internal/domain, not internal/types",
							posStr(fset, ts.Pos()), ts.Name.Name, base)
					}
					if !allowedTypes[ts.Name.Name] {
						t.Errorf("%s: type %s is not permitted in internal/types/%s (allowed in %s: %v)",
							posStr(fset, ts.Pos()), ts.Name.Name, base, base, allowedKeys(allowedTypes))
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) > 0 {
					rname := genericRecvTypeName(d.Recv.List[0].Type)
					if !allowedTypes[rname] {
						t.Errorf("%s: method on %s belongs in the file defining %s, not internal/types/%s",
							posStr(fset, d.Pos()), rname, rname, base)
					}
				}
			}
		}
	}

	for reqFile := range allowedTypesByFile {
		if !seenFiles[reqFile] {
			t.Errorf("internal/types: missing required taxonomy file %s", reqFile)
		}
	}

	scope := typedPkg.Scope()
	for _, name := range []string{"Refined", "Phantom", "Indexed", "Proof", "Option", "NonEmpty", "SortedSet", "Result"} {
		obj := scope.Lookup(name)
		if obj == nil {
			t.Fatalf("internal/types: missing required core type constructor %s", name)
		}
		st, ok := obj.Type().Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("internal/types: %s must be a struct, got %T", name, obj.Type().Underlying())
		}
		for i := range st.NumFields() {
			f := st.Field(i)
			if f.Exported() {
				t.Errorf("internal/types: field %s.%s is exported; all fields in %s must be unexported to guarantee unforgeability outside internal/types",
					name, f.Name(), name)
			}
		}
	}
}

// checkSpecPackage verifies that internal/spec enforces strict value semantics
// (no raw primitive types in World, Guest, Config, or Resources: only types.Refined,
// types.SortedSet, closed enums, etc.).
func checkSpecPackage(t *testing.T, root string) {
	_, _, typedPkg, _ := checkPureFiles(t, root, "internal/spec")
	scope := typedPkg.Scope()
	stats := &typeUsageStats{}
	for _, typeName := range []string{"World", "Guest", "Config", "Resources"} {
		obj := scope.Lookup(typeName)
		if obj == nil {
			t.Fatalf("internal/spec: missing required core type %q", typeName)
		}
		checkValueSemantics(t, "internal/spec", typeName, obj.Type(), map[types.Type]bool{}, stats)
	}
}

// checkDomainPackage verifies that internal/domain strictly partitions domain
// vocabulary declarations across tags.go, predicates.go, and proofs.go.
func checkDomainPackage(t *testing.T, root string) {
	fset, files, _, _ := checkPureFiles(t, root, "internal/domain")

	allowedFiles := map[string]bool{
		"tags.go":       true,
		"predicates.go": true,
		"proofs.go":     true,
	}
	seenFiles := map[string]bool{}

	for _, f := range files {
		base := filepath.Base(fset.Position(f.Pos()).Filename)
		seenFiles[base] = true
		if !allowedFiles[base] {
			t.Errorf("internal/domain/%s: unexpected file (allowed files: tags.go, predicates.go, proofs.go)", base)
			continue
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					name := ts.Name.Name
					switch base {
					case "tags.go":
						if strings.HasSuffix(name, "Pred") || strings.HasSuffix(name, "Order") || strings.HasSuffix(name, "Prop") || strings.HasSuffix(name, "Proof") {
							t.Errorf("%s: %s does not belong in internal/domain/tags.go (only Phantom *Tag and Typestate *State definitions belong here)",
								posStr(fset, ts.Pos()), name)
						}
					case "predicates.go":
						if !ts.Assign.IsValid() && (strings.HasSuffix(name, "Tag") || strings.HasSuffix(name, "State") || strings.HasSuffix(name, "Prop")) {
							t.Errorf("%s: %s does not belong in internal/domain/predicates.go (only *Pred, *Order, and Refined type aliases belong here)",
								posStr(fset, ts.Pos()), name)
						}
						if strings.HasSuffix(name, "Proof") {
							t.Errorf("%s: %s does not belong in internal/domain/predicates.go (proof aliases belong in proofs.go)",
								posStr(fset, ts.Pos()), name)
						}
					case "proofs.go":
						if !strings.HasSuffix(name, "Prop") && !strings.HasSuffix(name, "Proof") {
							t.Errorf("%s: %s in internal/domain/proofs.go must be a proposition (*Prop) or proof alias (*Proof)",
								posStr(fset, ts.Pos()), name)
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && ast.IsExported(d.Name.Name) {
					name := d.Name.Name
					switch base {
					case "tags.go":
						allowedPhantomConstructors := map[string]bool{
							"NewText":        true,
							"NewPeerCID":     true,
							"NewUnixSec":     true,
							"NewCommandText": true,
						}
						if !allowedPhantomConstructors[name] {
							t.Errorf("%s: exported function %s in internal/domain/tags.go is forbidden; do not add unchecked New* Phantom constructors over primitives (use a Refined[T, P] type with a non-trivial Check predicate in predicates.go)",
								posStr(fset, d.Pos()), name)
						}
					case "proofs.go":
						if !strings.HasPrefix(name, "Prove") {
							t.Errorf("%s: exported function %s in internal/domain/proofs.go must be a GDP proof rule (Prove*)",
								posStr(fset, d.Pos()), name)
						}
					case "predicates.go":
						if strings.HasPrefix(name, "Prove") {
							t.Errorf("%s: proof rule %s belongs in internal/domain/proofs.go, not predicates.go",
								posStr(fset, d.Pos()), name)
						}
					}
				}
				if base == "predicates.go" && d.Recv != nil && d.Name.Name == "Check" && d.Body != nil {
					hasBranch := false
					ast.Inspect(d.Body, func(n ast.Node) bool {
						switch x := n.(type) {
						case *ast.IfStmt, *ast.SwitchStmt:
							hasBranch = true
						case *ast.CallExpr:
							if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Check" {
								hasBranch = true
							}
						}
						return true
					})
					if !hasBranch {
						t.Errorf("%s: Predicate Check method in internal/domain/predicates.go is trivial (must contain a non-trivial validation branch)",
							posStr(fset, d.Pos()))
					}
				}
			}
		}
	}

	for req := range allowedFiles {
		if !seenFiles[req] {
			t.Errorf("internal/domain: missing required taxonomy file %s", req)
		}
	}
}

func checkTEAPackage(t *testing.T, root, relPkg string) {
	fset, files, typedPkg, _ := checkPureFiles(t, root, relPkg)

	// 4. Verify required TEA + IronFleet/TLA+ symbols:
	//    Model, Msg, Effect, Init, Update, Invariant, StepValid.
	scope := typedPkg.Scope()
	modelObj := scope.Lookup("Model")
	if modelObj == nil {
		t.Fatalf("%s: missing required type 'Model'", relPkg)
	}
	if _, ok := modelObj.Type().Underlying().(*types.Struct); !ok {
		t.Fatalf("%s: 'Model' must be a struct, got %T", relPkg, modelObj.Type().Underlying())
	}

	msgIface := requireSealedInterface(t, relPkg, scope, "Msg", "isMsg")
	effIface := requireSealedInterface(t, relPkg, scope, "Effect", "isEffect")
	msgType := scope.Lookup("Msg").Type()
	effSliceType := types.NewSlice(scope.Lookup("Effect").Type())

	// Check Init(...) Model
	initObj := scope.Lookup("Init")
	if initObj == nil {
		t.Fatalf("%s: missing required constructor 'Init(...) Model'", relPkg)
	}
	initSig, ok := initObj.Type().(*types.Signature)
	if !ok || initSig.Results().Len() != 1 || !types.Identical(initSig.Results().At(0).Type(), modelObj.Type()) {
		t.Errorf("%s: Init must return Model, got %s", relPkg, initObj.Type())
	}

	// Check Update(Model, Msg) (Model, []Effect)
	updateObj := scope.Lookup("Update")
	if updateObj == nil {
		t.Fatalf("%s: missing required function 'Update(Model, Msg) (Model, []Effect)'", relPkg)
	}
	updateSig, ok := updateObj.Type().(*types.Signature)
	if !ok || updateSig.Params().Len() != 2 || updateSig.Results().Len() != 2 ||
		!types.Identical(updateSig.Params().At(0).Type(), modelObj.Type()) ||
		!types.Identical(updateSig.Params().At(1).Type(), msgType) ||
		!types.Identical(updateSig.Results().At(0).Type(), modelObj.Type()) ||
		!types.Identical(updateSig.Results().At(1).Type(), effSliceType) {
		t.Errorf("%s: Update must have exact signature func(Model, Msg) (Model, []Effect), got %s", relPkg, updateObj.Type())
	}

	// Check Invariant(Model) error
	invObj := scope.Lookup("Invariant")
	if invObj == nil {
		t.Fatalf("%s: missing required function 'Invariant(Model) error'", relPkg)
	}
	invSig, ok := invObj.Type().(*types.Signature)
	if !ok || invSig.Params().Len() != 1 || invSig.Results().Len() != 1 ||
		!types.Identical(invSig.Params().At(0).Type(), modelObj.Type()) ||
		invSig.Results().At(0).Type().String() != "error" {
		t.Errorf("%s: Invariant must have exact signature func(Model) error, got %s", relPkg, invObj.Type())
	}

	// Check FrameValid(Model, Msg, Model) error (Separation Logic Frame Rule: Reynolds 2002)
	frameObj := scope.Lookup("FrameValid")
	if frameObj == nil {
		t.Fatalf("%s: missing required Separation Logic frame predicate 'FrameValid(Model, Msg, Model) error'", relPkg)
	}
	frameSig, ok := frameObj.Type().(*types.Signature)
	if !ok || frameSig.Params().Len() != 3 || frameSig.Results().Len() != 1 ||
		!types.Identical(frameSig.Params().At(0).Type(), modelObj.Type()) ||
		!types.Identical(frameSig.Params().At(1).Type(), msgType) ||
		!types.Identical(frameSig.Params().At(2).Type(), modelObj.Type()) ||
		frameSig.Results().At(0).Type().String() != "error" {
		t.Errorf("%s: FrameValid must have exact signature func(Model, Msg, Model) error, got %s", relPkg, frameObj.Type())
	}

	// Check StepValid(Model, Msg, Model, []Effect) error (IronFleet / TLA+ action invariant)
	stepObj := scope.Lookup("StepValid")
	if stepObj == nil {
		t.Fatalf("%s: missing required transition relation predicate 'StepValid(Model, Msg, Model, []Effect) error'", relPkg)
	}
	stepSig, ok := stepObj.Type().(*types.Signature)
	if !ok || stepSig.Params().Len() != 4 || stepSig.Results().Len() != 1 ||
		!types.Identical(stepSig.Params().At(0).Type(), modelObj.Type()) ||
		!types.Identical(stepSig.Params().At(1).Type(), msgType) ||
		!types.Identical(stepSig.Params().At(2).Type(), modelObj.Type()) ||
		!types.Identical(stepSig.Params().At(3).Type(), effSliceType) ||
		stepSig.Results().At(0).Type().String() != "error" {
		t.Errorf("%s: StepValid must have exact signature func(Model, Msg, Model, []Effect) error, got %s", relPkg, stepObj.Type())
	}

	// 5. Static State-Space Complexity Gate (Clarke et al.):
	//    Bound the number of top-level fields in Model to prevent combinatorial state explosion.
	const maxModelFields = 6
	if modelSt, ok := modelObj.Type().Underlying().(*types.Struct); ok {
		if modelSt.NumFields() > maxModelFields {
			t.Errorf("%s: Model has %d fields (max allowed: %d); decompose orthogonal state components to prevent state-space explosion",
				relPkg, modelSt.NumFields(), maxModelFields)
		}
	}

	// 6. Verify strict SMT-friendly value semantics, Field Type Whitelist
	//    (no bare primitives: only types.Refined, types.Phantom, types.Indexed,
	//    types.Proof, types.Option, types.NonEmpty, types.SortedSet, closed Enums,
	//    at most 1 bool per struct for SSOT, and domain structs/slices), and
	//    mandatory usage of Refinement Types, Phantom/GDP/Typestate Types, and
	//    Canonical SortedSet in Model.
	stats := &typeUsageStats{}
	checkValueSemantics(t, relPkg, "Model", modelObj.Type(), map[types.Type]bool{}, stats)

	var msgTypes []string
	var effTypes []string
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		tn, ok := obj.(*types.TypeName)
		if !ok || tn.Name() == "Msg" || tn.Name() == "Effect" {
			continue
		}
		if types.Implements(tn.Type(), msgIface) {
			if !strings.HasPrefix(tn.Name(), "Msg") {
				t.Errorf("%s: type %s implements Msg and must be named with 'Msg' prefix", relPkg, tn.Name())
			}
			msgTypes = append(msgTypes, tn.Name())
			checkValueSemantics(t, relPkg, tn.Name(), tn.Type(), map[types.Type]bool{}, stats)
		}
		if types.Implements(tn.Type(), effIface) {
			if !strings.HasPrefix(tn.Name(), "Effect") {
				t.Errorf("%s: type %s implements Effect and must be named with 'Effect' prefix", relPkg, tn.Name())
			}
			effTypes = append(effTypes, tn.Name())
			checkValueSemantics(t, relPkg, tn.Name(), tn.Type(), map[types.Type]bool{}, stats)
		}
	}

	if !stats.usedRefined {
		t.Errorf("%s: TEA domain package must use Refinement Types (types.Refined[T, P]) from internal/types", relPkg)
	}
	if !stats.usedPhantom {
		t.Errorf("%s: TEA domain package must use Phantom / Typestate / GDP Types (types.Phantom, types.Indexed, or types.Proof) from internal/types", relPkg)
	}
	if !stats.usedSortedSet {
		t.Errorf("%s: TEA domain package Model must use canonical Quotient Container types.SortedSet[T, K, Ord] from internal/types", relPkg)
	}

	if len(msgTypes) == 0 {
		t.Errorf("%s: no concrete types implement Msg", relPkg)
	}

	// 7. Verify that Update contains an exhaustive type switch covering every Msg implementation.
	checkExhaustiveMsgSwitchInUpdate(t, fset, files, msgTypes)

	// 8. Verify that the package's *_test.go verifies Separation Logic (FrameValid),
	//    Hoare Logic (Invariant, StepValid), and Incorrectness Logic (O'Hearn POPL 2020:
	//    under-approximate reachability of every concrete Msg and Effect type).
	checkTEATestVerificationSuite(t, root, relPkg, msgTypes, effTypes)
}

func checkTEATestVerificationSuite(t *testing.T, root, relPkg string, msgTypes, effTypes []string) {
	t.Helper()
	fset := token.NewFileSet()
	testFiles := parseTestFiles(t, fset, filepath.Join(root, relPkg))
	if len(testFiles) == 0 {
		t.Errorf("%s: missing required *_test.go formal verification test suite", relPkg)
		return
	}
	usedIdents := map[string]bool{}
	for _, tf := range testFiles {
		ast.Inspect(tf, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				usedIdents[id.Name] = true
			}
			return true
		})
	}
	for _, reqFn := range []string{"Invariant", "FrameValid", "StepValid", "Update"} {
		if !usedIdents[reqFn] {
			t.Errorf("%s: *_test.go must invoke %s to verify Hoare / Separation Logic properties", relPkg, reqFn)
		}
	}
	for _, mt := range msgTypes {
		if !usedIdents[mt] {
			t.Errorf("%s: *_test.go never exercises Msg type %s (violates Incorrectness Logic under-approximate reachability)", relPkg, mt)
		}
	}
	for _, et := range effTypes {
		if !usedIdents[et] {
			t.Errorf("%s: *_test.go never asserts reachability of Effect type %s (violates Incorrectness Logic under-approximate reachability)", relPkg, et)
		}
	}
}

type typeUsageStats struct {
	usedRefined   bool
	usedPhantom   bool
	usedSortedSet bool
}

func requireSealedInterface(t *testing.T, relPkg string, scope *types.Scope, name, marker string) *types.Interface {
	t.Helper()
	obj := scope.Lookup(name)
	if obj == nil {
		t.Fatalf("%s: missing required interface %q", relPkg, name)
	}
	iface, ok := obj.Type().Underlying().(*types.Interface)
	if !ok {
		t.Fatalf("%s: %s must be an interface, got %T", relPkg, name, obj.Type().Underlying())
	}
	hasMarker := false
	for i := range iface.NumMethods() {
		if iface.Method(i).Name() == marker {
			hasMarker = true
			break
		}
	}
	if !hasMarker {
		t.Errorf("%s: interface %s must be sealed with unexported marker method %s()", relPkg, name, marker)
	}
	return iface
}

func checkValueSemantics(t *testing.T, relPkg, rootName string, typ types.Type, seen map[types.Type]bool, stats *typeUsageStats) {
	t.Helper()
	unaliased := types.Unalias(typ)
	if named, ok := unaliased.(*types.Named); ok {
		if pkg := named.Obj().Pkg(); pkg != nil && pkg.Path() == "net/netip" {
			switch named.Obj().Name() {
			case "Addr", "Prefix":
				return
			}
		}
		if pkg := named.Obj().Pkg(); pkg != nil && pkg.Path() == modulePrefix+"/internal/types" {
			switch named.Obj().Name() {
			case "Refined":
				stats.usedRefined = true
				return
			case "Phantom", "Proof":
				stats.usedPhantom = true
				return
			case "Indexed":
				stats.usedPhantom = true
				if named.TypeArgs() != nil && named.TypeArgs().Len() >= 1 {
					checkValueSemantics(t, relPkg, rootName, named.TypeArgs().At(0), seen, stats)
				}
				return
			case "Option", "NonEmpty":
				if named.TypeArgs() != nil && named.TypeArgs().Len() >= 1 {
					checkValueSemantics(t, relPkg, rootName, named.TypeArgs().At(0), seen, stats)
				}
				return
			case "SortedSet":
				stats.usedSortedSet = true
				if named.TypeArgs() != nil && named.TypeArgs().Len() >= 2 {
					checkValueSemantics(t, relPkg, rootName, named.TypeArgs().At(0), seen, stats)
					checkValueSemantics(t, relPkg, rootName, named.TypeArgs().At(1), seen, stats)
				}
				return
			}
		}
	}

	if seen[unaliased] {
		return
	}
	seen[unaliased] = true

	switch u := unaliased.Underlying().(type) {
	case *types.Basic:
		if u.Info()&(types.IsFloat|types.IsComplex) != 0 {
			t.Errorf("%s: %s contains floating/complex type %s (forbidden in TEA Model/Msg/Effect; breaks reflexive equality and SMT decidability)",
				relPkg, rootName, typ)
			return
		}
		if u.Kind() == types.Bool {
			return
		}
		if named, ok := unaliased.(*types.Named); ok && isClosedEnumType(named) {
			return
		}
		t.Errorf("%s: %s uses raw primitive type %s; TEA structs may only use types from internal/types & internal/domain (Refined, Phantom, Indexed, Proof, Option, NonEmpty, SortedSet), closed Enum types, or a single bool",
			relPkg, rootName, typ)
	case *types.Slice:
		if rootName == "Model" || strings.HasPrefix(rootName, "Model.") {
			t.Errorf("%s: %s uses raw slice %s in Model; use canonical Quotient Container types.SortedSet[T, K, Ord] in Model to guarantee SSOT uniqueness and canonical ordering",
				relPkg, rootName, typ)
		}
		checkValueSemantics(t, relPkg, rootName, u.Elem(), seen, stats)
	case *types.Array:
		checkValueSemantics(t, relPkg, rootName, u.Elem(), seen, stats)
	case *types.Struct:
		boolCount := 0
		for i := range u.NumFields() {
			f := u.Field(i)
			if b, ok := types.Unalias(f.Type()).Underlying().(*types.Basic); ok && b.Kind() == types.Bool {
				boolCount++
			}
			checkValueSemantics(t, relPkg, rootName+"."+f.Name(), f.Type(), seen, stats)
		}
		if boolCount > 1 {
			t.Errorf("%s: %s has %d bool fields; SSOT state normalization allows at most 1 bool per struct (replace multiple bools with a closed Enum sum type to make illegal combinations unrepresentable)",
				relPkg, rootName, boolCount)
		}
	case *types.Pointer:
		t.Errorf("%s: %s contains pointer type %s (forbidden in TEA Model/Msg/Effect; use value types)", relPkg, rootName, typ)
	case *types.Map:
		t.Errorf("%s: %s contains map type %s (forbidden in TEA Model/Msg/Effect; use sorted slices for determinism)", relPkg, rootName, typ)
	case *types.Signature:
		t.Errorf("%s: %s contains function type %s (forbidden in TEA Model/Msg/Effect; data must be inspectable)", relPkg, rootName, typ)
	case *types.Chan:
		t.Errorf("%s: %s contains channel type %s (forbidden in TEA Model/Msg/Effect)", relPkg, rootName, typ)
	case *types.Interface:
		hasMarker := false
		for i := range u.NumMethods() {
			m := u.Method(i).Name()
			if m == "isMsg" || m == "isEffect" {
				hasMarker = true
				break
			}
		}
		if !hasMarker {
			t.Errorf("%s: %s contains unsealed interface %s (forbidden in TEA Model/Msg/Effect)", relPkg, rootName, typ)
		}
	}
}

// isClosedEnumType reports whether named is a defined basic type with at least
// 2 package-level const values in its defining package.
func isClosedEnumType(named *types.Named) bool {
	pkg := named.Obj().Pkg()
	if pkg == nil {
		return false
	}
	scope := pkg.Scope()
	consts := 0
	for _, name := range scope.Names() {
		if c, ok := scope.Lookup(name).(*types.Const); ok && types.Identical(c.Type(), named) {
			consts++
		}
	}
	return consts >= 2
}

func checkExhaustiveMsgSwitchInUpdate(t *testing.T, fset *token.FileSet, files []*ast.File, msgTypes []string) {
	t.Helper()
	covered := map[string]bool{}
	foundUpdate := false

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Update" || fn.Recv != nil {
				continue
			}
			foundUpdate = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSwitchStmt)
				if !ok {
					return true
				}
				for _, stmt := range ts.Body.List {
					cc, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, expr := range cc.List {
						if id, ok := expr.(*ast.Ident); ok {
							covered[id.Name] = true
						}
					}
				}
				return true
			})
		}
	}

	if !foundUpdate {
		return
	}
	for _, mt := range msgTypes {
		if !covered[mt] {
			t.Errorf("%s: Update type switch is not exhaustive: missing case for Msg type %s",
				fset.Position(files[0].Pos()).Filename, mt)
		}
	}
}

// badInitialisms maps improperly cased initialisms/acronyms in CamelCase identifiers
// to their required Go casing (allowing plural 's' after an uppercase initialism, e.g. CPUs, PIDs, IDs).
var badInitialismRE = regexp.MustCompile(
	`(?:^|[a-z0-9])(Id|Vm|Os|Gc|Ip|Tcp|Udp|Dns|Ttl|Http|Url|Uri|Api|Cli|Json|Toml|Pid|Uid|Gid|Pty|Cpu|Rng|Cid|Cidr|Nft)(?:$|s?[A-Z0-9]|s$)` +
		`|^(?:ID|VM|OS|GC|IP|TCP|UDP|DNS|TTL|HTTP|URL|URI|API|CLI|JSON|TOML|PID|UID|GID|PTY|CPU|RNG|CID|CIDR|NFT)[a-rt-z]`,
)

// TestNamingAndExportConventions enforces strict identifier naming across the entire repository:
//  1. Export visibility ("Uppercase vs lowercase"): a package-level symbol in internal/...
//     may start with an Uppercase letter ONLY if it is referenced by another package
//     (or is part of the TEA / wire-protocol specification). Package-internal symbols MUST
//     start with a lowercase letter.
//  2. No underscores (snake_case) in Go identifiers.
//  3. Consistent initialism / acronym casing (ID, VM, OS, GC, IP, TCP, UDP, DNS, TTL, PID, NFT, etc.).
//  4. Consistent receiver names across all methods of a type.
func TestNamingAndExportConventions(t *testing.T) {
	root := repoRoot(t)
	allPkgs := append(discoverPackages(t, root, "cmd"), discoverPackages(t, root, "internal")...)

	// Collect all cross-package selector usages `pkgAlias.ExportedIdent` across the repo
	// (including test files in other packages).
	externalRefs := collectExternalSymbolRefs(t, root, allPkgs)

	for _, relPkg := range allPkgs {
		if relPkg == "internal/arch" {
			continue
		}
		fset := token.NewFileSet()
		files := parseNonTestFiles(t, fset, filepath.Join(root, relPkg))
		testFiles := parseTestFiles(t, fset, filepath.Join(root, relPkg))

		isPureSpec := strings.HasPrefix(relPkg, "internal/tea/") || relPkg == "internal/proto" || relPkg == "internal/types" || relPkg == "internal/domain" || relPkg == "internal/spec"
		refsInOtherPkgs := markTransitiveExportedTypes(files, externalRefs[relPkg])

		receiverNames := map[string]string{}           // typeName -> first receiver ident
		receiverPos := make(map[string]token.Position) // typeName -> first pos

		for _, f := range files {
			// SSOT Regex Ban: domain validation predicates belong in internal/domain.
			// Forbid duplicate validation regexes in internal/guest/agent and internal/host/egress,
			// and forbid guest-name regex duplication in internal/host/config.
			for _, imp := range f.Imports {
				if strings.Trim(imp.Path.Value, `"`) == "regexp" {
					if relPkg == "internal/guest/agent" || relPkg == "internal/host/egress" {
						t.Errorf("%s (%s): importing \"regexp\" is forbidden; use SSOT refinement types in internal/domain",
							posStr(fset, imp.Pos()), relPkg)
					}
				}
			}

			// Check top-level declarations for dead exports (uppercase symbols only used internally).
			if strings.HasPrefix(relPkg, "internal/") && !isPureSpec {
				for _, decl := range f.Decls {
					switch d := decl.(type) {
					case *ast.FuncDecl:
						if d.Recv == nil && ast.IsExported(d.Name.Name) {
							if !refsInOtherPkgs[d.Name.Name] {
								t.Errorf("%s (%s): function %q starts with uppercase (exported) but is never used outside %s; make it unexported (lowercase)",
									posStr(fset, d.Name.Pos()), relPkg, d.Name.Name, relPkg)
							}
						}
					case *ast.GenDecl:
						for _, spec := range d.Specs {
							switch s := spec.(type) {
							case *ast.TypeSpec:
								if ast.IsExported(s.Name.Name) && !refsInOtherPkgs[s.Name.Name] {
									t.Errorf("%s (%s): type %q starts with uppercase (exported) but is never used outside %s; make it unexported (lowercase)",
										posStr(fset, s.Name.Pos()), relPkg, s.Name.Name, relPkg)
								}
							case *ast.ValueSpec:
								for _, id := range s.Names {
									if ast.IsExported(id.Name) && !refsInOtherPkgs[id.Name] {
										t.Errorf("%s (%s): %s %q starts with uppercase (exported) but is never used outside %s; make it unexported (lowercase)",
											posStr(fset, id.Pos()), relPkg, d.Tok, id.Name, relPkg)
									}
								}
							}
						}
					}
				}
			}

			// Check receiver naming consistency and identifier casing across all declarations.
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
					continue
				}
				recvField := fn.Recv.List[0]
				typeName := recvTypeName(recvField.Type)
				if typeName == "" || len(recvField.Names) == 0 {
					continue
				}
				rname := recvField.Names[0].Name
				if rname == "_" {
					continue
				}
				if prev, exists := receiverNames[typeName]; exists && prev != rname {
					t.Errorf("%s (%s): method %s.%s uses receiver name %q, but earlier method at %s:%d used %q",
						posStr(fset, recvField.Pos()), relPkg, typeName, fn.Name.Name, rname,
						filepath.Base(receiverPos[typeName].Filename), receiverPos[typeName].Line, prev)
				} else if !exists {
					receiverNames[typeName] = rname
					receiverPos[typeName] = fset.Position(recvField.Pos())
				}
			}

			checkFileIdentifierConventions(t, fset, relPkg, f, false)
		}

		for _, tf := range testFiles {
			checkFileIdentifierConventions(t, fset, relPkg, tf, true)
		}
	}
}

func checkFileIdentifierConventions(t *testing.T, fset *token.FileSet, relPkg string, f *ast.File, isTest bool) {
	t.Helper()

	checkIdent := func(id *ast.Ident, kind string, mustBeLower bool) {
		if id == nil || id.Name == "_" || id.Name == "" {
			return
		}
		name := id.Name
		if isTest && kind == "func" && (strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Fuzz") || strings.HasPrefix(name, "Example")) {
			if m := badInitialismRE.FindString(name); m != "" {
				t.Errorf("%s (%s): %s %q has inconsistent initialism casing (%q)", posStr(fset, id.Pos()), relPkg, kind, name, m)
			}
			return
		}
		if strings.Contains(name, "_") {
			t.Errorf("%s (%s): %s %q contains underscore; use CamelCase without underscores",
				posStr(fset, id.Pos()), relPkg, kind, name)
		}
		if mustBeLower && unicode.IsUpper(rune(name[0])) {
			t.Errorf("%s (%s): local/parameter %s %q must start with a lowercase letter",
				posStr(fset, id.Pos()), relPkg, kind, name)
		}
		if m := badInitialismRE.FindString(name); m != "" {
			t.Errorf("%s (%s): %s %q has inconsistent initialism casing (%q); use all-caps or all-lowercase for acronyms (ID, VM, OS, GC, NFT, PID, etc.)",
				posStr(fset, id.Pos()), relPkg, kind, name, m)
		}
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			checkIdent(d.Name, "func", isTest)
			if d.Recv != nil {
				for _, field := range d.Recv.List {
					for _, id := range field.Names {
						checkIdent(id, "receiver", true)
					}
				}
			}
			if d.Type.Params != nil {
				for _, field := range d.Type.Params.List {
					for _, id := range field.Names {
						checkIdent(id, "param", true)
					}
				}
			}
			if d.Type.Results != nil {
				for _, field := range d.Type.Results.List {
					for _, id := range field.Names {
						checkIdent(id, "result", true)
					}
				}
			}
			if d.Body != nil {
				ast.Inspect(d.Body, func(n ast.Node) bool {
					switch stmt := n.(type) {
					case *ast.AssignStmt:
						if stmt.Tok == token.DEFINE {
							for _, lhs := range stmt.Lhs {
								if id, ok := lhs.(*ast.Ident); ok {
									checkIdent(id, "local var", true)
								}
							}
						}
					case *ast.ValueSpec:
						for _, id := range stmt.Names {
							checkIdent(id, "local const/var", true)
						}
					case *ast.TypeSpec:
						checkIdent(stmt.Name, "local type", true)
					}
					return true
				})
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					checkIdent(s.Name, "type", isTest)
					if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, field := range st.Fields.List {
							for _, id := range field.Names {
								checkIdent(id, "field", false)
							}
						}
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						checkIdent(id, d.Tok.String(), isTest)
					}
				}
			}
		}
	}
}

// markTransitiveExportedTypes expands the set of externally referenced symbols
// in a package to include any exported type that appears in the signature of an
// externally referenced function/method or in an exported field of a referenced struct.
func markTransitiveExportedTypes(files []*ast.File, direct map[string]bool) map[string]bool {
	out := make(map[string]bool, len(direct))
	for k, v := range direct {
		out[k] = v
	}

	markExprTypes := func(expr ast.Expr) {
		if expr == nil {
			return
		}
		ast.Inspect(expr, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && ast.IsExported(id.Name) {
				out[id.Name] = true
			}
			return true
		})
	}

	changed := true
	for changed {
		changed = false
		before := len(out)
		for _, f := range files {
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					used := false
					if d.Recv == nil {
						used = out[d.Name.Name]
					} else if len(d.Recv.List) > 0 {
						rt := recvTypeName(d.Recv.List[0].Type)
						used = out[rt] && ast.IsExported(d.Name.Name)
					}
					if used {
						if d.Type.Params != nil {
							for _, fld := range d.Type.Params.List {
								markExprTypes(fld.Type)
							}
						}
						if d.Type.Results != nil {
							for _, fld := range d.Type.Results.List {
								markExprTypes(fld.Type)
							}
						}
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok || !out[ts.Name.Name] {
							continue
						}
						markExprTypes(ts.Type)
					}
				}
			}
		}
		if len(out) > before {
			changed = true
		}
	}
	return out
}

func collectExternalSymbolRefs(t *testing.T, root string, allPkgs []string) map[string]map[string]bool {
	t.Helper()
	refs := make(map[string]map[string]bool)
	for _, p := range allPkgs {
		refs[p] = make(map[string]bool)
	}

	for _, callerPkg := range allPkgs {
		fset := token.NewFileSet()
		dir := filepath.Join(root, callerPkg)
		allFiles := append(parseNonTestFiles(t, fset, dir), parseTestFiles(t, fset, dir)...)
		for _, f := range allFiles {
			aliasToPkg := map[string]string{}
			for _, imp := range f.Imports {
				impPath := strings.Trim(imp.Path.Value, `"`)
				if !strings.HasPrefix(impPath, modulePrefix+"/") {
					continue
				}
				targetRel := strings.TrimPrefix(impPath, modulePrefix+"/")
				alias := filepath.Base(targetRel)
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				aliasToPkg[alias] = targetRel
			}

			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				xid, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if targetRel, ok := aliasToPkg[xid.Name]; ok && targetRel != callerPkg {
					if refs[targetRel] == nil {
						refs[targetRel] = make(map[string]bool)
					}
					refs[targetRel][sel.Sel.Name] = true
				}
				return true
			})
		}
	}
	return refs
}

func recvTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.StarExpr:
		return recvTypeName(e.X)
	}
	return ""
}

func genericRecvTypeName(expr ast.Expr) string {
	return recvTypeName(expr)
}

func discoverPackages(t *testing.T, root, sub string) []string {
	t.Helper()
	var out []string
	base := filepath.Join(root, sub)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		hasGo := false
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				hasGo = true
				break
			}
		}
		if hasGo {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func parseNonTestFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func parseTestFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func posStr(fset *token.FileSet, p token.Pos) string {
	pos := fset.Position(p)
	return fmt.Sprintf("%s:%d:%d", filepath.Base(pos.Filename), pos.Line, pos.Column)
}

func allowedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

type repoImporter struct {
	t     *testing.T
	root  string
	std   types.Importer
	cache map[string]*types.Package
}

// firstOf imports with the first importer that can.
type firstOf []types.Importer

func (is firstOf) Import(path string) (*types.Package, error) {
	var errs []error
	for _, i := range is {
		pkg, err := i.Import(path)
		if err == nil {
			return pkg, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// newRepoImporter type-checks the module from source and the standard
// library from export data, or from its source where export data is not
// available (the Nix build sandbox has no build cache to produce it).
func newRepoImporter(t *testing.T, root string) *repoImporter {
	// A test binary built with -trimpath (as Nix builds it) does not know
	// GOROOT; the go command does.
	if build.Default.GOROOT == "" {
		if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
			build.Default.GOROOT = strings.TrimSpace(string(out))
		}
	}
	return &repoImporter{
		t:     t,
		root:  root,
		std:   firstOf{importer.Default(), importer.ForCompiler(token.NewFileSet(), "source", nil)},
		cache: make(map[string]*types.Package),
	}
}

func (imp *repoImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := imp.cache[path]; ok {
		return pkg, nil
	}
	if path == modulePrefix || strings.HasPrefix(path, modulePrefix+"/") {
		rel := strings.TrimPrefix(strings.TrimPrefix(path, modulePrefix), "/")
		fset := token.NewFileSet()
		files := parseNonTestFiles(imp.t, fset, filepath.Join(imp.root, rel))
		conf := types.Config{Importer: imp, Error: func(error) {}}
		pkg, _ := conf.Check(path, fset, files, nil)
		if pkg == nil {
			return nil, fmt.Errorf("failed to check %s", path)
		}
		imp.cache[path] = pkg
		return pkg, nil
	}
	pkg, err := imp.std.Import(path)
	if err == nil {
		imp.cache[path] = pkg
		return pkg, nil
	}
	// Fallback for external modules when running in pure source environments:
	dummy := types.NewPackage(path, filepath.Base(path))
	dummy.MarkComplete()
	imp.cache[path] = dummy
	return dummy, nil
}

// TestADTSafety enforces algebraic safety conventions for types.Result and types.Option
// across all production code in the repository:
//  1. Calling .Value() directly on types.Result is forbidden in production code
//     (must use Unwrap(), ValueOr(fallback), or monadic Map/AndThen).
//  2. Expressions evaluating to types.Result cannot be bare expressions in ExprStmt
//     (preventing accidental error discarding).
//  3. Manual unpacking patterns `if res.IsErr() { return ... }` are audited to promote
//     monadic composition (types.AndThen / types.Map).
func TestADTSafety(t *testing.T) {
	root := repoRoot(t)
	allPkgs := append(discoverPackages(t, root, "cmd"), discoverPackages(t, root, "internal")...)

	imp := newRepoImporter(t, root)

	for _, relPkg := range allPkgs {
		if relPkg == "internal/arch" || relPkg == "internal/types" {
			continue
		}
		fset := token.NewFileSet()
		files := parseNonTestFiles(t, fset, filepath.Join(root, relPkg))
		if len(files) == 0 {
			continue
		}

		info := &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}
		conf := types.Config{
			Importer: imp,
			Error:    func(error) {}, // suppress errors from dummy external package imports
		}
		conf.Check(modulePrefix+"/"+relPkg, fset, files, info)

		for _, f := range files {
			checkFileADTSafety(t, fset, relPkg, f, info)
		}
	}

	// Verify that the safety checks actively catch violations:
	t.Run("ViolationsDetected", func(t *testing.T) {
		const snippet = `package dummy
import (
	"errors"
	"github.com/RNG999/antic/internal/types"
)
func testViolation(r types.Result[string]) types.Result[int] {
	r.Value()         // violation 1: .Value() in production code
	types.Ok("hello") // violation 2: unhandled Result[T] statement
	err := errors.New("boom")
	if err != nil {
		return types.Err[int](err) // violation 3: manual if err != nil boilerplate returning Result
	}
	return types.Ok(42)
}
`
		testFset := token.NewFileSet()
		f, err := parser.ParseFile(testFset, "dummy.go", snippet, 0)
		if err != nil {
			t.Fatal(err)
		}
		info := &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		conf.Check("dummy", testFset, []*ast.File{f}, info)

		var errs []string
		mockT := &recordingT{errs: &errs}
		checkFileADTSafety(mockT, testFset, "dummy", f, info)

		if len(errs) == 0 {
			t.Fatalf("expected ADT safety violations to be flagged, got none")
		}
		foundValueViolation := false
		foundDiscardViolation := false
		foundIfErrViolation := false
		for _, e := range errs {
			if strings.Contains(e, "calling Result.Value() is forbidden") {
				foundValueViolation = true
			}
			if strings.Contains(e, "unhandled Result[T]") {
				foundDiscardViolation = true
			}
			if strings.Contains(e, "manual 'if err != nil") {
				foundIfErrViolation = true
			}
		}
		if !foundValueViolation {
			t.Fatalf("expected Result.Value() error, got: %v", errs)
		}
		if !foundDiscardViolation {
			t.Fatalf("expected unhandled Result[T] discard error, got: %v", errs)
		}
		if !foundIfErrViolation {
			t.Fatalf("expected manual if err != nil error, got: %v", errs)
		}
	})
}

type recordingT struct {
	errs *[]string
}

func (r *recordingT) Errorf(format string, args ...any) {
	*r.errs = append(*r.errs, fmt.Sprintf(format, args...))
}

type errorReporter interface {
	Errorf(format string, args ...any)
}

func checkFileADTSafety(t errorReporter, fset *token.FileSet, relPkg string, f *ast.File, info *types.Info) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Value" {
					recvType := info.TypeOf(sel.X)
					if isResultType(recvType) {
						t.Errorf("%s (%s): calling Result.Value() is forbidden in production code; use Unwrap(), ValueOr(fallback), or monadic Map/AndThen",
							posStr(fset, node.Pos()), relPkg)
					}
				}
			}
		case *ast.ExprStmt:
			exprType := info.TypeOf(node.X)
			if isResultType(exprType) {
				t.Errorf("%s (%s): unhandled Result[T]; discarding Result without checking or extracting value violates safety",
					posStr(fset, node.Pos()), relPkg)
			}
		case *ast.IfStmt:
			// Check for manual "if err != nil { return types.Err[...](err) }" boilerplate
			// in functions returning Result[T]:
			if bin, ok := node.Cond.(*ast.BinaryExpr); ok && bin.Op == token.NEQ {
				if isNilIdent(bin.X) || isNilIdent(bin.Y) {
					if len(node.Body.List) == 1 {
						if ret, ok := node.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
							if isResultType(info.TypeOf(ret.Results[0])) {
								t.Errorf("%s (%s): manual 'if err != nil { return Err(err) }' detected returning Result[T]; use monadic types.AndThen (FlatMap) instead to eliminate boilerplate",
									posStr(fset, node.Pos()), relPkg)
							}
						}
					}
				}
			}
		}
		return true
	})
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isResultType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	for {
		if ptr, ok := typ.(*types.Pointer); ok {
			typ = ptr.Elem()
			continue
		}
		break
	}
	named, ok := typ.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == "github.com/RNG999/antic/internal/types" && obj.Name() == "Result"
}

func TestHostPrimitiveObsessionSafety(t *testing.T) {
	root := repoRoot(t)
	imp := newRepoImporter(t, root)

	for _, relPkg := range []string{"internal/host/instance", "internal/host/share", "internal/host/guest"} {
		t.Run(relPkg, func(t *testing.T) {
			fset := token.NewFileSet()
			files := parseNonTestFiles(t, fset, filepath.Join(root, relPkg))
			info := &types.Info{
				Types: make(map[ast.Expr]types.TypeAndValue),
				Defs:  make(map[*ast.Ident]types.Object),
				Uses:  make(map[*ast.Ident]types.Object),
			}
			conf := types.Config{Importer: imp, Error: func(error) {}}
			pkg, err := conf.Check(modulePrefix+"/"+relPkg, fset, files, info)
			if err != nil {
				t.Fatal(err)
			}
			checkDynamicPrimitiveObsession(t, fset, relPkg, files, info, pkg.Scope())
		})
	}

	t.Run("ViolationsDetected", func(t *testing.T) {
		// Use arbitrary, non-hardcoded struct/function/method/closure names to prove universal
		// dynamic detection across exported and unexported functions, methods, closures (*ast.FuncLit),
		// unchecked Phantom casts (types.Phantom[string, Tag]), 'any' parameters, and vague names
		// (resolve*, *Helper*).
		const snippet = `package bad
import (
	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)
type ExecutablePathTag struct{}
type ExecutablePath = types.Phantom[string, ExecutablePathTag]
func NewExecutablePath(p string) ExecutablePath {
	return types.NewPhantom[string, ExecutablePathTag](p)
}
func Textf(format string, args ...any) domain.Text {
	return domain.NewText(format)
}
type ArbitraryRecord struct {
	GuestLabel string
	TargetDir  string
	ExtraRoots []string
}
func MountCustomShare(path string, count int) string { return "" }
func (ArbitraryRecord) LookupByGuest(name string) (string, error) { return "", nil }
func (ArbitraryRecord) startNet() (string, error) { return "", nil }
func resolveHelperBinary(name string) domain.AbsPath {
	find := func(bin string, extra ...string) domain.AbsPath { return domain.AbsPath{} }
	return find(name)
}
func readLockPID() string { return "" }
func journalPath(base domain.AbsPath) string { return base.Get() }
`
		testFset := token.NewFileSet()
		f, err := parser.ParseFile(testFset, "bad.go", snippet, 0)
		if err != nil {
			t.Fatal(err)
		}
		info := &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		pkg, err := conf.Check("bad", testFset, []*ast.File{f}, info)
		if err != nil {
			t.Fatal(err)
		}
		var errs []string
		mockT := &recordingT{errs: &errs}
		checkDynamicPrimitiveObsession(mockT, testFset, "bad", []*ast.File{f}, info, pkg.Scope())

		if len(errs) < 18 {
			t.Fatalf("expected at least 18 dynamic primitive obsession errors, got %d: %v", len(errs), errs)
		}
	})
}

var bannedGenericHostFuncTokens = map[string]bool{
	"resolve": true,
	"helper":  true,
	"helpers": true,
	"util":    true,
	"utils":   true,
}

func checkExplicitFuncNaming(t errorReporter, fset *token.FileSet, relPkg, qualName, shortName string, pos token.Pos) {
	for _, tok := range splitCamelCaseTokens(shortName) {
		if bannedGenericHostFuncTokens[tok] {
			t.Errorf("%s (%s): %s uses banned generic/vague word %q; use an explicit domain verb/noun",
				posStr(fset, pos), relPkg, qualName, tok)
		}
	}
}

func checkStructFieldsPrimitives(t errorReporter, fset *token.FileSet, relPkg, structName string, st *types.Struct) {
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if reflect.StructTag(st.Tag(i)).Get("json") != "" {
			continue
		}
		if anonSt, ok := field.Type().Underlying().(*types.Struct); ok {
			if _, isNamed := types.Unalias(field.Type()).(*types.Named); !isNamed {
				checkStructFieldsPrimitives(t, fset, relPkg, structName+"."+field.Name(), anonSt)
				continue
			}
		}
		if containsBarePrimitive(field.Type()) {
			t.Errorf("%s (%s): struct %s field %s has bare primitive or unvalidated type %s; must use a domain refinement type (types.Refined[T, P]) or structured domain type",
				posStr(fset, field.Pos()), relPkg, structName, field.Name(), field.Type().String())
		}
	}
}

// checkDynamicPrimitiveObsession dynamically enforces refinement types (types.Refined[T, P])
// and structured domain types across a host package without hardcoded function exemptions:
//  1. Universal Surface Check: Every struct field (except JSON wire/disk DTO fields), every
//     exported and unexported function parameter/result, every exported and unexported method
//     parameter/result, and every local helper closure (*ast.FuncLit) must not use bare primitives
//     (string, int, etc.), 'any' (interface{}), or unvalidated Phantom wrappers over primitives
//     (types.Phantom[string, Tag]).
//  2. Explicit Identifier Naming: Forbids generic/vague words ('resolve', 'helper', 'util') in
//     function and method names.
//  3. Semantic Identifier-Type & Return-Type Check: Ensures identifier names and return values
//     match their domain refinement predicates and forbids early '.Get()' unboxing in return statements.
func checkDynamicPrimitiveObsession(t errorReporter, fset *token.FileSet, relPkg string, files []*ast.File, info *types.Info, scope *types.Scope) {
	// 1. Universal Surface Check over go/types Scope (both exported AND unexported!)
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		switch o := obj.(type) {
		case *types.TypeName:
			if o.IsAlias() {
				continue
			}
			named, ok := types.Unalias(o.Type()).(*types.Named)
			if !ok {
				continue
			}
			if st, ok := named.Underlying().(*types.Struct); ok {
				checkStructFieldsPrimitives(t, fset, relPkg, o.Name(), st)
			}
			for i := 0; i < named.NumMethods(); i++ {
				m := named.Method(i)
				checkExplicitFuncNaming(t, fset, relPkg, o.Name()+"."+m.Name(), m.Name(), m.Pos())
				if sig, ok := m.Type().(*types.Signature); ok {
					checkSignaturePrimitives(t, fset, relPkg, o.Name()+"."+m.Name(), sig, m.Pos())
				}
			}
		case *types.Func:
			checkExplicitFuncNaming(t, fset, relPkg, o.Name(), o.Name(), o.Pos())
			if sig, ok := o.Type().(*types.Signature); ok {
				checkSignaturePrimitives(t, fset, relPkg, o.Name(), sig, o.Pos())
			}
		}
	}

	// 2. Local helper closure (*ast.FuncLit) inspection & Semantic Identifier-Type Check over AST
	for _, f := range files {
		callbackLits := make(map[*ast.FuncLit]bool)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.FuncLit); ok {
						callbackLits[lit] = true
					}
				}
			}
			return true
		})

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok || callbackLits[lit] {
				return true
			}
			if tv, ok := info.Types[lit]; ok {
				if sig, ok := tv.Type.(*types.Signature); ok {
					checkSignaturePrimitives(t, fset, relPkg, "closure", sig, lit.Pos())
				}
			}
			return true
		})

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				fnName := d.Name.Name
				if d.Type.Params != nil {
					for _, field := range d.Type.Params.List {
						for _, id := range field.Names {
							if obj := info.Defs[id]; obj != nil {
								checkSemanticIdentifierType(t, fset, relPkg, "func "+fnName+" param "+id.Name, id.Name, obj.Type(), id.Pos())
							}
						}
					}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						continue
					}
					for _, field := range st.Fields.List {
						if field.Tag != nil && reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json") != "" {
							continue
						}
						for _, id := range field.Names {
							if obj := info.Defs[id]; obj != nil {
								checkSemanticIdentifierType(t, fset, relPkg, "struct "+ts.Name.Name+" field "+id.Name, id.Name, obj.Type(), id.Pos())
							}
						}
					}
				}
			}
		}
	}

	// 3. Semantic Function Return Type Inference & Late-Unboxing Check
	checkSemanticFuncResultsAndEarlyUnboxing(t, fset, relPkg, files, info)
}

func checkSemanticFuncResultsAndEarlyUnboxing(t errorReporter, fset *token.FileSet, relPkg string, files []*ast.File, info *types.Info) {
	for _, f := range files {
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			fnName := d.Name.Name
			if fnObj, ok := info.Defs[d.Name].(*types.Func); ok {
				if sig, ok := fnObj.Type().(*types.Signature); ok {
					checkSemanticFuncResult(t, fset, relPkg, fnName, sig, d.Name.Pos())
				}
			}
			if d.Body != nil {
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if _, isFuncLit := n.(*ast.FuncLit); isFuncLit {
						return false
					}
					ret, ok := n.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					for _, resExpr := range ret.Results {
						call, ok := resExpr.(*ast.CallExpr)
						if !ok {
							continue
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Get" {
							continue
						}
						if tv, ok := info.Types[sel.X]; ok && isAnyRefinedType(tv.Type) {
							t.Errorf("%s (%s): func %s performs early unboxing via 'return ...Get()' on refinement type %s; return the Refined type directly and unbox only at the OS syscall boundary",
								posStr(fset, ret.Pos()), relPkg, fnName, tv.Type.String())
						}
					}
					return true
				})
			}
		}
	}
}

func checkSemanticFuncResult(t errorReporter, fset *token.FileSet, relPkg, fnName string, sig *types.Signature, pos token.Pos) {
	if sig.Results().Len() == 0 {
		return
	}
	r0 := sig.Results().At(0).Type()
	// Action functions that only return error (e.g. TrashConfig(name, path) error) are commands, not getters.
	if sig.Results().Len() == 1 && r0.String() == "error" {
		return
	}
	expectedPred := semanticPredicateForFuncResultName(fnName)
	if expectedPred == "" {
		return
	}
	if !matchesRefinedOrWrapped(r0, expectedPred) {
		t.Errorf("%s (%s): func %s name implies returning %s, but result 0 has type %s",
			posStr(fset, pos), relPkg, fnName, expectedPred, r0.String())
	}
}

func semanticPredicateForFuncResultName(fnName string) string {
	tokens := splitCamelCaseTokens(fnName)
	if len(tokens) == 0 {
		return ""
	}
	// Action/predicate verbs whose primary return value is a status/result/boolean rather than the object named in the suffix
	switch tokens[0] {
	case "add", "remove", "trash", "restore", "write", "ensure", "start", "stop", "lock", "recover", "check", "within", "bare", "in", "has", "is":
		return ""
	}
	last := tokens[len(tokens)-1]
	switch last {
	case "path", "paths", "dir", "dirs", "home", "root", "roots", "file", "console", "libexec":
		return "AbsPathPred"
	case "pid", "pids":
		return "ProcessIDPred"
	case "id", "ids":
		return "InstanceIDPred"
	case "tag", "tags":
		return "ShareTagPred"
	}
	if len(tokens) >= 2 && tokens[len(tokens)-2] == "guest" && last == "name" {
		return "GuestNamePred"
	}
	return ""
}

func matchesRefinedOrWrapped(typ types.Type, expectedPred string) bool {
	if isRefinedTypeWithPredicate(typ, expectedPred) || isSliceOfRefinedType(typ, expectedPred) {
		return true
	}
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	if named.Obj().Pkg().Path() == modulePrefix+"/internal/types" && (named.Obj().Name() == "Option" || named.Obj().Name() == "Result") {
		if named.TypeArgs() != nil && named.TypeArgs().Len() >= 1 {
			return isRefinedTypeWithPredicate(named.TypeArgs().At(0), expectedPred)
		}
	}
	return false
}

func checkSignaturePrimitives(t errorReporter, fset *token.FileSet, relPkg, qualName string, sig *types.Signature, fallbackPos token.Pos) {
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		pos := p.Pos()
		if !pos.IsValid() {
			pos = fallbackPos
		}
		if containsBarePrimitive(p.Type()) {
			t.Errorf("%s (%s): %s parameter %q has bare primitive or unvalidated type %s; must accept a domain refinement type or structured domain type",
				posStr(fset, pos), relPkg, qualName, p.Name(), p.Type().String())
		}
	}
	for i := 0; i < sig.Results().Len(); i++ {
		res := sig.Results().At(i)
		pos := res.Pos()
		if !pos.IsValid() {
			pos = fallbackPos
		}
		if containsBarePrimitive(res.Type()) {
			t.Errorf("%s (%s): %s result %d has bare primitive or unvalidated type %s; must return a domain refinement type or structured domain type",
				posStr(fset, pos), relPkg, qualName, i, res.Type().String())
		}
	}
}

func checkSemanticIdentifierType(t errorReporter, fset *token.FileSet, relPkg, contextDesc, identName string, typ types.Type, pos token.Pos) {
	if !isAnyRefinedType(typ) && !isSliceOfAnyRefinedType(typ) {
		if _, isStruct := typ.Underlying().(*types.Struct); isStruct {
			return
		}
	}
	expectedPred := semanticPredicateForIdentifier(identName)
	if expectedPred == "" {
		return
	}
	if !isRefinedTypeWithPredicate(typ, expectedPred) && !isSliceOfRefinedType(typ, expectedPred) {
		t.Errorf("%s (%s): %s implies domain concept requiring %s, but has type %s",
			posStr(fset, pos), relPkg, contextDesc, expectedPred, typ.String())
	}
}

func semanticPredicateForIdentifier(ident string) string {
	tokens := splitCamelCaseTokens(ident)
	if len(tokens) == 0 {
		return ""
	}
	last := tokens[len(tokens)-1]
	switch last {
	case "path", "paths", "dir", "dirs", "home", "root", "roots", "file", "console", "libexec", "src", "dst":
		return "AbsPathPred"
	case "pid", "pids":
		return "ProcessIDPred"
	case "name", "names":
		if len(tokens) >= 2 && tokens[len(tokens)-2] == "unit" {
			return "UnitNamePred"
		}
		return "GuestNamePred"
	case "id", "ids":
		return "InstanceIDPred"
	case "tag", "tags":
		return "ShareTagPred"
	}
	return ""
}

func splitCamelCaseTokens(s string) []string {
	var tokens []string
	start := 0
	runes := []rune(s)
	for i := 1; i < len(runes); i++ {
		prev := runes[i-1]
		curr := runes[i]
		nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if (unicode.IsLower(prev) && unicode.IsUpper(curr)) || (unicode.IsUpper(prev) && unicode.IsUpper(curr) && nextIsLower) {
			tokens = append(tokens, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	if start < len(runes) {
		tokens = append(tokens, strings.ToLower(string(runes[start:])))
	}
	return tokens
}

func containsBarePrimitive(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if isAnyRefinedType(typ) {
		return false
	}
	unaliased := types.Unalias(typ)
	if named, ok := unaliased.(*types.Named); ok {
		// Unvalidated Phantom[T, Tag] over a primitive is treated transparently as a bare primitive
		// so that fake casts like NewExecutablePath(p string) ExecutablePath or domain.Text are rejected.
		if obj := named.Obj(); obj != nil && obj.Pkg() != nil &&
			obj.Pkg().Path() == modulePrefix+"/internal/types" && obj.Name() == "Phantom" &&
			named.TypeArgs() != nil && named.TypeArgs().Len() >= 1 {
			return containsBarePrimitive(named.TypeArgs().At(0))
		}
		return false
	}
	switch u := unaliased.(type) {
	case *types.Basic:
		// bool is an algebraic two-valued sum (2 = 1 + 1); string and numeric primitives are bare primitives.
		return u.Kind() != types.Bool && u.Kind() != types.Invalid
	case *types.Interface:
		// Ban 'any' / 'interface{}' (empty interface) as an untyped escape hatch.
		return u.NumMethods() == 0
	case *types.Slice:
		if basic, ok := types.Unalias(u.Elem()).(*types.Basic); ok && basic.Kind() == types.Byte {
			return false
		}
		return containsBarePrimitive(u.Elem())
	case *types.Array:
		if basic, ok := types.Unalias(u.Elem()).(*types.Basic); ok && basic.Kind() == types.Byte {
			return false
		}
		return containsBarePrimitive(u.Elem())
	case *types.Pointer:
		return containsBarePrimitive(u.Elem())
	case *types.Map:
		return containsBarePrimitive(u.Key()) || containsBarePrimitive(u.Elem())
	case *types.Signature:
		for i := 0; i < u.Params().Len(); i++ {
			if containsBarePrimitive(u.Params().At(i).Type()) {
				return true
			}
		}
		for i := 0; i < u.Results().Len(); i++ {
			if containsBarePrimitive(u.Results().At(i).Type()) {
				return true
			}
		}
		return false
	}
	return false
}

func isAnyRefinedType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == modulePrefix+"/internal/types" && obj.Name() == "Refined"
}

func isSliceOfAnyRefinedType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	slice, ok := types.Unalias(typ).(*types.Slice)
	if !ok {
		return false
	}
	return isAnyRefinedType(slice.Elem())
}

func isRefinedTypeWithPredicate(typ types.Type, expectedPredName string) bool {
	if !isAnyRefinedType(typ) {
		return false
	}
	named := types.Unalias(typ).(*types.Named)
	if named.TypeArgs() == nil || named.TypeArgs().Len() < 2 {
		return false
	}
	predType := types.Unalias(named.TypeArgs().At(1))
	predNamed, ok := predType.(*types.Named)
	if !ok {
		return false
	}
	return predNamed.Obj().Name() == expectedPredName
}

func isSliceOfRefinedType(typ types.Type, expectedPredName string) bool {
	if typ == nil {
		return false
	}
	slice, ok := types.Unalias(typ).(*types.Slice)
	if !ok {
		return false
	}
	return isRefinedTypeWithPredicate(slice.Elem(), expectedPredName)
}

var (
	mdLinkRE     = regexp.MustCompile(`\]\(([^)#\s]+)\)`)
	inlinePathRE = regexp.MustCompile("`((?:cmd|internal|formal|examples|nix|e2e|just|skills|docs)/[A-Za-z0-9_./-]+)`")
)

// TestDocsCodeConsistency verifies that documentation files stay synchronized with the codebase:
//  1. Every relative file/directory link and inline repository path in README.md, formal/README.md,
//     docs/design/spec.md, and skills/antic/SKILL.md resolves to an existing path in the repository.
//  2. skills/antic/SKILL.md and internal/host/cli/skill/SKILL.md are identical and accurately
//     reflect optional [<name>] resolution when a single guest is defined.
//  3. No Go file or doc references obsolete subcommands ("antic share add", "antic pkg ", "antic port ").
func TestDocsCodeConsistency(t *testing.T) {
	root := repoRoot(t)
	docFiles := []string{
		"README.md",
		"formal/README.md",
		"docs/design/spec.md",
		"skills/antic/SKILL.md",
		"internal/host/cli/skill/SKILL.md",
	}

	for _, relDoc := range docFiles {
		absDoc := filepath.Join(root, relDoc)
		contentBytes, err := os.ReadFile(absDoc)
		if err != nil {
			t.Fatalf("missing documentation file %s: %v", relDoc, err)
		}
		content := string(contentBytes)
		docDir := filepath.Dir(absDoc)

		for _, m := range mdLinkRE.FindAllStringSubmatch(content, -1) {
			link := m[1]
			if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			target := filepath.Clean(filepath.Join(docDir, link))
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: broken markdown link %q (resolved to %s)", relDoc, link, target)
			}
		}

		for lineNum, line := range strings.Split(content, "\n") {
			if strings.Contains(line, "削除") || strings.Contains(line, "廃止") {
				continue
			}
			for _, m := range inlinePathRE.FindAllStringSubmatch(line, -1) {
				relPath := strings.TrimSuffix(m[1], "/")
				if strings.Contains(relPath, "seed-N.txtar") || strings.Contains(relPath, "*") {
					continue
				}
				// Allow package.Symbol references like internal/tea/egress.Rules by stripping a trailing .Ident
				target := filepath.Join(root, relPath)
				if _, err := os.Stat(target); err != nil {
					if idx := strings.LastIndex(relPath, "."); idx > 0 {
						pkgCandidate := filepath.Join(root, relPath[:idx])
						if fi, statErr := os.Stat(pkgCandidate); statErr == nil && fi.IsDir() {
							continue
						}
					}
					t.Errorf("%s:%d: referenced repository path %q does not exist", relDoc, lineNum+1, relPath)
				}
			}
		}
	}

	skillA, errA := os.ReadFile(filepath.Join(root, "skills/antic/SKILL.md"))
	skillB, errB := os.ReadFile(filepath.Join(root, "internal/host/cli/skill/SKILL.md"))
	if errA != nil || errB != nil {
		t.Fatalf("reading SKILL.md files: %v / %v", errA, errB)
	}
	if string(skillA) != string(skillB) {
		t.Errorf("skills/antic/SKILL.md and internal/host/cli/skill/SKILL.md are out of sync")
	}
	if strings.Contains(string(skillA), "Every guest-targeted command requires the explicit guest <name>") {
		t.Errorf("SKILL.md contradicts resolveTargetGuest in internal/host/cli/cli.go (single-guest [<name>] is optional)")
	}

	obsoleteCmds := []string{"antic share add", "antic share rm", "antic pkg ", "antic port "}
	allPkgs := append(discoverPackages(t, root, "cmd"), discoverPackages(t, root, "internal")...)
	for _, relPkg := range allPkgs {
		if relPkg == "internal/arch" {
			continue
		}
		pkgDir := filepath.Join(root, relPkg)
		entries, _ := os.ReadDir(pkgDir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(pkgDir, e.Name()))
			if err != nil {
				continue
			}
			text := string(b)
			for _, obs := range obsoleteCmds {
				if strings.Contains(text, obs) {
					t.Errorf("%s/%s: references obsolete CLI command %q", relPkg, e.Name(), obs)
				}
			}
		}
	}
}
