package main

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// L9: the worker builds one leadweb (search cache, per-domain rate limiter, circuit breaker,
// proxy accounting) per browser-service configuration and every consumer — collect, the
// in-process providers (rpclocal), AI columns, source workbooks — shares it.
func TestWorkerLeadwebIsShared(t *testing.T) {
	env := map[string]string{"BROWSER_SERVICE_URL": "http://browser.internal:8080", "BROWSER_SERVICE_TOKEN": "tok-shared-1"}
	get := func(k string) string { return env[k] }
	cfg := config.Config{AppEnv: "test"}
	a, err := workerLeadweb(cfg, get)
	if err != nil || a.Web == nil || a.Browser == nil {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := workerLeadweb(cfg, get)
	if err != nil || b.Web != a.Web || b.Browser != a.Browser {
		t.Fatal("a second consumer got its own leadweb")
	}
	env["BROWSER_SERVICE_URL"] = "http://other.internal:8080"
	c, _ := workerLeadweb(cfg, get)
	if c.Web == a.Web {
		t.Fatal("a different browser-service configuration must not reuse the instance")
	}
}
