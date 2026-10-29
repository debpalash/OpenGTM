package api

import (
	"bufio"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// exportLimit is the row cap of routers/leads.py export_csv.
const exportLimit = 10000

// exportHeader is the 27-title header row, in Python order.
var exportHeader = []string{
	"Company", "Website", "Email", "Phone", "City", "State", "Address",
	"Specialization", "Description", "Industry Tags",
	"Score", "Tier", "Status", "Source",
	"LinkedIn", "Contact Person", "Company Size", "Employee Count",
	"Founded Year", "Technologies", "Funding Stage", "Revenue Range",
	"Glassdoor Rating", "Decision Makers",
	"Secondary Emails", "Secondary Phones", "Notes",
}

// plainNumber is export.py _PLAIN_NUMBER. Python's "$" also matches before a
// final newline, hence the optional "\n".
var plainNumber = regexp.MustCompile(`^[+-]?[0-9][0-9 ().-]*\n?$`)

// csvSafe is export.py csv_safe: nil is "", text starting with = + - @ TAB or
// CR gets a leading apostrophe unless it is a plain number, other values pass.
func csvSafe(v any) any {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		if t != "" && strings.ContainsRune("=+-@\t\r", rune(t[0])) && !plainNumber.MatchString(t) {
			return "'" + t
		}
	}
	return v
}

// csvText is the str() csv.writer applies to a cell.
func csvText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		switch {
		case math.IsNaN(t):
			return "nan"
		case math.IsInf(t, 1):
			return "inf"
		case math.IsInf(t, -1):
			return "-inf"
		}
		s, _ := canonjson.FloatRepr(t)
		return s
	}
	return ""
}

// writeExportRow writes one csv.writer row of already-safe cells.
func writeExportRow(w interface{ Write([]byte) (int, error) }, cells []any) error {
	s := make([]string, len(cells))
	for i, c := range cells {
		s[i] = csvText(c)
	}
	return wbcrud.WriteCSVRow(w, s)
}

func strv(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func intv(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// exportCells lists a lead's 27 cells in header order, csv_safe applied.
func exportCells(l leadsread.Lead) []any {
	cells := []any{
		l.Company, strv(l.Website), strv(l.Email), strv(l.Phone), strv(l.City),
		strv(l.State), strv(l.Address), strv(l.Specialization), strv(l.Description),
		strv(l.IndustryTags), intv(l.Score), strv(l.ScoreTier), strv(l.Status),
		strv(l.Source), strv(l.LinkedinURL), strv(l.ContactPerson),
		strv(l.CompanySize), intv(l.EmployeeCountExact),
		strv(l.FoundedYear), strv(l.Technologies), strv(l.FundingStage),
		strv(l.RevenueRange), strv(l.GlassdoorRating),
		strv(l.DecisionMakers), strv(l.SecondaryEmails), strv(l.SecondaryPhones),
		strv(l.Notes),
	}
	for i, c := range cells {
		cells[i] = csvSafe(c)
	}
	return cells
}

// registerExport registers GET /api/export/csv (current_workspace only).
func registerExport(r *httpx.Router, d Deps) {
	if d.Pool == nil {
		return
	}
	h := leadReadRoutes{d: d}
	r.Handle("GET /api/export/csv", http.HandlerFunc(h.exportCSV),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity))
}

// exportCSV filters on the raw status, city, tier and score_min values (no
// "__all__" handling, no dead exclusion), reads at most exportLimit leads by
// score and streams them through a buffered writer. At most exportLimit rows
// are ever held, and the connection is released before the first byte is sent.
func (h leadReadRoutes) exportCSV(w http.ResponseWriter, r *http.Request) {
	v := &queryValidator{q: r.URL.Query()}
	p := leadsread.ListParams{
		Status: v.str("status"), City: v.str("city"), ScoreTier: v.str("tier"),
		ScoreMin: v.optInt("score_min"), Limit: exportLimit, OrderBy: "score DESC",
	}
	if v.fail(w) {
		return
	}
	var leads []leadsread.Lead
	if !h.inTx(w, r, "export", func(tx pgx.Tx) (err error) {
		leads, err = leadsread.ListLeads(r.Context(), tx, p)
		return err
	}) {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=opengtm_leads.csv")
	out := bufio.NewWriterSize(w, 32<<10)
	hdr := make([]any, len(exportHeader))
	for i, t := range exportHeader {
		hdr[i] = t
	}
	if writeExportRow(out, hdr) != nil {
		return
	}
	for _, l := range leads {
		if writeExportRow(out, exportCells(l)) != nil {
			return
		}
	}
	_ = out.Flush()
}
