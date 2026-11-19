//go:build integration

package workbook_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// A re-run that ends in an error or a miss must not erase a value the cell already had: the old value stays and the
// failed attempt is recorded in the cell's metadata. A re-run that returns a new value replaces it.
func TestRerunKeepsValueOnFailureAndReplacesOnNewValue(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-rerun", "["+colJSON("c1", "p1")+"]", 1, 10)
	mode := "value1"
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "value1":
			fmt.Fprint(w, `{"v":"1999"}`)
		case "value2":
			fmt.Fprint(w, `{"v":"2024"}`)
		case "miss":
			fmt.Fprint(w, `{}`)
		case "error":
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	run := func() {
		t.Helper()
		c := e.claim(wsA, payloadFor(wsA, "wb-rerun", map[string]float64{"p1": 0.05}, nil))
		if _, err := workbook.Run(e.ctx, e.deps(s, nil), c); err != nil {
			t.Fatal(err)
		}
	}
	val := func() string {
		return e.str(wsA, `SELECT value FROM workbook_enrichments WHERE workbook_id='wb-rerun'`)
	}
	status := func() string {
		return e.str(wsA, `SELECT status FROM workbook_enrichments WHERE workbook_id='wb-rerun'`)
	}
	overlay := func() string {
		return e.str(wsA, `SELECT enrichments::jsonb->'c1'->>'value' FROM workbook_rows WHERE workbook_id='wb-rerun'`)
	}

	run()
	if val() != "1999" || status() != "complete" {
		t.Fatalf("first run: value %q status %q", val(), status())
	}

	for _, m := range []string{"miss", "error"} {
		mode = m
		run()
		if val() != "1999" || status() != "complete" || overlay() != "1999" {
			t.Fatalf("%s re-run erased the value: value %q status %q overlay %q", m, val(), status(), overlay())
		}
		meta := e.str(wsA, `SELECT cell_metadata::text FROM workbook_enrichments WHERE workbook_id='wb-rerun'`)
		if !strings.Contains(meta, "last_failed_attempt") {
			t.Fatalf("%s re-run: the failed attempt is not recorded in the cell metadata: %q", m, meta)
		}
	}

	mode = "value2"
	run()
	if val() != "2024" || status() != "complete" || overlay() != "2024" {
		t.Fatalf("a re-run that returns a new value must replace it: value %q overlay %q", val(), overlay())
	}
	if meta := e.str(wsA, `SELECT COALESCE(cell_metadata::text,'') FROM workbook_enrichments WHERE workbook_id='wb-rerun'`); strings.Contains(meta, "last_failed_attempt") {
		t.Fatalf("a fresh value must clear the stale failed-attempt note: %q", meta)
	}
}
