package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// staticTemplatesRouter serves the unauthenticated gallery/list routes (no database).
func staticTemplatesRouter() *httpx.Router {
	r := httpx.NewRouter()
	RegisterTemplates(r, Deps{Flags: config.Flags{PGLeadStore: true}})
	return r
}

func replayStatic(t *testing.T, r *httpx.Router, name string) {
	t.Helper()
	fx := contract.Load(t, "templates/"+name)
	req := httptest.NewRequest(fx.Request.Method, fx.Request.Path, nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	if fx.Response.Status == 307 {
		got.Body, fx.Response.Body = nil, nil
		delete(got.Headers, "Content-Type")
		delete(fx.Response.Headers, "content-type")
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
}

func TestGalleryListAndCategoryFilter(t *testing.T) {
	r := staticTemplatesRouter()
	for _, n := range []string{"gallery_all", "gallery_authed", "gallery_cat_empty_param", "gallery_cat_outbound", "gallery_cat_signals"} {
		replayStatic(t, r, n)
	}
}

func TestUnknownCategory404(t *testing.T) {
	replayStatic(t, staticTemplatesRouter(), "gallery_cat_unknown")
}

func TestRecipeDetailMatchesFixture(t *testing.T) {
	r := staticTemplatesRouter()
	for _, n := range []string{"recipe_full_founder_outreach", "recipe_hiring_signals", "recipe_webhook_export", "recipe_missing"} {
		replayStatic(t, r, n)
	}
}

func TestLegacyTemplateListMatchesFixture(t *testing.T) {
	r := staticTemplatesRouter()
	for _, n := range []string{"templates_all", "templates_cat_sales", "templates_cat_signals", "templates_cat_unknown", "templates_trailing_slash"} {
		replayStatic(t, r, n)
	}
}

func TestGalleryLiteralsBeatTemplateID(t *testing.T) {
	r := staticTemplatesRouter()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/templates/gallery", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"recipes"`) {
		t.Fatalf("gallery: %d %.100s", rec.Code, rec.Body.String())
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestBuildTemplateColumnsGuards(t *testing.T) {
	base := func() wbregistry.Template {
		return wbregistry.Template{
			ID: "x", Name: "X", Description: "d",
			Columns: raw(`[{"key":"company","name":"Company","type":"text"}]`),
		}
	}
	cases := []struct {
		name   string
		mod    func(*wbregistry.Template)
		detail string
	}{
		{"missing enrichment id", func(tp *wbregistry.Template) {
			tp.EnrichmentColumns = raw(`[{"name":"E","type":"waterfall"}]`)
		}, "Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook."},
		{"blank enrichment id", func(tp *wbregistry.Template) {
			tp.EnrichmentColumns = raw(`[{"id":"  ","name":"E","type":"waterfall"}]`)
		}, "Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook."},
		{"enrichment id clashes with field", func(tp *wbregistry.Template) {
			tp.EnrichmentColumns = raw(`[{"id":"company","name":"E","type":"waterfall"}]`)
		}, "Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook."},
		{"duplicate enrichment ids", func(tp *wbregistry.Template) {
			tp.EnrichmentColumns = raw(`[{"id":"e","name":"E","type":"waterfall"},{"id":"e","name":"F","type":"waterfall"}]`)
		}, "Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook."},
		{"duplicate field keys", func(tp *wbregistry.Template) {
			tp.Columns = raw(`[{"key":"company","name":"A","type":"text"},{"key":"company","name":"B","type":"text"}]`)
		}, "Template column IDs must be unique"},
		{"cycle", func(tp *wbregistry.Template) {
			tp.EnrichmentColumns = raw(`[{"id":"a","name":"A","type":"ai_formula","prompt":"{b}"},{"id":"b","name":"B","type":"ai_formula","prompt":"{a}"}]`)
		}, "Template has circular column dependencies. Repair the template before creating a workbook."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp := base()
			c.mod(&tp)
			_, _, g := buildTemplateColumns(tp)
			if g == nil || g.Status != 422 || g.Detail != c.detail {
				t.Fatalf("guard = %v, want 422 %q", g, c.detail)
			}
		})
	}
	// a clean template builds editor columns in the documented shape
	tp := base()
	tp.EnrichmentColumns = raw(`[{"id":"e","name":"E","type":"waterfall","width":220}]`)
	cols, n, g := buildTemplateColumns(tp)
	if g != nil || n != 1 || len(cols) != 2 {
		t.Fatalf("clean: %v %d %d", g, n, len(cols))
	}
	b, _ := json.Marshal(cols)
	want := `[{"id":"company","name":"Company","type":"lead_field","lead_field":"company","width":180},{"id":"e","name":"E","type":"waterfall","width":220}]`
	if string(b) != want {
		t.Fatalf("columns = %s", b)
	}
}

func TestEveryShippedTemplateBuilds(t *testing.T) {
	reg, err := wbregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range reg.Templates("") {
		if _, _, g := buildTemplateColumns(tp); g != nil {
			t.Errorf("%s: %v", tp.ID, g)
		}
	}
}

// The stored columns_config text is not HTML-escaped (like every other write path and Python's
// json.dumps): "<", ">" and "&" stay literal.
func TestTemplateColumnsStoredWithoutHTMLEscaping(t *testing.T) {
	tp := wbregistry.Template{ID: "x", Name: "X", Description: "d",
		Columns: raw(`[{"key":"company","name":"A<B & C>","type":"text"}]`)}
	cols, _, g := buildTemplateColumns(tp)
	if g != nil {
		t.Fatal(g)
	}
	b, err := marshalTemplateColumns(cols)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"A<B & C>"`) || strings.Contains(string(b), `\u003c`) || strings.Contains(string(b), `\u0026`) {
		t.Fatalf("columns_config escaped: %s", b)
	}
}
