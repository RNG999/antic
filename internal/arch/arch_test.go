// Package arch enforces architectural boundaries, Sans-I/O & The Elm Architecture
// (TEA) formal-verification constraints, and strict identifier naming / export
// visibility conventions across the antic repository.
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
//	Rank 0 (pure):         internal/proto, internal/tea/*
//	Rank 1 (domain):       internal/host/config, internal/host/share
//	Rank 2 (infra):        internal/host/chv, internal/host/systemd, internal/host/transport,
//	                       internal/host/egress, internal/host/instance, internal/host/nixbuild
//	Rank 3 (orchestrator): internal/host/guest
//	Rank 4 (cli):          internal/host/cli
func packageLayerRank(relPkg string) (domain string, rank int, ok bool) {
	switch {
	case relPkg == "internal/proto" || strings.HasPrefix(relPkg, "internal/tea/"):
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
	case relPkg == "internal/guest/sd" || relPkg == "internal/guest/session":
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

	for _, relPkg := range teaPkgs {
		t.Run(relPkg, func(t *testing.T) {
			checkTEAPackage(t, root, relPkg)
		})
	}
}

func checkTEAPackage(t *testing.T, root, relPkg string) {
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
				if targetRel != "internal/proto" && !strings.HasPrefix(targetRel, "internal/tea/") {
					t.Errorf("%s: forbidden internal import %q in pure TEA package", posStr(fset, imp.Pos()), impPath)
				}
				continue
			}
			if !allowedPureStdlib[impPath] {
				t.Errorf("%s: forbidden import %q in pure TEA package (allowed stdlib: %v)",
					posStr(fset, imp.Pos()), impPath, allowedKeys(allowedPureStdlib))
			}
		}
	}

	// 2. Type-check the TEA package to inspect types, expressions, and contracts.
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	conf := types.Config{Importer: importer.Default()}
	typedPkg, err := conf.Check(modulePrefix+"/"+relPkg, fset, files, info)
	if err != nil {
		t.Fatalf("%s: typecheck failed: %v", relPkg, err)
	}

	// 3. AST & type-based Sans-I/O and Formal Methods checks:
	//    - No package-level var declarations
	//    - No init() functions
	//    - No pointer receivers (value receivers only)
	//    - No unbounded 'for' loops (only bounded 'for ... range' over slices/arrays allowed for termination)
	//    - No go / chan / select
	//    - No panic(...) calls
	//    - No time.Now / time.Since / time.Until / time.Sleep
	//    - No for ... range over map (randomized iteration order in Go)
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok == token.VAR {
					t.Errorf("%s: package-level 'var' declaration is forbidden in pure TEA packages (use 'const' or Model fields)",
						posStr(fset, d.Pos()))
				}
			case *ast.FuncDecl:
				if d.Name.Name == "init" && d.Recv == nil {
					t.Errorf("%s: init() function is forbidden in pure TEA packages", posStr(fset, d.Pos()))
				}
				if d.Recv != nil {
					for _, field := range d.Recv.List {
						if _, isPtr := field.Type.(*ast.StarExpr); isPtr {
							t.Errorf("%s: pointer receiver on %s is forbidden in pure TEA packages (use value receivers)",
								posStr(fset, field.Pos()), d.Name.Name)
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
				// Unbounded or condition-based for loops make termination non-trivial for formal verification;
				// require bounded range loops over slices/arrays.
				t.Errorf("%s: general 'for' loop is forbidden in pure TEA packages (use bounded 'for ... range' over a slice to guarantee termination)",
					posStr(fset, stmt.Pos()))
			case *ast.GoStmt:
				t.Errorf("%s: 'go' statement is forbidden in pure TEA packages", posStr(fset, stmt.Pos()))
			case *ast.ChanType:
				t.Errorf("%s: 'chan' type is forbidden in pure TEA packages", posStr(fset, stmt.Pos()))
			case *ast.SelectStmt:
				t.Errorf("%s: 'select' statement is forbidden in pure TEA packages", posStr(fset, stmt.Pos()))
			case *ast.RangeStmt:
				if tv, ok := info.Types[stmt.X]; ok {
					if _, isMap := tv.Type.Underlying().(*types.Map); isMap {
						t.Errorf("%s: ranging over map (%s) is forbidden in pure TEA packages because Go randomizes map iteration order",
							posStr(fset, stmt.Pos()), tv.Type)
					}
				}
			case *ast.CallExpr:
				if id, ok := stmt.Fun.(*ast.Ident); ok && id.Name == "panic" {
					t.Errorf("%s: panic() is forbidden in pure TEA packages (transitions must be total)", posStr(fset, stmt.Pos()))
				}
				if sel, ok := stmt.Fun.(*ast.SelectorExpr); ok {
					if xid, ok := sel.X.(*ast.Ident); ok && xid.Name == "time" {
						switch sel.Sel.Name {
						case "Now", "Since", "Until", "Sleep", "After", "Tick":
							t.Errorf("%s: time.%s is forbidden in pure TEA packages (pass timestamps explicitly via Msg)",
								posStr(fset, stmt.Pos()), sel.Sel.Name)
						}
					}
				}
			}
			return true
		})
	}

	// 4. Verify required TEA + IronFleet/TLA+ symbols:
	//    Model, Msg, Effect, Init, Update, Invariant, StepValid.
	scope := typedPkg.Scope()
	modelObj := scope.Lookup("Model")
	if modelObj == nil {
		t.Fatalf("%s: missing required type 'Model'", relPkg)
	}
	modelStruct, ok := modelObj.Type().Underlying().(*types.Struct)
	if !ok {
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

	// 5. Verify strict SMT-friendly value semantics (no pointers, maps, floats, funcs, chans, or unsealed interfaces)
	//    and naming conventions (Msg* for Msg, Effect* for Effect).
	checkValueSemantics(t, relPkg, "Model", modelStruct, map[types.Type]bool{})

	var msgTypes []string
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
			checkValueSemantics(t, relPkg, tn.Name(), tn.Type(), map[types.Type]bool{})
		}
		if types.Implements(tn.Type(), effIface) {
			if !strings.HasPrefix(tn.Name(), "Effect") {
				t.Errorf("%s: type %s implements Effect and must be named with 'Effect' prefix", relPkg, tn.Name())
			}
			checkValueSemantics(t, relPkg, tn.Name(), tn.Type(), map[types.Type]bool{})
		}
	}

	if len(msgTypes) == 0 {
		t.Errorf("%s: no concrete types implement Msg", relPkg)
	}

	// 6. Verify that Update contains an exhaustive type switch covering every Msg implementation.
	checkExhaustiveMsgSwitchInUpdate(t, fset, files, msgTypes)
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

func checkValueSemantics(t *testing.T, relPkg, rootName string, typ types.Type, seen map[types.Type]bool) {
	t.Helper()
	if seen[typ] {
		return
	}
	seen[typ] = true

	switch u := typ.Underlying().(type) {
	case *types.Basic:
		if u.Info()&(types.IsFloat|types.IsComplex) != 0 {
			t.Errorf("%s: %s contains floating/complex type %s (forbidden in TEA Model/Msg/Effect; breaks reflexive equality and SMT decidability)",
				relPkg, rootName, typ)
		}
		return
	case *types.Slice:
		checkValueSemantics(t, relPkg, rootName, u.Elem(), seen)
	case *types.Array:
		checkValueSemantics(t, relPkg, rootName, u.Elem(), seen)
	case *types.Struct:
		for i := range u.NumFields() {
			f := u.Field(i)
			checkValueSemantics(t, relPkg, rootName+"."+f.Name(), f.Type(), seen)
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

		isTEA := strings.HasPrefix(relPkg, "internal/tea/")
		isProto := relPkg == "internal/proto"
		refsInOtherPkgs := markTransitiveExportedTypes(files, externalRefs[relPkg])

		receiverNames := map[string]string{}           // typeName -> first receiver ident
		receiverPos := make(map[string]token.Position) // typeName -> first pos

		for _, f := range files {
			// Check top-level declarations for dead exports (uppercase symbols only used internally).
			if strings.HasPrefix(relPkg, "internal/") && !isTEA && !isProto {
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
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
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
