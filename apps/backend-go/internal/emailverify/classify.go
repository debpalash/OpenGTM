// Package emailverify ports the Python email verification stack:
// email_verify.py (classification, MX, SMTP RCPT probe), email_verify_cascade.py
// (normalised verdict cascade), email_deliverability.py (confidence tag) and
// email_waterfall.py (email finder waterfall). Every outbound connection goes
// through the egress dial guard and DNS/SMTP are injectable (Deps), so tests
// never touch the network.
package emailverify

import (
	"bufio"
	"bytes"
	"embed"
	"regexp"
	"strings"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// The three data files are byte copies of legacy/python/.../enrichment/data;
// TestClassifyVectors pins their count and sha256.
//
//go:embed data/role_accounts.txt data/free_providers.txt data/disposable_domains.txt
var dataFS embed.FS

var (
	loadOnce  sync.Once
	roleSet   map[string]struct{}
	freeSet   map[string]struct{}
	disposSet map[string]struct{}
)

func loadSet(name string) map[string]struct{} {
	raw, err := dataFS.ReadFile("data/" + name)
	if err != nil {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, bytes.Count(raw, []byte{'\n'})+1)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.ToLower(providers.PyStrip(sc.Text())); line != "" {
			out[line] = struct{}{}
		}
	}
	return out
}

func loadAll() {
	loadOnce.Do(func() {
		roleSet = loadSet("role_accounts.txt")
		freeSet = loadSet("free_providers.txt")
		disposSet = loadSet("disposable_domains.txt")
	})
}

func roleAccounts() map[string]struct{}      { loadAll(); return roleSet }
func freeProviders() map[string]struct{}     { loadAll(); return freeSet }
func disposableDomains() map[string]struct{} { loadAll(); return disposSet }

var emailRE = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)

// Classification is EmailClassification: a network-free read of an address.
type Classification struct {
	Email        string
	ValidSyntax  bool
	LocalPart    string
	Domain       string
	IsRole       bool // info@, sales@, hr@: not a person
	IsFree       bool // gmail/yahoo/...: weak B2B signal
	IsDisposable bool // throwaway domain
}

// IsPersonalCorporate is true for a real person at a real company domain.
func (c Classification) IsPersonalCorporate() bool {
	return c.ValidSyntax && !(c.IsRole || c.IsFree || c.IsDisposable)
}

// Classify is classify_email: strip, lower-case, split at the first "@".
func Classify(email string) Classification {
	email = strings.ToLower(providers.PyStrip(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok {
		return Classification{Email: email}
	}
	_, isRole := roleAccounts()[local]
	_, isFree := freeProviders()[domain]
	_, isDisp := disposableDomains()[domain]
	return Classification{
		Email: email, ValidSyntax: emailRE.MatchString(email), LocalPart: local, Domain: domain,
		IsRole: isRole, IsFree: isFree, IsDisposable: isDisp,
	}
}
