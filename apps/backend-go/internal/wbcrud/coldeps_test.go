package wbcrud

import (
	"reflect"
	"sort"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

func TestColumnVectorsMatchPythonDeps(t *testing.T) {
	v := loadVectors(t)
	if len(v.Deps) == 0 || len(v.Renames) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Deps {
		t.Run("deps/"+c.Name, func(t *testing.T) {
			cols := rawColumns(t, c.Columns)
			for i, col := range cols {
				got := colgraph.Refs(graphCols([]Column{col})[0]) // sorted
				if !reflect.DeepEqual(got, append([]string{}, c.Refs[i]...)) {
					t.Errorf("refs[%d] = %v, want %v", i, got, c.Refs[i])
				}
				if g := colIDs(ReferencingColumns(cols, col)); !sameJSON(t, g, c.Referencing[i]) {
					t.Errorf("referencing[%d] = %v, want %v", i, g, c.Referencing[i])
				}
			}
			blocked := []string{}
			for id := range CycleBlockedColumns(cols) {
				blocked = append(blocked, id)
			}
			sort.Strings(blocked)
			if !reflect.DeepEqual(blocked, append([]string{}, c.Blocked...)) {
				t.Errorf("blocked = %v, want %v", blocked, c.Blocked)
			}
		})
	}
	for _, c := range v.Renames {
		t.Run("rename/"+c.Name, func(t *testing.T) {
			before, after := rawColumns(t, c.Before), rawColumns(t, c.After)
			if g := colIDs(BrokenRenameReferences(before, after)); !sameJSON(t, g, c.Broken) {
				t.Errorf("broken = %v, want %v", g, c.Broken)
			}
			checkGuard(t, ValidateRenames(before, after), c.Guard)
		})
	}
}

func TestCycleErrorSortedIDs(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Cycles {
		t.Run(c.Name, func(t *testing.T) {
			checkGuard(t, ValidateNewCycles(rawColumns(t, c.Before), rawColumns(t, c.After)), c.Guard)
		})
	}
}

func checkGuard(t *testing.T, got *GuardError, want *guardVec) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("unexpected guard %+v", got)
		}
		return
	}
	if got == nil {
		t.Fatalf("want guard %+v, got nil", want)
	}
	if got.Status != want.Status || got.Detail != want.Detail {
		t.Errorf("guard = %d %q, want %d %q", got.Status, got.Detail, want.Status, want.Detail)
	}
}
