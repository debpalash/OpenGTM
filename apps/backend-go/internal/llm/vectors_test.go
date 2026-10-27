package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// runCompleteVec replays one recorded completion scenario against Go.
func runCompleteVec(t *testing.T, v complVec) {
	t.Helper()
	rs := newReplay(t, v.Script)
	sink, sl := &usageSink{}, &sleepLog{}
	c := newVecClient(t, v.Settings, rs.URL, v.Env, sink, sl)
	ctx := context.Background()

	for i, call := range v.Calls {
		var want json.RawMessage = v.Results[i]
		if call.Kind == "extract_json" {
			got, err := c.ExtractJSON(ctx, call.Prompt, call.System, call.MaxTokens)
			var wantObj map[string]any
			_ = json.Unmarshal(want, &wantObj)
			if len(wantObj) == 0 {
				if err == nil || got != nil {
					t.Fatalf("want failure ({}), got %v err=%v", got, err)
				}
				continue
			}
			if err != nil || !reflect.DeepEqual(got, wantObj) {
				t.Fatalf("call %d: want %v got %v err=%v", i, wantObj, got, err)
			}
			continue
		}
		var temp *float64
		if call.Temperature != nil {
			temp = call.Temperature
		}
		got, err := c.Complete(ctx, CompleteRequest{Prompt: call.Prompt, System: call.System, MaxTokens: call.MaxTokens, Temperature: temp})
		var wantStr string
		_ = json.Unmarshal(want, &wantStr)
		if got != wantStr {
			t.Fatalf("call %d: want %q got %q (err %v)", i, wantStr, got, err)
		}
		if (wantStr == "") != (err != nil) {
			t.Fatalf("call %d: text %q but err %v", i, got, err)
		}
	}

	native := strings.HasPrefix(v.Name, "anthropic")
	compareRequests(t, v.Requests, rs.requests(), !native)
	compareRows(t, v.UsageRows, sink.rows)
	if !native {
		if !reflect.DeepEqual(append([]float64{}, v.Sleeps...), append([]float64{}, sl.d...)) {
			t.Errorf("backoff sleeps: want %v got %v", v.Sleeps, sl.d)
		}
	}
	if v.Name != "timeout_message" {
		wantErrs, gotErrs := v.Errors, c.usage.allErrors()
		if len(wantErrs) != len(gotErrs) {
			t.Fatalf("usage errors: want %q got %q", wantErrs, gotErrs)
		}
		for i := range wantErrs {
			w, g := wantErrs[i], gotErrs[i]
			if native { // the SDK printed a Python dict repr after "Error code: N - "
				w, _, _ = strings.Cut(w, " - ")
				g, _, _ = strings.Cut(g, " - ")
			}
			if w != g {
				t.Errorf("usage error %d: want %q got %q", i, w, g)
			}
		}
	}
	got := c.Usage()
	if got.PromptTokens != v.Usage.PromptTokens || got.CompletionTokens != v.Usage.CompletionTokens ||
		got.TotalTokens != v.Usage.TotalTokens || got.CacheWriteTokens != v.Usage.CacheWriteTokens ||
		got.CacheReadTokens != v.Usage.CacheReadTokens || got.Calls != v.Usage.Calls {
		t.Errorf("token usage: want %+v got %+v", v.Usage, got)
	}
}

func TestCompleteVectors(t *testing.T) {
	v := loadVectors(t)
	all := append(append([]complVec{}, v.Complete...), v.AnthropicCall...)
	if len(all) < 25 {
		t.Fatalf("vectors missing: %d", len(all))
	}
	for _, sc := range all {
		t.Run(sc.Name, func(t *testing.T) { runCompleteVec(t, sc) })
	}
}

func TestExtractJSONFlowVectors(t *testing.T) {
	v := loadVectors(t)
	for _, sc := range v.ExtractJSON {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Name == "extract_json_list_is_returned_as_is" {
				// Deviation: Python returned the list to callers that expect a dict; Go
				// accepts objects only, so the reply is retried once and then fails.
				rs := newReplay(t, sc.Script)
				c := newVecClient(t, sc.Settings, rs.URL, nil, &usageSink{}, &sleepLog{})
				got, err := c.ExtractJSON(context.Background(), "get it", "", 0)
				if got != nil || !errors.Is(err, ErrInvalidJSON) || len(rs.requests()) != 2 {
					t.Fatalf("got %v err=%v reqs=%d", got, err, len(rs.requests()))
				}
				return
			}
			runCompleteVec(t, sc)
		})
	}
}

// Parse vectors: every recorded input; where Python was wrong the expectation
// is the corrected result (see the WP report).
func TestParseObjectVectors(t *testing.T) {
	v := loadVectors(t)
	fixed := map[string]string{
		// two-level regex returned the inner object of a three-level one
		`text {"a": {"b": {"c": 3}}} trailing`: `{"a": {"b": {"c": 3}}}`,
		// braces inside strings broke the regex
		`lead {"s": "brace } inside"} tail`: `{"s": "brace } inside"}`,
	}
	for _, p := range v.ParseJSON {
		t.Run(p.Input, func(t *testing.T) {
			got, ok := ParseObject(p.Input)
			if fx, has := fixed[p.Input]; has {
				var want map[string]any
				_ = json.Unmarshal([]byte(fx), &want)
				if !ok || !reflect.DeepEqual(got, want) {
					t.Fatalf("fixed: want %v got %v ok=%v", want, got, ok)
				}
				return
			}
			var want map[string]any
			isObj := p.OK && !p.NonFinite && json.Unmarshal(p.Output, &want) == nil && want != nil
			if !isObj {
				// null, arrays, scalars and NaN: not an object, so not accepted.
				if ok {
					t.Fatalf("expected no object, got %v", got)
				}
				return
			}
			if !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("want %v got %v ok=%v", want, got, ok)
			}
		})
	}
}

func TestProviderResolutionVectors(t *testing.T) {
	v := loadVectors(t)
	for _, sc := range v.DefaultProvider {
		t.Run(sc.Name, func(t *testing.T) {
			c := New(Options{Settings: mapSettings(sc.Settings), HTTP: http.DefaultClient})
			ctx := context.Background()
			if got := c.DefaultProvider(ctx); got != sc.Default {
				t.Fatalf("default: want %q got %q", sc.Default, got)
			}
			provs := c.Providers(ctx)
			if len(provs) != len(sc.Providers) {
				t.Fatalf("providers: want %d got %+v", len(sc.Providers), provs)
			}
			for i, w := range sc.Providers {
				g := provs[i]
				if g.ID != w.ID || g.BaseURL != w.BaseURL || g.Model != w.Model || g.TokenParam != w.TokenParam || g.Native != (w.Native == "anthropic") {
					t.Errorf("provider %d: want %+v got %+v", i, w, g)
				}
			}
			p, ok := c.AnthropicProvider(ctx)
			if (sc.Anthropic != nil) != ok || (ok && p.ID != *sc.Anthropic) {
				t.Errorf("anthropic provider: want %v got %v/%v", sc.Anthropic, p.ID, ok)
			}
		})
	}
}

func TestChatChainVectors(t *testing.T) {
	v := loadVectors(t)
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range v.ChatChain {
		t.Run(sc.Name, func(t *testing.T) {
			c := New(Options{Settings: mapSettings(sc.Settings), HTTP: http.DefaultClient, Registry: reg})
			conv := func(p ChatProvider) chatProv {
				return chatProv{p.ID, p.Name, p.APIKey, p.BaseURL, p.Model, p.OpenAICompatible}
			}
			var got []chatProv
			for _, p := range c.ChatChain(context.Background()) {
				got = append(got, conv(p))
			}
			if len(got) != len(sc.Chain) || (len(got) > 0 && !reflect.DeepEqual(got, sc.Chain)) {
				t.Fatalf("chain:\nwant %+v\ngot  %+v", sc.Chain, got)
			}
			if a := conv(c.ActiveChatProvider(context.Background())); a != sc.Active {
				t.Fatalf("active:\nwant %+v\ngot  %+v", sc.Active, a)
			}
		})
	}
}

func TestCostVectors(t *testing.T) {
	v := loadVectors(t)
	n := 0
	for _, c := range v.Cost {
		if c.Usage == nil {
			continue
		}
		n++
		u := AnthropicUsage{InputTokens: c.Usage[0], OutputTokens: c.Usage[1], CacheCreationInputTokens: c.Usage[2], CacheReadInputTokens: c.Usage[3]}
		model := c.Model
		if got := AnthropicCostUSD(u, model); model != "" && got != c.USD {
			t.Errorf("%s %v: want %v got %v", model, c.Usage, c.USD, got)
		}
		if in, out := PriceForModel(c.Model); in != c.Price[0] || out != c.Price[1] {
			t.Errorf("price %q: want %v got %v/%v", c.Model, c.Price, in, out)
		}
	}
	if n < 10 {
		t.Fatal("cost vectors missing")
	}
	// An empty model prices at the default model, like prov["model"] or DEFAULT.
	u := AnthropicUsage{InputTokens: 1000, OutputTokens: 500}
	if got, want := AnthropicCostUSD(u, ""), AnthropicCostUSD(u, AnthropicDefaultModel); got != want {
		t.Errorf("default model price: %v vs %v", got, want)
	}
}

func TestAnthropicTurnVectors(t *testing.T) {
	v := loadVectors(t)
	for _, sc := range v.ToolCall {
		t.Run(sc.Name, func(t *testing.T) {
			rs := newReplay(t, sc.Script)
			sink := &usageSink{}
			c := newVecClient(t, sc.Settings, rs.URL, nil, sink, &sleepLog{})
			resp, err := c.AnthropicTurn(context.Background(), AnthropicRequest{
				Messages: sc.Args.Messages, System: sc.Args.System, Tools: sc.Args.Tools, ToolChoice: sc.Args.ToolChoice,
				OutputFormat: sc.Args.OutputFormat, MaxTokens: sc.Args.MaxTokens,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Content) == 0 || resp.StopReason != "end_turn" {
				t.Fatalf("raw response lost: %+v", resp)
			}
			compareRequests(t, sc.Requests, rs.requests(), false)
			compareRows(t, sc.UsageRows, sink.rows)
		})
	}
}

func TestBatchVectors(t *testing.T) {
	v := loadVectors(t)
	for _, sc := range v.Batch {
		t.Run(sc.Name, func(t *testing.T) {
			rs := newReplay(t, sc.Script)
			sink, sl := &usageSink{}, &sleepLog{}
			c := newVecClient(t, sc.Settings, rs.URL, nil, sink, sl)
			var reqs []BatchRequest
			for _, r := range sc.RequestsIn {
				reqs = append(reqs, BatchRequest{CustomID: r.CustomID, Prompt: r.Prompt, System: r.System, MaxTokens: r.MaxTokens})
			}
			var submitted []string
			got, err := c.BatchComplete(context.Background(), reqs, BatchOptions{
				PollInterval: time.Millisecond, BatchID: sc.KW.BatchID, KnownResultIDs: sc.KW.KnownResultIDs,
				OnSubmitted: func(id string) { submitted = append(submitted, id) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, sc.Result) {
				t.Errorf("results: want %v got %v", sc.Result, got)
			}
			if len(submitted) != len(sc.Submitted) || (len(submitted) > 0 && submitted[0] != sc.Submitted[0]) {
				t.Errorf("submitted: want %v got %v", sc.Submitted, submitted)
			}
			compareRows(t, sc.UsageRows, sink.rows)
			// The Python SDK retrieved the batch once more before reading results to
			// learn results_url; Go builds the path itself. Compare with repeats of
			// the same GET collapsed.
			dedup := func(rr []recordedReq) []recordedReq {
				var out []recordedReq
				for _, r := range rr {
					if len(out) > 0 && out[len(out)-1].Method == "GET" && r.Method == "GET" && out[len(out)-1].Path == r.Path {
						continue
					}
					out = append(out, r)
				}
				return out
			}
			compareRequests(t, dedup(sc.Requests), dedup(rs.requests()), false)
		})
	}
}

func TestStreamVectors(t *testing.T) {
	v := loadVectors(t)
	// Deviations from Python (see the report): a chunk with an empty choices
	// list is skipped instead of failing the round with "list index out of range".
	deviations := map[string][]string{
		"empty_choices_chunk": {"content:z", "done", "cursor:p1"},
	}
	for _, sc := range v.Stream {
		t.Run(sc.Name, func(t *testing.T) {
			rs := newReplay(t, sc.Script)
			toProv := func(p chatProv) ChatProvider {
				return ChatProvider{ID: p.ID, Name: p.Name, APIKey: p.APIKey, BaseURL: strings.ReplaceAll(p.BaseURL, "{{BASE}}", rs.URL), Model: p.Model, OpenAICompatible: p.OpenAICompatible}
			}
			cur := Cursor{Provider: toProv(sc.Provider)}
			for _, f := range sc.Fallbacks {
				cur.Fallbacks = append(cur.Fallbacks, toProv(f))
			}
			timeout := time.Duration(0)
			if sc.Timeout != nil {
				timeout = time.Duration(*sc.Timeout * float64(time.Second))
			}
			c := New(Options{Settings: mapSettings{}, HTTP: http.DefaultClient, StreamTimeout: timeout})
			var msgs, tools any
			_ = json.Unmarshal(sc.Messages, &msgs)
			_ = json.Unmarshal(sc.Tools, &tools)

			var got []string
			var calls []ToolCall
			for f := range c.Stream(context.Background(), StreamRequest{Messages: msgs, Tools: tools, Cursor: cur}) {
				switch f.Kind {
				case FrameContent, FrameWarning, FrameError:
					got = append(got, f.Kind+":"+f.Text)
				case FrameToolCalls:
					calls = f.ToolCalls
					got = append(got, "tool_calls")
				case FrameDone:
					got = append(got, "done")
				case FrameCursor:
					got = append(got, "cursor:"+f.Cursor.Provider.ID+":"+idList(f.Cursor.Fallbacks))
				}
			}
			var want []string
			var wantCalls []ToolCall
			for _, f := range sc.Frames {
				switch f.Kind {
				case "content", "warning", "error":
					var s string
					_ = json.Unmarshal(f.Data, &s)
					s = strings.ReplaceAll(s, "{{BASE}}", rs.URL)
					want = append(want, f.Kind+":"+s)
				case "tool_calls":
					_ = json.Unmarshal(f.Data, &wantCalls)
					want = append(want, "tool_calls")
				case "done":
					want = append(want, "done")
				case "cursor":
					want = append(want, "cursor:"+f.Provider+":"+strings.Join(f.Fallbacks, ","))
				}
			}
			if d, ok := deviations[sc.Name]; ok {
				want = nil
				for _, s := range d {
					if strings.HasPrefix(s, "cursor:") {
						s += ":" // no fallbacks
					}
					want = append(want, s)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("frames:\nwant %q\ngot  %q", want, got)
			}
			if len(wantCalls) > 0 && !reflect.DeepEqual(calls, wantCalls) {
				t.Errorf("tool calls: want %+v got %+v", wantCalls, calls)
			}
			compareRequests(t, sc.Requests, rs.requests(), true)
		})
	}
}

func idList(ps []ChatProvider) string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	return strings.Join(ids, ",")
}
