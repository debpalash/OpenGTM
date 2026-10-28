// Package ambitionbox is the HTTP client of AmbitionBox's public service
// gateway (port of services/leadgen/ambitionbox.py): company search, pages,
// jobs, detail, similar companies and multi-page collection.
//
// Only the HTTP part lives here. The gateway needs no browser or scraper tier
// in Python (plain JSON over aiohttp with fixed appid/systemid headers), so
// nothing of this client needs the browser service; the import job that
// drives it is ambitionimport.
package ambitionbox

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is an insertion-ordered JSON object (Python dict key order).
type Obj = wbcrud.Obj

// ABCompany is a parsed company card. Fields that Python passes through
// untouched (ids, counts, flags) stay `any` so odd gateway values (a string id,
// a null flag) round-trip exactly.
type ABCompany struct {
	CompanyID       any
	Name            string
	ShortName       string
	URLName         string
	LogoURL         string
	Industry        string
	Rating          any // nil = unrated (NOT 0.0)
	ReviewCount     any // nil = unknown
	JobsCount       any
	SalariesCount   any
	InterviewsCount any
	EmployeeCount   string
	TopLocation     string
	TotalLocations  any
	IsVerified      any
	CompanyType     string
	HighlyRatedFor  []any // []*Obj{name, rating}
	CriticallyRated []any
}

// ABJob is a parsed job.
type ABJob struct {
	JobID         string
	Title         string
	Company       string
	CompanyID     any
	CompanyRating any
	JobProfile    string
	Locations     any
	MinExp        any
	MaxExp        any
	Skills        any
	PostedOn      string
	Portal        string
	JdpURL        string
}

const siteBase = "https://www.ambitionbox.com"

func get(o *Obj, k string) any {
	v, _ := o.Get(k)
	return v
}

// text is `card.get(k, "")` for a text field; null or non-text counts as "".
func text(o *Obj, k string) string {
	s, _ := get(o, k).(string)
	return s
}

// truthy is Python truthiness of a decoded JSON value.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int64:
		return t != 0
	case float64:
		return t != 0
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) > 0
	case *Obj:
		return t.Len() > 0
	}
	return true
}

// orZero is `v or 0`.
func orZero(v any) any {
	if truthy(v) {
		return v
	}
	return int64(0)
}

// round1 is Python's round(x, 1): an int stays an int, a float is rounded
// half-to-even on its exact binary value (FormatFloat 'f' does the same).
func round1(v any) (any, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n, nil
			}
		}
		f, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("unsupported rating %q", s)
		}
		r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
		return r, nil
	}
	return nil, fmt.Errorf("unsupported rating type %s", providers.TypeName(v))
}

func objOr(o *Obj, k string) (*Obj, error) {
	v := get(o, k)
	if !truthy(v) {
		return wbcrud.NewObj(), nil
	}
	if d, ok := v.(*Obj); ok {
		return d, nil
	}
	return nil, fmt.Errorf("%s is not an object", k)
}

func ratedFor(card *Obj, key string) ([]any, error) {
	out := []any{}
	v := get(card, key)
	if !truthy(v) {
		return out, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a list", key)
	}
	for _, e := range list {
		r, ok := e.(*Obj)
		if !ok {
			return nil, fmt.Errorf("%s entry is not an object", key)
		}
		for _, need := range []string{"name", "ratings"} {
			if _, has := r.Get(need); !has {
				return nil, fmt.Errorf("'%s'", need) // Python KeyError text
			}
		}
		d := wbcrud.NewObj()
		d.Set("name", get(r, "name"))
		d.Set("rating", get(r, "ratings"))
		out = append(out, d)
	}
	return out, nil
}

// CompanyFromAPI is ABCompany.from_api.
func CompanyFromAPI(card *Obj) (ABCompany, error) {
	loc, err := objOr(card, "topLocationDetail")
	if err != nil {
		return ABCompany{}, err
	}
	tag, err := objOr(card, "tag")
	if err != nil {
		return ABCompany{}, err
	}
	c := ABCompany{
		CompanyID: orDefault(card, "companyId", int64(0)),
		Name:      text(card, "name"), ShortName: text(card, "shortName"), URLName: text(card, "urlName"),
		LogoURL: text(card, "logoUrl"), Industry: text(card, "primaryIndustry"),
		JobsCount: orZero(get(card, "jobsCount")), SalariesCount: orZero(get(card, "salariesCount")),
		InterviewsCount: orZero(get(card, "interviewsCount")),
		TopLocation:     text(loc, "name"), TotalLocations: orZero(get(loc, "totalLocationsCount")),
		IsVerified:  orDefault(card, "isVerifiedEmployer", false),
		CompanyType: text(tag, "name"),
	}
	if c.EmployeeCount = text(card, "totalEmployeesIndia"); c.EmployeeCount == "" {
		c.EmployeeCount = text(card, "totalEmployees")
	}
	if r := get(card, "companyRating"); r != nil {
		if c.Rating, err = round1(r); err != nil {
			return ABCompany{}, err
		}
	}
	c.ReviewCount = get(card, "reviewCount") // null stays null
	if c.HighlyRatedFor, err = ratedFor(card, "highlyRatedFor"); err != nil {
		return ABCompany{}, err
	}
	if c.CriticallyRated, err = ratedFor(card, "criticallyRatedFor"); err != nil {
		return ABCompany{}, err
	}
	return c, nil
}

// orDefault is `card.get(k, def)`: a present null stays null.
func orDefault(o *Obj, k string, def any) any {
	if v, ok := o.Get(k); ok {
		return v
	}
	return def
}

// ToDict is ABCompany.to_dict with its key order.
func (c ABCompany) ToDict() *Obj {
	o := wbcrud.NewObj()
	o.Set("company_id", c.CompanyID)
	o.Set("name", c.Name)
	o.Set("short_name", c.ShortName)
	o.Set("url_name", c.URLName)
	o.Set("logo_url", c.LogoURL)
	o.Set("industry", c.Industry)
	o.Set("rating", c.Rating)
	o.Set("review_count", c.ReviewCount)
	o.Set("jobs_count", c.JobsCount)
	o.Set("salaries_count", c.SalariesCount)
	o.Set("interviews_count", c.InterviewsCount)
	o.Set("employee_count", c.EmployeeCount)
	o.Set("top_location", c.TopLocation)
	o.Set("total_locations", c.TotalLocations)
	o.Set("is_verified", c.IsVerified)
	o.Set("company_type", c.CompanyType)
	o.Set("highly_rated_for", c.HighlyRatedFor)
	o.Set("critically_rated_for", c.CriticallyRated)
	o.Set("profile_url", siteBase+"/overview/"+c.URLName+"-overview")
	return o
}

// JobFromAPI is ABJob.from_api.
func JobFromAPI(j *Obj) (ABJob, error) {
	rating, err := round1(orZero(get(j, "companyRating")))
	if err != nil {
		return ABJob{}, err
	}
	job := ABJob{
		JobID: text(j, "jobId"), Title: text(j, "title"), Company: text(j, "company"),
		CompanyID: orDefault(j, "companyId", int64(0)), CompanyRating: rating, JobProfile: text(j, "jobProfile"),
		Locations: orDefault(j, "locations", []any{}), MinExp: orZero(get(j, "minExp")), MaxExp: orZero(get(j, "maxExp")),
		Skills: orDefault(j, "skills", []any{}), PostedOn: text(j, "postedOn"), Portal: text(j, "portal"),
	}
	if truthy(get(j, "jdpUrl")) {
		job.JdpURL = siteBase + pyStr(get(j, "jdpUrl"))
	}
	return job, nil
}

// pyStr is str(v) for the scalar values an f-string meets here.
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return fmt.Sprint(v)
}

// ToDict is ABJob.to_dict with its key order.
func (j ABJob) ToDict() *Obj {
	exp := pyStr(j.MinExp) + "+ yrs"
	if truthy(j.MaxExp) {
		exp = pyStr(j.MinExp) + "-" + pyStr(j.MaxExp) + " yrs"
	}
	o := wbcrud.NewObj()
	o.Set("job_id", j.JobID)
	o.Set("title", j.Title)
	o.Set("company", j.Company)
	o.Set("company_id", j.CompanyID)
	o.Set("company_rating", j.CompanyRating)
	o.Set("job_profile", j.JobProfile)
	o.Set("locations", j.Locations)
	o.Set("experience", exp)
	o.Set("skills", j.Skills)
	o.Set("posted_on", j.PostedOn)
	o.Set("portal", j.Portal)
	o.Set("url", j.JdpURL)
	return o
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify is _slugify: display name -> gateway filter slug.
func slugify(value string) string {
	s := strings.ToLower(dotI.Replace(providers.PyStrip(value)))
	s = strings.ReplaceAll(s, "&", "and")
	return strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
}

var industryAliases = map[string]string{
	"hr": "recruitment", "hr-services": "recruitment", "human-resource": "recruitment", "human-resources": "recruitment",
	"human-resources-recruitment": "recruitment", "human-resources-and-recruitment": "recruitment",
	"hr-recruitment": "recruitment", "hr-and-recruitment": "recruitment", "recruiting": "recruitment",
	"staffing": "recruitment", "staffing-and-recruiting": "recruitment", "recruitment-and-staffing": "recruitment",
	"recruitment-staffing": "recruitment", "saas": "software-product", "software-as-a-service": "software-product",
}

// industrySlug is _industry_slug.
func industrySlug(value string) string {
	s := slugify(value)
	if a, ok := industryAliases[s]; ok {
		return a
	}
	return s
}
