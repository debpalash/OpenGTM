// Package entities is the canonical company entity store (services/entities in Python).
// All three tables are tenant tables under FORCE RLS: every call runs in tenancy.InWorkspace.
package entities

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// ErrNotFound is returned for a missing entity, including one owned by another workspace.
var ErrNotFound = errors.New("entities: not found")

// Store reads canonical entities.
type Store struct{ Pool *pgxpool.Pool }

// Company marshals exactly like CompanyEntity.to_api() (key order included).
type Company struct {
	ID                 string          `json:"id"`
	WorkspaceID        string          `json:"workspace_id"`
	CanonicalName      string          `json:"canonical_name"`
	PrimaryDomain      *string         `json:"primary_domain"`
	PrimaryPhone       *string         `json:"primary_phone"`
	PrimaryEmail       *string         `json:"primary_email"`
	PrimaryCity        *string         `json:"primary_city"`
	IdentityKeys       json.RawMessage `json:"identity_keys"`
	Fields             json.RawMessage `json:"fields"`
	Sources            json.RawMessage `json:"sources"`
	SourceAgreement    json.RawMessage `json:"source_agreement"`
	CorroborationCount *int32          `json:"corroboration_count"`
	ObservationCount   *int32          `json:"observation_count"`
}

// ReviewPair is one entry of the review queue.
type ReviewPair struct {
	ID        int32           `json:"id"`
	EntityID  *string         `json:"entity_id"`
	Candidate json.RawMessage `json:"candidate"`
	Score     *float64        `json:"score"`
	Reason    *string         `json:"reason"`
	Status    *string         `json:"status"`
}

func text(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func int4(i pgtype.Int4) *int32 {
	if !i.Valid {
		return nil
	}
	return &i.Int32
}

// orDefault mirrors Python's `value or default`: SQL NULL and falsy JSON
// (null, false, 0, "", [], {}) become def; anything else is kept verbatim.
func orDefault(raw []byte, def string) json.RawMessage {
	t := bytes.TrimSpace(raw)
	switch string(t) {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return json.RawMessage(def)
	}
	return json.RawMessage(t)
}

func toCompany(e gen.CompanyEntity) Company {
	return Company{
		ID: e.ID, WorkspaceID: e.WorkspaceID, CanonicalName: e.CanonicalName,
		PrimaryDomain: text(e.PrimaryDomain), PrimaryPhone: text(e.PrimaryPhone),
		PrimaryEmail: text(e.PrimaryEmail), PrimaryCity: text(e.PrimaryCity),
		IdentityKeys:       orDefault(e.IdentityKeys, "{}"),
		Fields:             orDefault(e.Fields, "{}"),
		Sources:            orDefault(e.Sources, "[]"),
		SourceAgreement:    orDefault(e.SourceAgreement, "{}"),
		CorroborationCount: int4(e.CorroborationCount), ObservationCount: int4(e.ObservationCount),
	}
}

// ListCompanies returns the workspace's entities with corroboration >= minCorroboration,
// most corroborated first.
func (s *Store) ListCompanies(ctx context.Context, ws string, minCorroboration, limit int32) ([]Company, error) {
	out := []Company{}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).ListCompanyEntities(ctx, gen.ListCompanyEntitiesParams{
			WorkspaceID: ws, MinCorroboration: minCorroboration, RowLimit: limit})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, toCompany(r))
		}
		return nil
	})
	return out, err
}

// GetCompany returns one entity of the workspace, or ErrNotFound.
func (s *Store) GetCompany(ctx context.Context, ws, id string) (Company, error) {
	var c Company
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		e, err := gen.New(tx).GetCompanyEntity(ctx, gen.GetCompanyEntityParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c = toCompany(e)
		return nil
	})
	return c, err
}

// ReviewQueue returns review pairs with the given status whose entity belongs to ws, best score first.
func (s *Store) ReviewQueue(ctx context.Context, ws, status string, limit int32) ([]ReviewPair, error) {
	out := []ReviewPair{}
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).ListEntityReviewPairs(ctx, gen.ListEntityReviewPairsParams{
			WorkspaceID: ws, Status: status, RowLimit: limit})
		if err != nil {
			return err
		}
		for _, r := range rows {
			p := ReviewPair{ID: r.ID, EntityID: text(r.EntityID), Reason: text(r.Reason), Status: text(r.Status)}
			if r.Candidate == nil {
				p.Candidate = json.RawMessage("null")
			} else {
				p.Candidate = json.RawMessage(r.Candidate)
			}
			if r.Score.Valid {
				p.Score = &r.Score.Float64
			}
			out = append(out, p)
		}
		return nil
	})
	return out, err
}
