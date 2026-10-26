package enrichcache

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

type keyVectors struct {
	Norm struct {
		Domain  []struct{ In, Out string } `json:"domain"`
		Company []struct{ In, Out string } `json:"company"`
		Name    []struct{ In, Out string } `json:"name"`
	} `json:"norm"`
	CanonicalKey []struct {
		Name string `json:"name"`
		Lead struct {
			Website       string `json:"website"`
			Company       string `json:"company"`
			ContactPerson string `json:"contact_person"`
		} `json:"lead"`
		Keys map[string]*string `json:"keys"`
	} `json:"canonical_key"`
	EmailKey []struct {
		Email    *string `json:"email"`
		Verifier string  `json:"verifier"`
		Key      string  `json:"key"`
	} `json:"email_key"`
}

func loadKeyVectors(t *testing.T) keyVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/key_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v keyVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCanonicalKeyMatchesVectors(t *testing.T) {
	v := loadKeyVectors(t)
	if len(v.CanonicalKey) < 20 || len(v.Norm.Domain) == 0 || len(v.Norm.Company) == 0 || len(v.Norm.Name) == 0 {
		t.Fatalf("vectors look empty: %d keys", len(v.CanonicalKey))
	}
	for _, c := range v.Norm.Domain {
		if got := normDomain(c.In); got != c.Out {
			t.Errorf("normDomain(%q)=%q want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Norm.Company {
		if got := normCompany(c.In); got != c.Out {
			t.Errorf("normCompany(%q)=%q want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Norm.Name {
		if got := normName(c.In); got != c.Out {
			t.Errorf("normName(%q)=%q want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.CanonicalKey {
		lead := Lead{Website: c.Lead.Website, Company: c.Lead.Company, ContactPerson: c.Lead.ContactPerson}
		for field, want := range c.Keys {
			got, ok := CanonicalKey(field, lead)
			if want == nil {
				if ok || got != "" {
					t.Errorf("%s/%q: got (%q,%v) want not ok", c.Name, field, got, ok)
				}
				continue
			}
			if !ok || got != *want {
				t.Errorf("%s/%q: got (%q,%v) want %q", c.Name, field, got, ok, *want)
			}
		}
	}
}

func TestEmailKeyMatchesVectors(t *testing.T) {
	for _, c := range loadKeyVectors(t).EmailKey {
		email := ""
		if c.Email != nil {
			email = *c.Email
		}
		if got := EmailKey(email, c.Verifier); got != c.Key {
			t.Errorf("EmailKey(%q,%q)=%q want %q", email, c.Verifier, got, c.Key)
		}
	}
}

func TestEmailCacheKeyLowercasedHash(t *testing.T) {
	// sha256("a@b.co") + ":" + verifier; case and surrounding whitespace do not
	// change the hash, the verifier does.
	const h = "80305c9bb1bb2480e03894350e0a8a366dcbdeb302e69e0817aa0743abd77054"
	for _, in := range []string{"a@b.co", "A@B.CO", "  A@b.Co\t"} {
		if got := EmailKey(in, "reacher"); got != h+":reacher" {
			t.Errorf("EmailKey(%q)=%q", in, got)
		}
	}
	if EmailKey("a@b.co", "smtp") == EmailKey("a@b.co", "reacher") || EmailKey("x@y.co", "reacher") == h+":reacher" {
		t.Fatal("key collision")
	}
}

func TestTTLTables(t *testing.T) {
	cases := map[string]int{"email": 30, "phone": 60, "linkedin_url": 90, "company_size": 90, "description": 180, "founded_year": 365, "city": 30, "": 30}
	for f, days := range cases {
		if got := TTLFor(f); got != time.Duration(days)*24*time.Hour {
			t.Errorf("TTLFor(%q)=%v want %d days", f, got, days)
		}
	}
	if EmailTTL("valid") != 14*24*time.Hour || EmailTTL("invalid") != 14*24*time.Hour ||
		EmailTTL("catch_all") != 14*24*time.Hour || EmailTTL("unknown") != 24*time.Hour || EmailTTL("") != 24*time.Hour {
		t.Fatal("EmailTTL mismatch")
	}
}
