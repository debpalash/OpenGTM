// Package wbfunctions stores reusable workbook column chains
// (services/workbook/functions.py, Postgres path). workbook_functions is a
// global table (no RLS): reads and writes use the plain pool. Only Apply touches
// a tenant table and does so inside tenancy.InWorkspace.
package wbfunctions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

var (
	ErrFunctionNotFound = errors.New("Function not found")
	ErrWorkbookNotFound = errors.New("Workbook not found")
)

// Store reads and writes workbook_functions.
type Store struct{ Pool *pgxpool.Pool }

// New returns a Store on the plain (non-tenant) pool.
func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// List returns functions ordered by usage_count desc (ties: oldest first). An
// empty category lists everything (Python `if category:`).
func (s *Store) List(ctx context.Context, category string) ([]gen.ListWorkbookFunctionsRow, error) {
	return gen.New(s.Pool).ListWorkbookFunctions(ctx, category)
}

// Get returns one function, or ErrFunctionNotFound.
func (s *Store) Get(ctx context.Context, id string) (gen.GetWorkbookFunctionRow, error) {
	r, err := gen.New(s.Pool).GetWorkbookFunction(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrFunctionNotFound
	}
	return r, err
}

// Create inserts a function (version 1, usage 0, float epoch timestamps) and
// returns its new id. chain is the JSON array text.
func (s *Store) Create(ctx context.Context, name, description, category string, chain []byte) (string, error) {
	id := newUUID4()
	now := float64(time.Now().UnixMicro()) / 1e6
	err := gen.New(s.Pool).InsertWorkbookFunction(ctx, gen.InsertWorkbookFunctionParams{
		ID: id, Name: name, Description: description, Category: category, ColumnsChain: chain, Now: now,
	})
	return id, err
}

// Delete removes a function; deleting an unknown id is not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.Pool).DeleteWorkbookFunction(ctx, id)
}

// ApplyResult is the body of POST /api/functions/{id}/apply.
type ApplyResult struct {
	Applied      string `json:"applied"`
	ColumnsAdded int    `json:"columns_added"`
	TotalColumns int    `json:"total_columns"`
}

// Apply appends the chain's columns whose id (or legacy key) is not already in
// the workbook's columns_config, then bumps usage_count. The workbook must
// belong to workspaceID (else ErrWorkbookNotFound, checked first). Cycle validation is
// the caller's, via ApplyChecked. columns_config is only rewritten (stamping
// updated_at) when it changes, as SQLAlchemy skips an equal-value UPDATE.
func (s *Store) Apply(ctx context.Context, workspaceID, funcID, workbookID string) (ApplyResult, error) {
	return s.ApplyChecked(ctx, workspaceID, funcID, workbookID, nil)
}

// Validator inspects the columns_config Apply is about to write. before is the stored value (as read
// under the row lock), after the exact merged array that will be written. A non-nil error aborts the
// transaction untouched and is returned from ApplyChecked as is (usage_count is not bumped).
type Validator func(before, after []byte) error

// ApplyChecked is Apply with a validator run on the merged columns inside the same locked
// transaction as the write, so no concurrent column edit can land between check and write (F4). The
// validator only runs when the merge would change columns_config.
func (s *Store) ApplyChecked(ctx context.Context, workspaceID, funcID, workbookID string, validate Validator) (ApplyResult, error) {
	var res ApplyResult
	err := tenancy.InWorkspace(ctx, s.Pool, workspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		cfg, err := q.LockWorkbookColumnsConfig(ctx, gen.LockWorkbookColumnsConfigParams{WorkbookID: workbookID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkbookNotFound
		}
		if err != nil {
			return err
		}
		fn, err := q.GetWorkbookFunction(ctx, funcID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFunctionNotFound
		}
		if err != nil {
			return err
		}
		existing, wasNull, err := splitArray(cfg)
		if err != nil {
			return err
		}
		chain, _, err := splitArray(fn.ColumnsChain)
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(existing)+len(chain))
		for _, c := range existing {
			k, err := identity(c)
			if err != nil {
				return err
			}
			seen[k] = true
		}
		merged := existing
		added := 0
		for _, c := range chain {
			k, err := identity(c)
			if err != nil {
				return err
			}
			if !seen[k] {
				seen[k] = true
				merged = append(merged, c)
				added++
			}
		}
		res = ApplyResult{Applied: fn.Name, ColumnsAdded: added, TotalColumns: len(merged)}
		if added == 0 && !wasNull {
			return nil
		}
		out := joinArray(merged)
		if validate != nil {
			if err := validate(cfg, out); err != nil {
				return err
			}
		}
		return q.SetWorkbookColumnsConfig(ctx, gen.SetWorkbookColumnsConfigParams{
			ColumnsConfig: out, WorkbookID: workbookID, WorkspaceID: workspaceID,
		})
	})
	if err != nil {
		return ApplyResult{}, err
	}
	if err := gen.New(s.Pool).BumpWorkbookFunctionUsage(ctx, funcID); err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// splitArray splits a JSON array into its raw elements. SQL NULL and JSON null
// read as an empty list (Python `columns_config or []`) with wasNull set.
func splitArray(raw []byte) (elems []json.RawMessage, wasNull bool, err error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, true, nil
	}
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, false, fmt.Errorf("columns array: %w", err)
	}
	return elems, false, nil
}

func joinArray(elems []json.RawMessage) []byte {
	b, _ := json.Marshal(elems)
	if elems == nil {
		return []byte("[]")
	}
	return b
}

// identity is `c.get("id") or c.get("key")` reduced to a set key. A column
// that is not an object is an error (Python raises AttributeError -> 500).
func identity(raw json.RawMessage) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return "", errors.New("column is not an object")
	}
	v := pyOr(m["id"], m["key"])
	return setKey(v), nil
}

// pyOr returns a if it is truthy in Python, else b (which may be absent: None).
func pyOr(a, b json.RawMessage) json.RawMessage {
	if truthy(a) {
		return a
	}
	return b
}

func truthy(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	switch s {
	case "", "null", "false", `""`, "[]", "{}":
		return false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f != 0
	}
	return true
}

// setKey maps a JSON value to a key equal exactly when Python set members are
// equal for the shapes that occur here (strings, numbers, absent).
func setKey(raw json.RawMessage) string {
	s := string(bytes.TrimSpace(raw))
	if s == "" || s == "null" {
		return "none"
	}
	if s == "true" {
		return "n:1"
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) {
		return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return "s:" + str
	}
	return "j:" + s
}

// newUUID4 is str(uuid.uuid4()).
func newUUID4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
