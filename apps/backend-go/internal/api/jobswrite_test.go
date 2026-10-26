package api

import "testing"

// parseKeepLeads is pydantic's bool parsing of the keep_leads query value (FastAPI validates it before the route runs).
func TestParseKeepLeads(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want bool
		bad  bool
	}{
		{nil, false, false}, {[]string{"true"}, true, false}, {[]string{"TRUE"}, true, false}, {[]string{"True"}, true, false},
		{[]string{"1"}, true, false}, {[]string{"t"}, true, false}, {[]string{"y"}, true, false}, {[]string{"yes"}, true, false}, {[]string{"on"}, true, false},
		{[]string{"false"}, false, false}, {[]string{"0"}, false, false}, {[]string{"f"}, false, false}, {[]string{"n"}, false, false},
		{[]string{"No"}, false, false}, {[]string{"OFF"}, false, false},
		{[]string{"maybe"}, false, true}, {[]string{""}, false, true}, {[]string{" true"}, false, true}, {[]string{"2"}, false, true},
		{[]string{"truee"}, false, true}, {[]string{"false", "true"}, true, false}, {[]string{"true", "bogus"}, false, true},
	} {
		got, fe := parseKeepLeads(c.in)
		if (fe != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("%q: got %v %v, want %v bad=%v", c.in, got, fe, c.want, c.bad)
			continue
		}
		if fe != nil {
			if fe.Type != "bool_parsing" || fe.Msg != "Input should be a valid boolean, unable to interpret input" ||
				len(fe.Loc) != 2 || fe.Loc[0] != "query" || fe.Loc[1] != "keep_leads" || fe.Input != c.in[len(c.in)-1] {
				t.Errorf("%q: error %+v", c.in, fe)
			}
		}
	}
}
