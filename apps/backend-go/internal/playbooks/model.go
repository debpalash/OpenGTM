// Package playbooks is the store, request validation and JSON models of
// research playbooks (legacy/python/apps/api/routers/playbooks.py and
// services/playbooks/models.py). Every function that touches the database runs
// inside tenancy.InWorkspace and filters by workspace_id as well.
package playbooks

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// Step is one entry of a playbook's steps list (PlaybookStep).
type Step struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	PromptTemplate string `json:"prompt_template"`
	// OutputFormat is empty only for a step a PATCH stored without it (pydantic exclude_unset): it is then omitted.
	OutputFormat string `json:"output_format,omitempty"`
}

// Playbook mirrors ResearchPlaybook.to_api (key order is the contract).
type Playbook struct {
	ID                      string
	Name                    string
	Description             string
	PromptTemplate          string
	Steps                   []Step
	OutputFormat            string
	MaxSteps                int
	CellBudgetUSD           float64
	Version                 int
	Enabled                 bool
	ScheduleAudienceID      *string
	ScheduleIntervalMinutes *int
	NextRunAt               *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Run mirrors PlaybookRun.to_api; prompt and step snapshots are not exposed.
type Run struct {
	ID            string
	PlaybookID    string
	AudienceID    string
	Status        string
	PromptVersion int
	MaxMembers    int
	Attempted     int
	Succeeded     int
	Failed        int
	Error         *string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     time.Time
}

// Result mirrors PlaybookResult.to_api; the JSON key for Metadata is "metadata"
// (the column is result_metadata).
type Result struct {
	CompanyID, PersonID string
	ID                  int64
	RunID               string
	LeadID              int
	Status              string
	Value               string
	Metadata            json.RawMessage // any JSON; falsy (null, {}, [], 0, "", false) renders {}
	Error               *string
	Attempts            int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// RunPage is the list_runs response body.
type RunPage struct {
	Runs       []Run
	Limit      int
	Offset     *int // nil (null) when a cursor was used
	HasMore    bool
	NextCursor *string
}

// ResultPage is the list_results response body.
type ResultPage struct {
	Results    []Result
	Limit      int
	Offset     *int
	HasMore    bool
	NextCursor *string
}

// w is a tiny ordered object writer shared by the MarshalJSON methods.
type w struct {
	b bytes.Buffer
	n int
}

func (o *w) key(k string) {
	if o.n == 0 {
		o.b.WriteByte('{')
	} else {
		o.b.WriteByte(',')
	}
	o.n++
	writeString(&o.b, k)
	o.b.WriteByte(':')
}

func (o *w) val(k string, v any) { o.key(k); writeValue(&o.b, v) }

func (o *w) str(k, v string)     { o.key(k); writeString(&o.b, v) }
func (o *w) int(k string, v int) { o.key(k); o.b.WriteString(strconv.Itoa(v)) }

func (o *w) optStr(k string, v *string) {
	if v == nil {
		o.val(k, nil)
		return
	}
	o.str(k, *v)
}

func (o *w) optTime(k string, v *time.Time) {
	if v == nil {
		o.val(k, nil)
		return
	}
	o.str(k, isoNaive(*v))
}

func (o *w) done() []byte { o.b.WriteByte('}'); return o.b.Bytes() }

// MarshalJSON emits keys in ResearchPlaybook.to_api order.
func (p Playbook) MarshalJSON() ([]byte, error) {
	var o w
	o.str("id", p.ID)
	o.str("name", p.Name)
	o.str("description", p.Description)
	o.str("prompt_template", p.PromptTemplate)
	o.key("steps")
	o.b.WriteByte('[')
	for i, s := range p.Steps {
		if i > 0 {
			o.b.WriteByte(',')
		}
		o.b.WriteString(`{"key":`)
		writeString(&o.b, s.Key)
		o.b.WriteString(`,"name":`)
		writeString(&o.b, s.Name)
		o.b.WriteString(`,"prompt_template":`)
		writeString(&o.b, s.PromptTemplate)
		if s.OutputFormat != "" {
			o.b.WriteString(`,"output_format":`)
			writeString(&o.b, s.OutputFormat)
		}
		o.b.WriteByte('}')
	}
	o.b.WriteByte(']')
	o.str("output_format", p.OutputFormat)
	o.int("max_steps", p.MaxSteps)
	o.val("cell_budget_usd", p.CellBudgetUSD)
	o.int("version", p.Version)
	o.val("enabled", p.Enabled)
	o.optStr("schedule_audience_id", p.ScheduleAudienceID)
	if p.ScheduleIntervalMinutes == nil {
		o.val("schedule_interval_minutes", nil)
	} else {
		o.int("schedule_interval_minutes", *p.ScheduleIntervalMinutes)
	}
	o.optTime("next_run_at", p.NextRunAt)
	o.str("created_at", isoNaive(p.CreatedAt))
	o.str("updated_at", isoNaive(p.UpdatedAt))
	return o.done(), nil
}

// MarshalJSON emits keys in PlaybookRun.to_api order.
func (r Run) MarshalJSON() ([]byte, error) {
	var o w
	o.str("id", r.ID)
	o.str("playbook_id", r.PlaybookID)
	o.str("audience_id", r.AudienceID)
	o.str("status", r.Status)
	o.int("prompt_version", r.PromptVersion)
	o.int("max_members", r.MaxMembers)
	o.int("attempted", r.Attempted)
	o.int("succeeded", r.Succeeded)
	o.int("failed", r.Failed)
	o.optStr("error", r.Error)
	o.optTime("started_at", r.StartedAt)
	o.optTime("finished_at", r.FinishedAt)
	o.str("created_at", isoNaive(r.CreatedAt))
	return o.done(), nil
}

// MarshalJSON emits keys in PlaybookResult.to_api order.
func (r Result) MarshalJSON() ([]byte, error) {
	var o w
	o.val("id", r.ID)
	o.str("run_id", r.RunID)
	if r.LeadID != 0 {
		o.int("lead_id", r.LeadID)
	} else {
		o.val("lead_id", nil)
	}
	if r.CompanyID != "" {
		o.str("company_id", r.CompanyID)
	}
	if r.PersonID != "" {
		o.str("person_id", r.PersonID)
	}
	o.str("status", r.Status)
	o.str("value", r.Value)
	o.key("metadata")
	o.b.Write(metadataJSON(r.Metadata))
	o.optStr("error", r.Error)
	o.int("attempts", r.Attempts)
	o.str("created_at", isoNaive(r.CreatedAt))
	o.str("updated_at", isoNaive(r.UpdatedAt))
	return o.done(), nil
}

// metadataJSON re-emits stored metadata like `result_metadata or {}`: parsed
// and dumped (key order kept, floats as Python repr), falsy values become {}.
func metadataJSON(m json.RawMessage) []byte {
	if len(bytes.TrimSpace(m)) == 0 {
		return []byte("{}")
	}
	doc, err := decodeDoc(m)
	if err != nil {
		return []byte("{}")
	}
	switch t := doc.(type) {
	case nil:
		return []byte("{}")
	case bool:
		if !t {
			return []byte("{}")
		}
	case string:
		if t == "" {
			return []byte("{}")
		}
	case []any:
		if len(t) == 0 {
			return []byte("{}")
		}
	case *obj:
		if len(t.keys) == 0 {
			return []byte("{}")
		}
	case json.Number:
		if f, err := strconv.ParseFloat(string(t), 64); err == nil && f == 0 {
			return []byte("{}")
		}
	}
	var b bytes.Buffer
	writeValue(&b, doc)
	return b.Bytes()
}

func (p RunPage) MarshalJSON() ([]byte, error) {
	var o w
	o.key("runs")
	o.b.WriteByte('[')
	for i, r := range p.Runs {
		if i > 0 {
			o.b.WriteByte(',')
		}
		b, _ := r.MarshalJSON()
		o.b.Write(b)
	}
	o.b.WriteByte(']')
	o.int("limit", p.Limit)
	o.pageTail(p.Offset, p.HasMore, p.NextCursor)
	return o.done(), nil
}

func (p ResultPage) MarshalJSON() ([]byte, error) {
	var o w
	o.key("results")
	o.b.WriteByte('[')
	for i, r := range p.Results {
		if i > 0 {
			o.b.WriteByte(',')
		}
		b, _ := r.MarshalJSON()
		o.b.Write(b)
	}
	o.b.WriteByte(']')
	o.int("limit", p.Limit)
	o.pageTail(p.Offset, p.HasMore, p.NextCursor)
	return o.done(), nil
}

func (o *w) pageTail(offset *int, hasMore bool, next *string) {
	if offset == nil {
		o.val("offset", nil)
	} else {
		o.int("offset", *offset)
	}
	o.val("has_more", hasMore)
	o.optStr("next_cursor", next)
}
