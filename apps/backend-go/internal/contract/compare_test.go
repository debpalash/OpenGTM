package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeTB records failures instead of failing the real test.
type fakeTB struct {
	testing.TB
	failed bool
	msg    string
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.failed = true
	f.msg += strings.TrimSpace(fmt.Sprintf(format, args...))
}
func (f *fakeTB) Fatalf(format string, args ...any) { f.Errorf(format, args...) }

func resp(status int, headers map[string]string, body string) Resp {
	return Resp{Status: status, Headers: headers, Body: json.RawMessage(body)}
}

func TestAssertEqualIgnoresKeyOrder(t *testing.T) {
	f := &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"a": 1, "b": [1, {"x":1,"y":2}]}`), resp(200, nil, `{"b":[1,{"y":2,"x":1}],"a":1}`))
	if f.failed {
		t.Fatalf("unexpected failure: %s", f.msg)
	}
}

func TestAssertEqualFailsOnStatus(t *testing.T) {
	f := &fakeTB{TB: t}
	AssertEqual(f, resp(401, nil, `{}`), resp(200, nil, `{}`))
	if !f.failed || !strings.Contains(f.msg, "status") {
		t.Fatalf("want status failure, got %q", f.msg)
	}
}

func TestAssertEqualReportsFirstDiffPath(t *testing.T) {
	f := &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"a":{"b":[1,2]}}`), resp(200, nil, `{"a":{"b":[1,3]}}`))
	if !f.failed || !strings.Contains(f.msg, "$.a.b[1]") {
		t.Fatalf("want path $.a.b[1], got %q", f.msg)
	}
}

func TestIgnorePathsSkipsGeneratedId(t *testing.T) {
	f := &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"id":7,"name":"x"}`), resp(200, nil, `{"id":99,"name":"x"}`), IgnorePaths("$.id"))
	if f.failed {
		t.Fatalf("unexpected failure: %s", f.msg)
	}
	f = &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"id":7,"name":"y"}`), resp(200, nil, `{"id":99,"name":"x"}`), IgnorePaths("$.id"))
	if !f.failed {
		t.Fatal("name difference must still fail")
	}
}

func TestIgnoreShapeOfRejectsTypeChange(t *testing.T) {
	f := &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"t":"abc"}`), resp(200, nil, `{"t":"zzz"}`), IgnoreShapeOf("$.t"))
	if f.failed {
		t.Fatalf("same type must pass: %s", f.msg)
	}
	f = &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"t":5}`), resp(200, nil, `{"t":"zzz"}`), IgnoreShapeOf("$.t"))
	if !f.failed {
		t.Fatal("string vs number must fail")
	}
}

func TestIgnoreShapeOfNaiveTimestamp(t *testing.T) {
	want := resp(200, nil, `{"created_at":"2026-10-26T10:00:00.123456"}`)
	f := &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"created_at":"2026-10-27T11:01:02"}`), want, IgnoreShapeOf("$.created_at"))
	if f.failed {
		t.Fatalf("naive ISO must pass: %s", f.msg)
	}
	f = &fakeTB{TB: t}
	AssertEqual(f, resp(200, nil, `{"created_at":"2026-10-27T11:01:02Z"}`), want, IgnoreShapeOf("$.created_at"))
	if !f.failed {
		t.Fatal("Z suffix must fail the timestamp shape")
	}
}

func TestHeaderAllowListOnly(t *testing.T) {
	f := &fakeTB{TB: t}
	AssertEqual(f,
		resp(200, map[string]string{"Content-Type": "application/json", "X-Request-Id": "a", "Date": "x"}, `{}`),
		resp(200, map[string]string{"content-type": "application/json", "x-request-id": "b"}, `{}`))
	if f.failed {
		t.Fatalf("non-allow-listed headers must be ignored: %s", f.msg)
	}
	f = &fakeTB{TB: t}
	AssertEqual(f,
		resp(401, map[string]string{"WWW-Authenticate": "Basic"}, `{}`),
		resp(401, map[string]string{"www-authenticate": "Bearer"}, `{}`))
	if !f.failed || !strings.Contains(f.msg, "www-authenticate") {
		t.Fatalf("want www-authenticate failure, got %q", f.msg)
	}
	f = &fakeTB{TB: t}
	AssertEqual(f, resp(401, nil, `{}`), resp(401, map[string]string{"www-authenticate": "Bearer"}, `{}`))
	if !f.failed {
		t.Fatal("missing allow-listed header must fail")
	}
}

func TestLoadReadsFixture(t *testing.T) {
	fx := Load(t, "auth/token_success")
	if fx.Request.Method != "POST" || fx.Response.Status != 200 {
		t.Fatalf("unexpected fixture: %+v", fx.Request)
	}
}

func TestRecordedFixturesAgainstThemselves(t *testing.T) {
	fx := Load(t, "auth/me_ok")
	AssertEqual(t, fx.Response, fx.Response)
	bad := Load(t, "auth/token_bad_password")
	if bad.Response.Status != 401 || bad.Response.Headers["www-authenticate"] != "Bearer" {
		t.Fatalf("unexpected recorded fixture: %+v", bad.Response)
	}
}
