// Package sourceingest ports services/workbook/source_ingest.py and the
// connector page contracts of services/connectors/contracts.py: transactional,
// identity-keyed upserts of connector records into workbook rows.
package sourceingest

import "time"

// Record is a ConnectorRecord: one source record with its stable identity.
type Record struct {
	Provider  string
	RecordID  string
	Data      map[string]any
	Rank      int
	SourceURL *string
	FetchedAt time.Time
}

// Page is a ConnectorPage: one complete source page.
type Page struct {
	Provider    string
	Records     []Record
	Page        int
	PageSize    int
	SourceTotal *int
	TotalPages  *int
	HasMore     bool
}

// NewPage is ConnectorPage.from_records: has_more is `page < total_pages` when
// the total is known, else a full page (`len(records) >= page_size`).
func NewPage(provider string, records []Record, page, pageSize int, sourceTotal, totalPages *int) Page {
	hasMore := len(records) >= pageSize
	if totalPages != nil {
		hasMore = page < *totalPages
	}
	return Page{Provider: provider, Records: records, Page: page, PageSize: pageSize,
		SourceTotal: sourceTotal, TotalPages: totalPages, HasMore: hasMore}
}

// Summary is SourceIngestSummary.
type Summary struct {
	Added, Updated, Skipped int
	AddedRowIDs             []int64
}
