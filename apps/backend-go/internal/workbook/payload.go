package workbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// PayloadVersion is the only "go" block version this executor understands.
const PayloadVersion = 1

const defaultProviderTimeout = 10.0

// Upper bounds applied to payload knobs.
const (
	MaxConcurrency     = 64
	MaxRetryPasses     = 5
	MaxProviderTimeout = 120.0 // seconds
)

func clampF(v, lo, hi float64) float64 {
	if math.IsNaN(v) || v < lo {
		return lo
	}
	return math.Min(v, hi)
}

// ErrUnsupportedPayload is returned for a go block the executor must refuse.
var ErrUnsupportedPayload = errors.New("workbook: unsupported go payload")

type goBlock struct {
	Version            int                        `json:"version"`
	Manifests          map[string]json.RawMessage `json:"manifests"`
	ManifestSHA256     map[string]string          `json:"manifest_sha256"`
	Costs              map[string]float64         `json:"costs"`
	DefaultConcurrency int                        `json:"default_concurrency"`
}

type rawPayload struct {
	WorkspaceID     string          `json:"workspace_id"`
	WorkbookID      string          `json:"workbook_id"`
	ColumnIDs       *[]string       `json:"column_ids"`
	RowIDs          *[]int32        `json:"row_ids"`
	LeadIDs         *[]int32        `json:"lead_ids"`
	RowColumns      json.RawMessage `json:"row_columns"`
	Concurrency     *float64        `json:"concurrency"`
	RetryPasses     *float64        `json:"retry_passes"`
	FillMissing     bool            `json:"fill_missing"`
	Force           bool            `json:"force"`
	MaxProviders    *float64        `json:"max_providers"`
	ProviderTimeout *float64        `json:"provider_timeout"`
	Go              *goBlock        `json:"go"`
}

// runPayload is the decoded, validated run_workbook payload.
type runPayload struct {
	WorkspaceID, WorkbookID string
	ColumnIDs               map[string]bool // nil = every column
	RowIDs, LeadIDs         []int32
	HasRowIDs, HasLeadIDs   bool
	RowColumns              map[string][]string // nil = unrestricted
	hasRowColumns           bool
	Concurrency             int
	RetryPasses             int
	FillMissing, Force      bool
	MaxProviders            int
	ProviderTimeout         float64
	Manifests               map[string]manifest.Manifest
	Costs                   map[string]float64
}

// parsePayload validates the payload against the v1 go contract. The manifest
// sha256 map is required to be present for every manifest but is not
// re-derived: Python hashes the signed YAML file bytes, which the Go side never
// sees (the payload carries the Pydantic model_dump).
func parsePayload(raw []byte) (*runPayload, error) {
	var rp rawPayload
	if err := json.Unmarshal(raw, &rp); err != nil {
		return nil, fmt.Errorf("workbook: decode payload: %w", err)
	}
	if rp.WorkspaceID == "" || rp.WorkbookID == "" {
		return nil, errors.New("workbook: payload needs workspace_id and workbook_id")
	}
	if rp.Go == nil {
		return nil, fmt.Errorf("%w: missing go block", ErrUnsupportedPayload)
	}
	if rp.Go.Version != PayloadVersion {
		return nil, fmt.Errorf("%w: go.version %d", ErrUnsupportedPayload, rp.Go.Version)
	}
	p := &runPayload{
		WorkspaceID: rp.WorkspaceID, WorkbookID: rp.WorkbookID,
		FillMissing: rp.FillMissing, Force: rp.Force,
		Costs: rp.Go.Costs, Manifests: make(map[string]manifest.Manifest, len(rp.Go.Manifests)),
		Concurrency: rp.Go.DefaultConcurrency, RetryPasses: 1, ProviderTimeout: defaultProviderTimeout,
	}
	if p.Costs == nil {
		p.Costs = map[string]float64{}
	}
	for name, mraw := range rp.Go.Manifests {
		if rp.Go.ManifestSHA256[name] == "" {
			return nil, fmt.Errorf("%w: manifest %q has no sha256", ErrUnsupportedPayload, name)
		}
		var m manifest.Manifest
		if err := json.Unmarshal(mraw, &m); err != nil {
			return nil, fmt.Errorf("%w: manifest %q: %v", ErrUnsupportedPayload, name, err)
		}
		p.Manifests[name] = m
	}
	for name, c := range p.Costs {
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
			return nil, fmt.Errorf("%w: invalid cost for %q", ErrUnsupportedPayload, name)
		}
	}
	// Knobs are untrusted payload JSON: clamp so a bad value cannot spawn
	// unbounded goroutines, loop forever or overflow a Duration.
	conc := float64(p.Concurrency)
	if p.Concurrency < 1 {
		conc = 12 // WORKBOOK_RUN_CONCURRENCY default
	}
	if rp.Concurrency != nil {
		conc = *rp.Concurrency
	}
	p.Concurrency = int(clampF(conc, 1, MaxConcurrency))
	if rp.RetryPasses != nil {
		p.RetryPasses = int(clampF(*rp.RetryPasses, 0, MaxRetryPasses))
	}
	if rp.MaxProviders != nil {
		p.MaxProviders = int(clampF(*rp.MaxProviders, 0, 1<<20))
	}
	if rp.ProviderTimeout != nil {
		p.ProviderTimeout = *rp.ProviderTimeout
	}
	if !(p.ProviderTimeout > 0) {
		return nil, errors.New("workbook: provider_timeout must be positive")
	}
	p.ProviderTimeout = math.Min(p.ProviderTimeout, MaxProviderTimeout)
	if rp.ColumnIDs != nil {
		p.ColumnIDs = make(map[string]bool, len(*rp.ColumnIDs))
		for _, c := range *rp.ColumnIDs {
			p.ColumnIDs[c] = true
		}
	}
	if rp.RowIDs != nil {
		p.HasRowIDs, p.RowIDs = true, *rp.RowIDs
	}
	if rp.LeadIDs != nil {
		p.HasLeadIDs, p.LeadIDs = true, *rp.LeadIDs
	}
	if s := string(rp.RowColumns); s != "" && s != "null" {
		rc, err := parseRowColumns(rp.RowColumns)
		if err != nil {
			return nil, err
		}
		p.RowColumns, p.hasRowColumns = rc, true
	}
	return p, nil
}

// parseRowColumns ports the validation in cell_scope.restrict_work_items.
func parseRowColumns(raw json.RawMessage) (map[string][]string, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("Invalid per-row column scope")
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		if !isCanonicalPositiveInt(k) {
			return nil, errors.New("Invalid row identity in cell scope")
		}
		var ids []string
		if err := json.Unmarshal(v, &ids); err != nil {
			return nil, errors.New("Invalid column identities in cell scope")
		}
		for _, id := range ids {
			if id == "" {
				return nil, errors.New("Invalid column identities in cell scope")
			}
		}
		out[k] = ids
	}
	return out, nil
}

func isCanonicalPositiveInt(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) <= 18
}
