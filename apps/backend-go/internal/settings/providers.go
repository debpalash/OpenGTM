package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maskedUnknown   = "••••••••"
	testPrompt      = "Say 'hello' in exactly one word."
	testTimeout     = 20 * time.Second
	maxTestRespBody = 1 << 20
)

// Providers serves the LLM provider settings routes.
type Providers struct {
	Reg  *Registry
	App  *App
	HTTP *http.Client // egress-guarded client
	Log  *slog.Logger
}

// ProviderView is one entry of GET /api/settings/providers.
type ProviderView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Icon             string `json:"icon"`
	Configured       bool   `json:"configured"`
	APIKeyMasked     string `json:"api_key_masked"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	DefaultModel     string `json:"default_model"`
	DocsURL          string `json:"docs_url"`
	FreeTier         string `json:"free_tier"`
	IsDefault        bool   `json:"is_default"`
	OpenAICompatible bool   `json:"openai_compatible"`
}

// ProviderUpdate is the PUT body; nil means "not provided".
type ProviderUpdate struct {
	APIKey     *string
	BaseURL    *string
	Model      *string
	SetDefault bool
}

// MaskKey shows the first 8 and last 4 characters of a key of 16+ characters.
func MaskKey(key string) string {
	r := []rune(key)
	if len(r) < 16 {
		if key != "" {
			return maskedUnknown
		}
		return ""
	}
	return string(r[:8]) + "••••" + string(r[len(r)-4:])
}

func (p *Providers) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Providers) urlAndModel(ctx context.Context, prov Provider) (string, string) {
	base, model := prov.DefaultURL, prov.DefaultModel
	if prov.EnvURL != "" {
		if v, err := p.App.Get(ctx, prov.EnvURL, prov.DefaultURL); err == nil {
			base = v
		}
	}
	if prov.EnvModel != "" {
		if v, err := p.App.Get(ctx, prov.EnvModel, prov.DefaultModel); err == nil {
			model = v
		}
	}
	return base, model
}

func (p *Providers) defaultProvider(ctx context.Context) string {
	v, err := p.App.Get(ctx, "LLM_DEFAULT_PROVIDER", "openrouter")
	if err != nil {
		return "openrouter"
	}
	return v
}

// List returns every provider and the default provider id. A stored key that
// cannot be decrypted reports configured with a fixed mask; its name is logged
// (never its value) and the rest of the list is unaffected.
func (p *Providers) List(ctx context.Context) ([]ProviderView, string, error) {
	def := p.defaultProvider(ctx)
	ids := p.Reg.Providers.Keys()
	out := make([]ProviderView, 0, len(ids))
	for _, id := range ids {
		prov, _ := p.Reg.Provider(id)
		key, err := p.App.Get(ctx, prov.EnvKey, "")
		v := ProviderView{ID: id, Name: prov.Name, Icon: prov.Icon, DefaultModel: prov.DefaultModel,
			DocsURL: prov.Docs, FreeTier: prov.FreeTier, IsDefault: id == def, OpenAICompatible: prov.OpenAICompatible}
		if err != nil {
			p.log().Error("settings: stored provider key undecryptable", "key", prov.EnvKey)
			v.Configured, v.APIKeyMasked = true, maskedUnknown
		} else {
			v.Configured, v.APIKeyMasked = key != "", MaskKey(key)
		}
		v.BaseURL, v.Model = p.urlAndModel(ctx, prov)
		out = append(out, v)
	}
	return out, def, nil
}

// Update writes the provided fields; found is false for an unknown provider.
func (p *Providers) Update(ctx context.Context, id string, u ProviderUpdate) (found bool, err error) {
	prov, ok := p.Reg.Provider(id)
	if !ok {
		return false, nil
	}
	values := map[string]string{}
	if u.APIKey != nil {
		values[prov.EnvKey] = *u.APIKey
	}
	if u.BaseURL != nil && prov.EnvURL != "" {
		values[prov.EnvURL] = *u.BaseURL
	}
	if u.Model != nil && prov.EnvModel != "" {
		values[prov.EnvModel] = *u.Model
	}
	if u.SetDefault {
		values["LLM_DEFAULT_PROVIDER"] = id
	}
	return true, p.App.SetMany(ctx, values)
}

func errResult(msg string) map[string]any { return map[string]any{"status": "error", "error": msg} }

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// providerErrorMessage is _provider_error_message.
func providerErrorMessage(data any, raw string) string {
	switch d := data.(type) {
	case []any:
		for _, item := range d {
			if m := providerErrorMessage(item, ""); m != "" {
				return m
			}
		}
		return truncRunes(raw, 200)
	case map[string]any:
		switch e := d["error"].(type) {
		case map[string]any:
			for _, k := range []string{"message", "status", "code"} {
				if v := truthyStr(e[k]); v != "" {
					return truncRunes(v, 200)
				}
			}
			return ""
		case string:
			return truncRunes(e, 200)
		}
		if m, ok := d["message"].(string); ok {
			return truncRunes(m, 200)
		}
	}
	return truncRunes(raw, 200)
}

func truthyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == 0 {
			return ""
		}
		return fmt.Sprint(x)
	case bool:
		if !x {
			return ""
		}
		return "True"
	}
	return fmt.Sprint(v)
}

// Test sends one short prompt to the provider. found is false for an unknown
// provider; err is set only when the stored key cannot be decrypted.
func (p *Providers) Test(ctx context.Context, id string) (res map[string]any, found bool, err error) {
	prov, ok := p.Reg.Provider(id)
	if !ok {
		return nil, false, nil
	}
	key, err := p.App.Get(ctx, prov.EnvKey, "")
	if err != nil {
		return nil, true, err
	}
	if key == "" {
		return errResult("API key not configured"), true, nil
	}
	base, model := p.urlAndModel(ctx, prov)
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	return p.send(ctx, id, prov, key, base, model), true, nil
}

func (p *Providers) post(ctx context.Context, target string, hdr map[string]string, body any) (int, []byte, error) {
	st, raw, err := p.doPost(ctx, target, hdr, body)
	if err != nil {
		// Never surface the URL or any credential header value.
		msg := err.Error()
		for _, v := range hdr {
			if len(v) >= 8 {
				msg = strings.ReplaceAll(msg, v, "[redacted]")
				msg = strings.ReplaceAll(msg, strings.TrimPrefix(v, "Bearer "), "[redacted]")
			}
		}
		msg = strings.ReplaceAll(msg, target, "[url]")
		err = errors.New(msg)
	}
	return st, raw, err
}

func (p *Providers) doPost(ctx context.Context, target string, hdr map[string]string, body any) (int, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return 0, nil, unwrapURLErr(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return 0, nil, unwrapURLErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTestRespBody))
	if err != nil {
		return 0, nil, unwrapURLErr(err)
	}
	return resp.StatusCode, raw, nil
}

// unwrapURLErr drops the request URL (which can carry ?key=<secret>).
func unwrapURLErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func asMap(raw []byte) (map[string]any, any) {
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return nil, nil
	}
	m, _ := data.(map[string]any)
	return m, data
}

func (p *Providers) send(ctx context.Context, id string, prov Provider, key, base, model string) map[string]any {
	switch {
	case prov.OpenAICompatible:
		body := map[string]any{"model": model,
			"messages": []map[string]string{{"role": "user", "content": testPrompt}}}
		if id == "cerebras" {
			body["max_completion_tokens"] = 50
		} else {
			body["max_tokens"] = 50
		}
		status, raw, err := p.post(ctx, base+"/chat/completions", map[string]string{"Authorization": "Bearer " + key}, body)
		if err != nil {
			return errResult(err.Error())
		}
		m, data := asMap(raw)
		if status == http.StatusOK && m != nil {
			msg := map[string]any{}
			if ch, ok := m["choices"].([]any); ok {
				if len(ch) == 0 {
					return errResult("list index out of range")
				}
				if c0, ok := ch[0].(map[string]any); ok {
					if mm, ok := c0["message"].(map[string]any); ok {
						msg = mm
					}
				}
			}
			text := truthyStr(msg["content"])
			if text == "" {
				text = truthyStr(msg["reasoning"])
			}
			var actual any = model
			if v, ok := m["model"]; ok {
				actual = v
			}
			return map[string]any{"status": "ok", "response": truncRunes(strings.TrimSpace(text), 200), "model": actual}
		}
		if msg := providerErrorMessage(data, string(raw)); msg != "" {
			return errResult(msg)
		}
		return errResult(fmt.Sprintf("Provider returned HTTP %d", status))
	case id == "anthropic":
		status, raw, err := p.post(ctx, strings.TrimRight(base, "/")+"/v1/messages",
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"},
			map[string]any{"model": model, "max_tokens": 50,
				"messages": []map[string]string{{"role": "user", "content": testPrompt}}})
		if err != nil {
			return errResult(err.Error())
		}
		m, _ := asMap(raw)
		if m == nil {
			return errResult("Expecting value: line 1 column 1 (char 0)")
		}
		if status == http.StatusOK {
			var sb strings.Builder
			blocks, _ := m["content"].([]any)
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
					if t, ok := bm["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			var actual any = model
			if v, ok := m["model"]; ok {
				actual = v
			}
			return map[string]any{"status": "ok", "response": truncRunes(strings.TrimSpace(sb.String()), 200), "model": actual}
		}
		msg := ""
		switch e := m["error"].(type) {
		case map[string]any:
			msg, _ = e["message"].(string)
		case nil:
		case string:
			msg = e
		default:
			msg = fmt.Sprint(e)
		}
		if msg == "" {
			msg = truncRunes(string(raw), 200)
		}
		return errResult(msg)
	case id == "google_ai":
		status, raw, err := p.post(ctx, fmt.Sprintf("%s/models/%s:generateContent", strings.TrimRight(base, "/"), url.PathEscape(model)),
			map[string]string{"x-goog-api-key": key},
			map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": testPrompt}}}}})
		if err != nil {
			return errResult(err.Error())
		}
		m, _ := asMap(raw)
		if m == nil {
			return errResult("Expecting value: line 1 column 1 (char 0)")
		}
		if status == http.StatusOK {
			text := ""
			if cs, ok := m["candidates"].([]any); ok && len(cs) > 0 {
				if c0, ok := cs[0].(map[string]any); ok {
					if ct, ok := c0["content"].(map[string]any); ok {
						if ps, ok := ct["parts"].([]any); ok && len(ps) > 0 {
							if p0, ok := ps[0].(map[string]any); ok {
								text, _ = p0["text"].(string)
							}
						}
					}
				}
			}
			return map[string]any{"status": "ok", "response": strings.TrimSpace(text), "model": model}
		}
		if e, ok := m["error"].(map[string]any); ok {
			if msg, ok := e["message"].(string); ok {
				return errResult(msg)
			}
		}
		return errResult(truncRunes(string(raw), 200))
	}
	return errResult("Provider not supported for testing yet")
}
