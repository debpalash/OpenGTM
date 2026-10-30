package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"iter"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencevalid"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// GET /api/audiences/{id}/accounts (routers/audiences.py list_audience_accounts): person-level membership rolled up
// into company coverage and intent. Like FastAPI the order is authentication, workspace (no role gate), query
// validation (422), then the audience lookup (404).

const (
	audAccountsPage       = 1000
	audAccountsDefaultMax = 200000
	audAccountsTooLarge   = "audience too large for account rollup"
)

// audAccountsMaxMembers overrides AUDIENCE_ACCOUNTS_MAX_MEMBERS when non-zero (tests shrink it).
var audAccountsMaxMembers int

// registerAudiencesAccounts registers GET /api/audiences/{audience_id}/accounts.
func registerAudiencesAccounts(r *httpx.Router, d Deps) {
	h := audAccounts{audCrud: audCrud{d: d}}
	r.Handle("GET /api/audiences/{audience_id}/accounts", http.HandlerFunc(h.accounts),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity))
}

type audAccounts struct{ audCrud }

func (h audAccounts) maxMembers() int {
	if audAccountsMaxMembers > 0 {
		return audAccountsMaxMembers
	}
	if m := h.d.Flags.Audiences.AccountsMaxMembers; m > 0 {
		return m
	}
	return audAccountsDefaultMax
}

// audAccountsQuery validates limit (default 50, 1..200) and offset (default 0, 0..100000) in declaration order;
// the last value of a repeated parameter wins.
func audAccountsQuery(q url.Values) (limit, offset int, errs []httpx.FieldError) {
	bounded := func(name string, def, lo, hi int) int {
		vs := q[name]
		if len(vs) == 0 {
			return def
		}
		raw := vs[len(vs)-1]
		loc := []any{"query", name}
		b, ok := retentionQueryInt(strings.Trim(raw, " \t\n\r\v\f"))
		switch {
		case !ok:
			errs = append(errs, httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
				Msg: "Input should be a valid integer, unable to parse string as an integer"})
		case b.Cmp(big.NewInt(int64(lo))) < 0:
			errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
				Msg: "Input should be greater than or equal to " + strconv.Itoa(lo), Ctx: map[string]any{"ge": lo}})
		case b.Cmp(big.NewInt(int64(hi))) > 0:
			errs = append(errs, httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
				Msg: "Input should be less than or equal to " + strconv.Itoa(hi), Ctx: map[string]any{"le": hi}})
		default:
			return int(b.Int64())
		}
		return def
	}
	limit = bounded("limit", 50, 1, 200)
	offset = bounded("offset", 0, 0, 100000)
	return limit, offset, errs
}

// audAccSnapshot is dict(member.snapshot or {}): numbers stay exact (json.Number); anything but an object is empty.
func audAccSnapshot(raw []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func (h audAccounts) accounts(w http.ResponseWriter, r *http.Request) {
	limit, offset, errs := audAccountsQuery(r.URL.Query())
	if len(errs) > 0 {
		httpx.ValidationError(w, errs...)
		return
	}
	id, ok := h.audienceID(w, r)
	if !ok {
		return
	}
	ws := h.ws(r).WorkspaceID
	max := h.maxMembers()
	ctx := r.Context()
	var res audiencevalid.Result
	err := tenancy.InWorkspace(ctx, h.d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		found, err := q.AudAccExists(ctx, gen.AudAccExistsParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return err
		}
		if !found {
			return pgx.ErrNoRows
		}
		// Refuse an oversized audience before loading anything (the rollup keeps one entry per member).
		n, err := q.AudAccMemberCount(ctx, gen.AudAccMemberCountParams{WorkspaceID: ws, AudienceID: id})
		if err != nil {
			return err
		}
		if n > int64(max) {
			return audiencevalid.ErrTooLarge
		}
		rows, err := q.AudAccSignals(ctx, gen.AudAccSignalsParams{WorkspaceID: ws, AudienceID: id})
		if err != nil {
			return err
		}
		signals := make(map[int]audiencevalid.Signal, len(rows))
		for _, s := range rows {
			if s.LeadID.Valid {
				signals[int(s.LeadID.Int32)] = audiencevalid.Signal{Count: int(s.N), Weight: int(s.Weight)}
			}
		}
		members := func() iter.Seq2[audiencevalid.Member, error] {
			return func(yield func(audiencevalid.Member, error) bool) {
				after := int32(0)
				for {
					page, err := q.AudAccMembersPage(ctx, gen.AudAccMembersPageParams{WorkspaceID: ws, AudienceID: id,
						AfterID: after, PageLimit: audAccountsPage})
					if err != nil {
						yield(audiencevalid.Member{}, err)
						return
					}
					for _, m := range page {
						if !yield(audiencevalid.Member{LeadID: int(m.LeadID.Int32), Snapshot: audAccSnapshot(m.Snapshot)}, nil) {
							return
						}
						after = m.ID
					}
					if len(page) < audAccountsPage {
						return
					}
				}
			}
		}
		res, err = audiencevalid.Rollup(members(), signals, limit, offset, max)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Detail(w, http.StatusNotFound, audNotFound)
	case errors.Is(err, audiencevalid.ErrTooLarge):
		httpx.Detail(w, http.StatusUnprocessableEntity, audAccountsTooLarge)
	case err != nil:
		h.internal(w, "accounts", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}
