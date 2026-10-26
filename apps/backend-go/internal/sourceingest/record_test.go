package sourceingest

import "testing"

func ip(n int) *int { return &n }

func TestPageHasMoreRule(t *testing.T) {
	recs := make([]Record, 3)
	cases := []struct {
		name      string
		page, ps  int
		totalPage *int
		recs      []Record
		want      bool
	}{
		{"total_pages_more", 1, 3, ip(2), recs, true},
		{"total_pages_last", 2, 3, ip(2), recs, false},
		{"total_pages_beyond", 3, 3, ip(2), recs, false},
		{"total_pages_wins_over_full_page", 2, 3, ip(2), recs, false},
		{"total_pages_wins_over_short_page", 1, 30, ip(2), recs[:1], true},
		{"no_total_full_page", 1, 3, nil, recs, true},
		{"no_total_over_full", 1, 2, nil, recs, true},
		{"no_total_short_page", 1, 4, nil, recs, false},
		{"no_total_empty", 1, 4, nil, nil, false},
	}
	for _, c := range cases {
		p := NewPage("ambitionbox", c.recs, c.page, c.ps, ip(10), c.totalPage)
		if p.HasMore != c.want {
			t.Errorf("%s: HasMore=%v want %v", c.name, p.HasMore, c.want)
		}
		if p.Provider != "ambitionbox" || p.Page != c.page || p.PageSize != c.ps || len(p.Records) != len(c.recs) {
			t.Errorf("%s: fields not carried: %+v", c.name, p)
		}
	}
}
