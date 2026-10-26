package wbcrud

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

func rowFixture(id string, created, updated time.Time) gen.Workbook {
	return gen.Workbook{
		ID: id, Name: "N",
		Status:        pgtype.Text{String: "draft", Valid: true},
		SourceType:    pgtype.Text{String: "leads_filter", Valid: true},
		SourceConfig:  []byte(`{"row_storage_version": 2, "b": 1.5}`),
		ColumnsConfig: []byte(`[{"id":"c1","name":"X","type":"lead_field","width":200,"zz":1}]`),
		CompletedRows: pgtype.Int4{Int32: 0, Valid: true},
		SyncToLeads:   pgtype.Bool{Bool: true, Valid: true},
		CreatedAt:     pgtype.Timestamp{Time: created, Valid: true},
		UpdatedAt:     pgtype.Timestamp{Time: updated, Valid: true},
	}
}

func nullRowFixture() gen.Workbook { return gen.Workbook{ID: "w2", Name: "N"} }
