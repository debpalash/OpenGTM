//go:build integration

package records

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

func TestCanonicalAudienceMonitoringOutreachAndDelivery(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	company := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "No contacts yet", "domain": "warehouse.test", "industry": "Robotics"}}, "import:clay:companies", "row-1")
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO audiences(id,workspace_id,name,filters) VALUES('companies',$1,'Qualified companies','{"_record_type":"company","where":{"industry":"Robotics"}}')`, ws)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := audiencerefresh.Refresh(ctx, s.Pool, ws, "companies", audiencerefresh.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Entered != 1 || first.Audience.MemberCount != 1 {
		t.Fatalf("company-only audience %+v", first)
	}
	second, err := audiencerefresh.Refresh(ctx, s.Pool, ws, "companies", audiencerefresh.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Entered != 0 || second.Changed != 0 || second.Unchanged != 1 {
		t.Fatalf("repeat audience %+v", second)
	}
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		var lead *int32
		var cid string
		if e := tx.QueryRow(ctx, `SELECT lead_id,company_entity_id FROM audience_members WHERE workspace_id=$1 AND audience_id='companies'`, ws).Scan(&lead, &cid); e != nil {
			return e
		}
		if lead != nil || cid != company.ID {
			t.Fatal("company audience created a synthetic lead")
		}
		w := poller.WatchRow{ID: "watch-company", WorkspaceID: ws, Kind: "company", Target: "warehouse.test", CompanyID: company.ID}
		events := []poller.DetectedEvent{{SignalType: "company_funded", NaturalEventID: "funding-1", Title: "Funding", Company: "No contacts yet", Source: "test", Weight: 5}}
		for i := 0; i < 2; i++ {
			out, e := poller.EmitEvents(ctx, tx, nil, w, 0, events, time.Now())
			if e != nil {
				return e
			}
			if i == 0 && out.Inserted != 1 || i == 1 && out.Dupe != 1 {
				t.Fatalf("monitor dedup %+v", out)
			}
		}
		var n int
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM signals WHERE workspace_id=$1 AND company_entity_id=$2 AND lead_id IS NULL`, ws, company.ID).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			t.Fatal("company-only signal lost")
		}
		_, e := tx.Exec(ctx, `INSERT INTO audience_destinations(id,workspace_id,audience_id,name,destination_type,enabled,config,field_map,health_status) VALUES('dest',$1,'companies','Test','webhook',true,'{}','{}','healthy');`, ws)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO destination_runs(id,workspace_id,destination_id,status,attempted,succeeded,failed,skipped) VALUES('run',$1,'dest','running',0,0,0,0)`, ws)
		if e != nil {
			return e
		}
		input := deststore.UpsertDeliveryInput{RunID: "run", DestinationID: "dest", CompanyID: company.ID, SubjectKey: "company:" + company.ID, Operation: "upsert", IdempotencyKey: destdeliver.SubjectIdempotencyKey("dest", "company:"+company.ID, "fingerprint", false), Fingerprint: "fingerprint", Payload: json.RawMessage(`{}`), Status: "success"}
		_, out, e := deststore.UpsertDelivery(ctx, tx, input)
		if e != nil {
			return e
		}
		if out != deststore.Inserted {
			t.Fatal("delivery was not inserted")
		}
		input.IdempotencyKey = "a-different-post-migration-key"
		_, out, e = deststore.UpsertDelivery(ctx, tx, input)
		if e != nil {
			return e
		}
		if out != deststore.AlreadySucceeded {
			t.Fatal("historical subject delivery was repeated")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	person := mustRecord(t, s, ws, Person, Write{CompanyID: company.ID, Person: map[string]any{"full_name": "Ana", "email": "ana@warehouse.test"}}, "manual", "")
	err = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		seq, e := outreachstore.CreateSequence(ctx, tx, ws, outreachstore.NewSeqInput("Welcome"))
		if e != nil {
			return e
		}
		id, created, e := outreachstore.EnrollPerson(ctx, tx, ws, seq.ID, person.ID, "manual_test_consent", time.Now())
		if e != nil {
			return e
		}
		if !created {
			t.Fatal("person not enrolled")
		}
		id2, created, e := outreachstore.EnrollPerson(ctx, tx, ws, seq.ID, person.ID, "manual_test_consent", time.Now())
		if e != nil {
			return e
		}
		if created || id != id2 {
			t.Fatal("repeat person enrollment duplicated")
		}
		queued, e := outreachsend.EnqueueDue(ctx, tx, ws, seq.ID, 100, "go")
		if e != nil {
			return e
		}
		if queued != 1 {
			t.Fatalf("send jobs=%d", queued)
		}
		var payload []byte
		if e = tx.QueryRow(ctx, `SELECT payload FROM jobs WHERE workspace_id=$1 AND type='send'`, ws).Scan(&payload); e != nil {
			return e
		}
		var job map[string]any
		if e = json.Unmarshal(payload, &job); e != nil {
			return e
		}
		if job["person_id"] != person.ID {
			t.Fatal("send lost the person identity")
		}
		seq2, e := outreachstore.CreateSequence(ctx, tx, ws, outreachstore.NewSeqInput("Suppressed"))
		if e != nil {
			return e
		}
		if _, e = outreachstore.AddSuppression(ctx, tx, ws, "ana@warehouse.test", "unsubscribe", "test", true); e != nil {
			return e
		}
		_, _, e = outreachstore.EnrollPerson(ctx, tx, ws, seq2.ID, person.ID, "manual_test_consent", time.Now())
		if !errors.Is(e, outreachstore.ErrPersonSuppressed) {
			t.Fatalf("suppression bypassed: %v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	company, e := s.Get(ctx, ws, Company, company.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Patch(ctx, ws, Company, company.ID, company.Version, Write{Company: map[string]any{"industry": "Other"}}); e != nil {
		t.Fatal(e)
	}
	exit, e := audiencerefresh.Refresh(ctx, s.Pool, ws, "companies", audiencerefresh.Deps{})
	if e != nil {
		t.Fatal(e)
	}
	if exit.Exited != 1 {
		t.Fatal("accepted edit did not update company audience")
	}
}

func TestSnapshotRoundTripPreservesCustomFactsHistoryAndSources(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	c := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Snapshot Co", "domain": "snapshot.test", "custom": map[string]any{"large": json.Number("90071992547409931234"), "nested": []any{"one", true}}}}, "import:clay:companies", "source-row")
	p := mustRecord(t, s, ws, Person, Write{CompanyID: c.ID, Person: map[string]any{"full_name": "Ana", "email": "ana@snapshot.test"}, Employment: map[string]any{"title": "Founder"}}, "import:clay:people", "person-row")
	if _, e := s.Patch(ctx, ws, Company, c.ID, c.Version, Write{Company: map[string]any{"industry": "Robotics"}}); e != nil {
		t.Fatal(e)
	}
	snapshot, e := s.Snapshot(ctx, ws)
	if e != nil {
		t.Fatal(e)
	}
	req := RestoreRequest{Snapshot: snapshot, IdempotencyKey: "restore1"}
	preview, e := s.Restore(ctx, "other-tenant", req, true)
	if e != nil {
		t.Fatal(e)
	}
	if preview.Companies != 1 || preview.People != 1 {
		t.Fatal("snapshot preview counts")
	}
	page, e := s.List(ctx, "other-tenant", Company, "", 100, 0)
	if e != nil || page.Total != 0 {
		t.Fatal("restore preview persisted")
	}
	restored, e := s.Restore(ctx, "other-tenant", req, false)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Restore(ctx, "other-tenant", req, false)
	if e != nil || !again.Replayed {
		t.Fatalf("restore replay: %v", e)
	}
	c2, e := s.Get(ctx, "other-tenant", Company, restored.IDs["company:"+c.ID])
	if e != nil {
		t.Fatal(e)
	}
	if c2.Fields["custom"].(map[string]any)["large"] != json.Number("90071992547409931234") {
		t.Fatal("large custom number changed")
	}
	p2, e := s.Get(ctx, "other-tenant", Person, restored.IDs["person:"+p.ID])
	if e != nil {
		t.Fatal(e)
	}
	if len(p2.Employments) != 1 || len(p2.ExternalIDs) != 1 || *p2.CompanyID != c2.ID {
		t.Fatalf("restored relationships %+v", p2)
	}
	copy, e := s.Snapshot(ctx, "other-tenant")
	if e != nil {
		t.Fatal(e)
	}
	if len(copy.Tables["record_observations"]) != len(snapshot.Tables["record_observations"]) {
		t.Fatal("observation history changed")
	}
}

func TestCompanyMergeAndSplitPreserveCanonicalReferences(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	a := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Primary", "domain": "primary.test", "industry": "Robotics"}}, "manual", "")
	b := mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Secondary", "domain": "secondary.test", "description": "Accepted description"}}, "import:clay:companies", "secondary")
	p := mustRecord(t, s, ws, Person, Write{CompanyID: b.ID, Person: map[string]any{"full_name": "Ana", "email": "ana@secondary.test"}, Employment: map[string]any{"title": "Director"}}, "manual", "")
	wb, e := s.Workbook(ctx, ws, Person, []string{p.ID}, "References")
	if e != nil {
		t.Fatal(e)
	}
	filter, _ := json.Marshal(map[string]any{"_record_type": "company", "record_ids": []string{b.ID}})
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO audiences(id,workspace_id,name,filters) VALUES('merge-audience',$1,'Selected company',$2)`, ws, filter)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = audiencerefresh.Refresh(ctx, s.Pool, ws, "merge-audience", audiencerefresh.Deps{}); e != nil {
		t.Fatal(e)
	}
	graph := entities.Store{Pool: s.Pool}
	if _, e = graph.Merge(ctx, ws, a.ID, b.ID, "manual review"); e != nil {
		t.Fatal(e)
	}
	current, e := s.Get(ctx, ws, Person, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if *current.CompanyID != a.ID {
		t.Fatal("merged person reference lost")
	}
	refresh, e := audiencerefresh.Refresh(ctx, s.Pool, ws, "merge-audience", audiencerefresh.Deps{})
	if e != nil || refresh.Audience.MemberCount != 1 {
		t.Fatalf("merged selection: %+v %v", refresh, e)
	}
	var logID int64
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM entity_merge_log WHERE workspace_id=$1 AND kept_id=$2 AND merged_id=$3 ORDER BY id DESC LIMIT 1`, ws, a.ID, b.ID).Scan(&logID)
	})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = graph.Split(ctx, ws, logID); e != nil {
		t.Fatal(e)
	}
	current, e = s.Get(ctx, ws, Person, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if *current.CompanyID != b.ID {
		t.Fatal("split did not restore person reference")
	}
	company, e := s.Get(ctx, ws, Company, b.ID)
	if e != nil {
		t.Fatal(e)
	}
	if company.Fields["description"] != "Accepted description" {
		t.Fatal("split lost accepted company facts")
	}
	e = tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		var cid string
		e := tx.QueryRow(ctx, `SELECT data->>'company_id' FROM record_workbook_rows WHERE workspace_id=$1 AND workbook_id=$2`, ws, wb).Scan(&cid)
		if e == nil && cid != b.ID {
			t.Fatal("split did not restore workbook reference")
		}
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
}
