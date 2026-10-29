package govadmin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRolesForIsCatalogOwned(t *testing.T) {
	if got := RolesFor("outreach.write"); !slices.Equal(got, []string{"admin"}) {
		t.Fatalf("outreach.write: %v", got)
	}
	if got := RolesFor("tables.write"); !slices.Equal(got, []string{"editor", "admin"}) {
		t.Fatalf("tables.write: %v", got)
	}
	if RolesFor("nope") != nil {
		t.Fatal("unknown permission must have no roles")
	}
	// Callers cannot mutate the catalog through the returned slice.
	r := RolesFor("tables.write")
	r[0] = "viewer"
	if Permissions[0].DefaultRoles[0] != "editor" {
		t.Fatal("RolesFor leaked the catalog slice")
	}
	if Gate("tables.write") == nil {
		t.Fatal("nil gate")
	}
}

// Every identity.RequireRole(<literal permission>, <literal roles>...) call in
// internal/api must use exactly the catalog's roles for that permission
// (order-insensitive): the catalog is the single source of truth.
func TestAPIGatesMatchCatalog(t *testing.T) {
	files, err := filepath.Glob("../api/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no api files: %v", err)
	}
	checked := 0
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RequireRole" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "identity" {
				return true
			}
			var args []string
			for _, a := range call.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true // non-literal gate (variable): not pinnable here
				}
				s, _ := strconv.Unquote(lit.Value)
				args = append(args, s)
			}
			if len(args) == 0 || args[0] == "" {
				return true // permission-less role gate
			}
			perm, ok := lookup(args[0])
			if !ok {
				t.Errorf("%s: gate permission %q is not in the catalog", fset.Position(call.Pos()), args[0])
				return true
			}
			got := slices.Clone(args[1:])
			want := slices.Clone(perm.DefaultRoles)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: RequireRole(%q) roles %v != catalog %v", fset.Position(call.Pos()), args[0], got, want)
			}
			checked++
			return true
		})
	}
	if checked < 10 {
		t.Fatalf("scanner found only %d call sites; pattern broken", checked)
	}
}
