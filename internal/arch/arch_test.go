// Package arch enforces architectural boundaries, Sans-I/O & The Elm Architecture
// (TEA) formal-verification constraints, Phantom Types, Refinement Types (篩型),
// Single Source of Truth (SSOT) state normalization, and strict identifier
// naming / export visibility conventions across the antic repository.
package arch

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
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
		"adt.go":        {"Option": true, "NonEmpty": true},
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
	for _, name := range []string{"Refined", "Phantom", "Indexed", "Proof", "Option", "NonEmpty", "SortedSet"} {
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
						if ! ts.Assign.IsValid() && (strings.HasSuffix(name, "Tag") || strings.HasSuffix(name, "State") || strings.HasSuffix(name, "Prop")) {
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
						if !strings.HasPrefix(name, "New") {
							t.Errorf("%s: exported function %s in internal/domain/tags.go must be a Phantom constructor (New*)",
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

		isPureSpec := strings.HasPrefix(relPkg, "internal/tea/") || relPkg == "internal/proto" || relPkg == "internal/types" || relPkg == "internal/domain"
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
	t        *testing.T
	root     string
	fallback types.Importer
	cache    map[string]*types.Package
}

func newRepoImporter(t *testing.T, root string) *repoImporter {
	return &repoImporter{
		t:        t,
		root:     root,
		fallback: importer.Default(),
		cache:    make(map[string]*types.Package),
	}
}

func (imp *repoImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := imp.cache[path]; ok {
		return pkg, nil
	}
	if strings.HasPrefix(path, modulePrefix+"/") {
		rel := strings.TrimPrefix(path, modulePrefix+"/")
		fset := token.NewFileSet()
		files := parseNonTestFiles(imp.t, fset, filepath.Join(imp.root, rel))
		conf := types.Config{Importer: imp}
		pkg, err := conf.Check(path, fset, files, nil)
		if err != nil {
			return nil, err
		}
		imp.cache[path] = pkg
		return pkg, nil
	}
	pkg, err := imp.fallback.Import(path)
	if err == nil {
		imp.cache[path] = pkg
	}
	return pkg, err
}
