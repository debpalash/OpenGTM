// Package personintel ports the person-intelligence core of
// legacy/python/apps/api/services/person_intel.py: LinkedIn username parsing,
// DuckDuckGo Lite search, snippet extractors and GitHub enrichment. Every
// outbound request goes through a caller-supplied egress (SSRF-guarded)
// client via providers.Do: no redirects, 1 MiB body cap, 15 s timeout.
package personintel

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Obj is an insertion-ordered string map (a Python dict[str, str]): JSON key
// order is first-insertion order.
type Obj struct {
	keys []string
	vals map[string]string
}

// Set assigns k, keeping its original position when it already exists.
func (o *Obj) Set(k, v string) {
	if o.vals == nil {
		o.vals = map[string]string{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Get returns the value for k.
func (o *Obj) Get(k string) (string, bool) {
	if o == nil {
		return "", false
	}
	v, ok := o.vals[k]
	return v, ok
}

// Has reports whether k is present.
func (o *Obj) Has(k string) bool { _, ok := o.Get(k); return ok }

// Keys returns the keys in insertion order.
func (o *Obj) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

// Len is the number of keys.
func (o *Obj) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// MarshalJSON renders keys in insertion order ({} when empty or nil).
func (o *Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	if o != nil {
		for i, k := range o.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := encodeNoEscape(k)
			vb, _ := encodeNoEscape(o.vals[k])
			b.Write(kb)
			b.WriteByte(':')
			b.Write(vb)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads a JSON object of strings preserving key order.
func (o *Obj) UnmarshalJSON(data []byte) error {
	*o = Obj{}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("personintel: expected object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return err
		}
		o.Set(kt.(string), v)
	}
	_, err := dec.Token()
	return err
}

// Result is one search result (a Python dict). Category and the scraped_*
// fields are added later in the pipeline and omitted while empty.
type Result struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Source   string `json:"source"`
	Category string `json:"category,omitempty"`

	ScrapedText      string   `json:"scraped_text,omitempty"`
	ScrapedEmails    []string `json:"scraped_emails,omitempty"`
	ScrapedWordCount int      `json:"scraped_word_count,omitempty"`
}

// Entry is an article or mention of a profile.
type Entry struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"`
}

// Profile mirrors PersonProfile; field order is the model_dump key order.
type Profile struct {
	LinkedInURL string   `json:"linkedin_url"`
	Username    string   `json:"username"`
	Name        string   `json:"name"`
	Headline    string   `json:"headline"`
	Location    string   `json:"location"`
	Summary     string   `json:"summary"`
	Emails      []string `json:"emails"`
	SocialLinks *Obj     `json:"social_links"`
	Articles    []Entry  `json:"articles"`
	Mentions    []Entry  `json:"mentions"`
	Companies   []string `json:"companies"`
	Education   []string `json:"education"`
	Skills      []string `json:"skills"`
	RawSources  []Result `json:"raw_sources"`
	Status      string   `json:"status"`
}

// NewProfile returns a profile with the PersonProfile defaults.
func NewProfile(linkedinURL string) *Profile {
	return &Profile{LinkedInURL: linkedinURL, SocialLinks: &Obj{}, Status: "pending"}
}

// MarshalJSON renders nil collections as [] / {} like the pydantic defaults.
func (p Profile) MarshalJSON() ([]byte, error) {
	type plain Profile
	q := plain(p)
	if q.Emails == nil {
		q.Emails = []string{}
	}
	if q.SocialLinks == nil {
		q.SocialLinks = &Obj{}
	}
	if q.Articles == nil {
		q.Articles = []Entry{}
	}
	if q.Mentions == nil {
		q.Mentions = []Entry{}
	}
	if q.Companies == nil {
		q.Companies = []string{}
	}
	if q.Education == nil {
		q.Education = []string{}
	}
	if q.Skills == nil {
		q.Skills = []string{}
	}
	if q.RawSources == nil {
		q.RawSources = []Result{}
	}
	return encodeNoEscape(q)
}

// JSON renders the profile without HTML escaping (Python json.dumps does not
// escape <, > or &).
func (p *Profile) JSON() ([]byte, error) { return encodeNoEscape(p) }

func encodeNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func (p *Profile) social() *Obj {
	if p.SocialLinks == nil {
		p.SocialLinks = &Obj{}
	}
	return p.SocialLinks
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
