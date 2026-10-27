package leadpeople

import "testing"

func TestIdentityVectors(t *testing.T) {
	var v struct {
		PersonIDs []struct {
			Company string         `json:"company"`
			Person  map[string]any `json:"person"`
			ID      string         `json:"id"`
		} `json:"person_ids"`
		ResultSets []struct {
			Company  string           `json:"company"`
			Function string           `json:"function"`
			People   []map[string]any `json:"people"`
			ID       string           `json:"id"`
		} `json:"result_sets"`
		Aliases []struct {
			Company string   `json:"company"`
			Aliases []string `json:"aliases"`
		} `json:"aliases"`
		Relationship []struct {
			Text, Company string
			Match         bool
		} `json:"relationship"`
		FunctionTerms []struct {
			Function string   `json:"function"`
			Titles   []string `json:"titles"`
			Terms    []string `json:"terms"`
		} `json:"function_terms"`
		QueryTitles []struct {
			Function    string   `json:"function"`
			Titles      []string `json:"titles"`
			QueryTitles []string `json:"query_titles"`
		} `json:"query_titles"`
		Evidence []struct {
			Title, Snippet, Company string
			Terms                   []string
			Result                  []any
		} `json:"evidence"`
		Names []struct {
			Text, Name string
			Match      bool
		} `json:"names"`
		VerificationCompany []struct {
			Text, Company, Name string
			Match               bool
		} `json:"verification_company"`
		VerificationFunction []struct {
			Title, Snippet, Company, Name string
			Terms                         []string
			Match                         bool
		} `json:"verification_function"`
	}
	loadVec(t, "identity.json", &v)
	for i, c := range v.PersonIDs {
		if got := PersonEntityID(c.Company, c.Person); got != c.ID {
			t.Errorf("person_ids[%d] %q %v: got %s want %s", i, c.Company, c.Person, got, c.ID)
		}
	}
	for i, c := range v.ResultSets {
		if got := PeopleResultSetID(c.Company, c.Function, c.People); got != c.ID {
			t.Errorf("result_sets[%d]: got %s want %s", i, got, c.ID)
		}
	}
	for _, c := range v.Aliases {
		sameJSON(t, "aliases "+c.Company, companyAliases(c.Company), c.Aliases)
	}
	for _, c := range v.Relationship {
		if got := ExactCompanyRelationship(c.Text, c.Company); got != c.Match {
			t.Errorf("relationship(%q, %q) = %v want %v", c.Text, c.Company, got, c.Match)
		}
	}
	for _, c := range v.FunctionTerms {
		sameJSON(t, "terms "+c.Function, functionTerms(c.Function, c.Titles), c.Terms)
	}
	for _, c := range v.QueryTitles {
		sameJSON(t, "query_titles "+c.Function, queryTitles(c.Function, c.Titles), c.QueryTitles)
	}
	for _, c := range v.Evidence {
		st, conf, ok := FunctionEvidenceStrength(c.Title, c.Snippet, c.Terms, c.Company)
		var got []any
		if ok {
			got = []any{st, conf}
		}
		sameJSON(t, "evidence "+c.Title+"|"+c.Snippet, got, c.Result)
	}
	for _, c := range v.Names {
		if got := exactNameMatch(c.Text, c.Name); got != c.Match {
			t.Errorf("name(%q, %q) = %v want %v", c.Text, c.Name, got, c.Match)
		}
	}
	for _, c := range v.VerificationCompany {
		if got := verificationCompanyMatch(c.Text, c.Company, c.Name); got != c.Match {
			t.Errorf("vcompany(%q,%q,%q) = %v want %v", c.Text, c.Company, c.Name, got, c.Match)
		}
	}
	for _, c := range v.VerificationFunction {
		if got := verificationFunctionMatch(c.Title, c.Snippet, c.Company, c.Name, c.Terms); got != c.Match {
			t.Errorf("vfunction(%q,%q,%q,%q) = %v want %v", c.Title, c.Snippet, c.Company, c.Name, got, c.Match)
		}
	}
}
