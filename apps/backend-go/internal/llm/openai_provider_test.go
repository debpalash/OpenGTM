package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The first-party openai provider posts to api.openai.com/v1/chat/completions
// with a Bearer key and max_completion_tokens (max_tokens is deprecated and
// rejected by o-series models):
// https://platform.openai.com/docs/api-reference/chat/create
func TestOpenAIProviderRequestShape(t *testing.T) {
	var gotURL, gotAuth, gotBody string
	hc := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotURL, gotAuth, gotBody = r.URL.String(), r.Header.Get("Authorization"), string(b)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	})}
	c := newTestClient(t, mapSettings{"LLM_DEFAULT_PROVIDER": "openai", "OPENAI_API_KEY": "sk-test-123"}, hc, nil)
	out, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p", MaxTokens: 77})
	if err != nil || out != "hi" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if gotURL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("url = %q", gotURL)
	}
	if gotAuth != "Bearer sk-test-123" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"max_completion_tokens":77`) || strings.Contains(gotBody, `"max_tokens"`) {
		t.Errorf("token param wrong: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"model":"gpt-6-luna"`) {
		t.Errorf("default model wrong: %s", gotBody)
	}
}

// openai is paid: it never joins the failover chain unless chosen explicitly,
// needs a key, and the custom_openai special cases do not apply to it.
func TestOpenAIProviderExplicitOnlyAndNoCustomQuirks(t *testing.T) {
	for _, id := range ProviderOrder {
		if id == "openai" {
			t.Fatal("openai must not be in ProviderOrder")
		}
	}
	ctx := context.Background()
	c := newTestClient(t, mapSettings{"GROQ_API_KEY": "g", "OPENAI_API_KEY": "sk-test-123"}, nil, nil)
	for _, p := range c.Providers(ctx) {
		if p.ID == "openai" {
			t.Fatal("unselected openai must not receive traffic")
		}
	}
	c = newTestClient(t, mapSettings{"LLM_DEFAULT_PROVIDER": "openai"}, nil, nil)
	if _, ok := c.providerConfig(ctx, "openai"); ok {
		t.Fatal("openai must require a key")
	}
	c = newTestClient(t, mapSettings{"LLM_DEFAULT_PROVIDER": "openai", "OPENAI_API_KEY": "k"}, nil, nil)
	p, ok := c.providerConfig(ctx, "openai")
	if !ok || p.Timeout != 0 || p.ExtraBody != nil || p.TokenParam != "max_completion_tokens" {
		t.Fatalf("unexpected provider: %+v ok=%v", p, ok)
	}
	if c.Providers(ctx)[0].ID != "openai" {
		t.Fatal("explicit default must lead")
	}
}
