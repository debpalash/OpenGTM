package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// sharedWeb is the worker's one leadweb: the search cache (salted per workspace), per-domain rate
// limiter, circuit breaker and proxy accounting are process-wide, so collect, the in-process
// providers (rpclocal), AI columns and source workbooks hitting the same site or engine share one
// budget instead of each getting the full rate (review 3a L9).
type sharedWeb struct {
	Web     *leadweb.Web
	Browser *browserclient.Client
}

var (
	sharedWebMu sync.Mutex
	sharedWebs  = map[[32]byte]sharedWeb{}
)

// workerLeadweb returns the process's leadweb for the browser-service and leadweb settings get
// resolves, building it on first use. The Browser client is unconfigured (its calls fail with
// browserclient.ErrUnavailable) when BROWSER_SERVICE_URL / _TOKEN are unset.
func workerLeadweb(cfg config.Config, get func(string) string) (sharedWeb, error) {
	if get == nil {
		get = config.Getenv
	}
	sc := config.ScrapeFrom(get)
	lw, err := config.LeadwebFrom(get)
	if err != nil {
		return sharedWeb{}, err
	}
	// Secret formatting is deliberately redacted. Fingerprint the actual
	// credentials separately so different provider accounts never share a cache.
	credentials := sha256.New()
	for _, secret := range []config.Secret{lw.SerpAPIKey, lw.BingKey, lw.GoogleCSEID, lw.GoogleCSEKey,
		lw.BraveKey, lw.SerpingAPIKey, lw.SerplyKey, lw.FirecrawlKey, lw.SpicrawlKey, lw.ProxyManagerURL, lw.ProxyList} {
		_, _ = fmt.Fprintf(credentials, "%d:", len(secret.Reveal()))
		_, _ = credentials.Write([]byte(secret.Reveal()))
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%#v\x00%x", sc.BrowserURL, sc.BrowserToken.Reveal(),
		cfg.AppEnv, strings.Join(cfg.EgressAllowHosts, ","), lw, credentials.Sum(nil))))
	sharedWebMu.Lock()
	defer sharedWebMu.Unlock()
	if w, ok := sharedWebs[key]; ok {
		return w, nil
	}
	bc := browserclient.New(sc.BrowserURL, sc.BrowserToken.Reveal(), nil)
	w := sharedWeb{Web: leadweb.New(lw, bc, cfg.AppEnv, cfg.EgressAllowHosts), Browser: bc}
	sharedWebs[key] = w
	return w, nil
}
