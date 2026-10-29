package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/nlcolumn"
)

func runWebResearch(t *testing.T, handler http.HandlerFunc) (string, *fakeLeadStore) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{}}
	h := newTestRoutes(store, nil)
	h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.httpc = srv.Client()
	h.newLLM = func(context.Context, string) llmBundle {
		return llmBundle{Extract: nlcolumn.LLM(nil), Chat: fakeChat{p: llm.ChatProvider{ID: "x", APIKey: "k", BaseURL: srv.URL, Model: "m", OpenAICompatible: true}}}
	}
	rec := httptest.NewRecorder()
	e := &enrichRun{h: h, ctx: context.Background(), ws: "w", lead: &leadsread.Lead{ID: 1, Company: "Acme"}, out: sseOut{w: rec, f: rec}}
	done := make(chan struct{})
	go func() { e.webResearch(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("webResearch did not stop")
	}
	return rec.Body.String(), store
}

func TestEnrichStreamTotalDeadline(t *testing.T) {
	old := enrichStreamTotal
	enrichStreamTotal = 300 * time.Millisecond
	defer func() { enrichStreamTotal = old }()
	out, store := runWebResearch(t, func(w http.ResponseWriter, r *http.Request) {
		// Keep-alive comments forever: they must neither reset the idle timer nor outlive the deadline.
		for i := 0; i < 200 && r.Context().Err() == nil; i++ {
			fmt.Fprint(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	if !strings.Contains(out, "timed out") {
		t.Errorf("want a timeout step, got %s", out)
	}
	if len(store.updates) != 0 {
		t.Error("a timed-out stream saved research")
	}
}

func TestEnrichStreamByteCap(t *testing.T) {
	old := enrichStreamMaxBytes
	enrichStreamMaxBytes = 4 << 10
	defer func() { enrichStreamMaxBytes = old }()
	out, store := runWebResearch(t, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 1000 && r.Context().Err() == nil; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", strings.Repeat("x", 100))
			w.(http.Flusher).Flush()
		}
	})
	if !strings.Contains(out, "too large") {
		t.Errorf("want a too-large step, got %.300s", out)
	}
	if len(store.updates) != 0 {
		t.Error("an oversized stream saved research")
	}
}
