package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chatmemory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// copilotListLimit is list_conversations' default limit.
const copilotListLimit = 50

type copilotRoutes struct {
	d   Deps
	log *slog.Logger
}

// registerCopilotRoutes: info (GET/POST), conversations (list/get/delete), memories (routers/copilotkit.py).
func registerCopilotRoutes(r *httpx.Router, d Deps) {
	c := &copilotRoutes{d: d, log: d.logger()}
	r.Handle("GET /api/copilotkit/info", http.HandlerFunc(c.info))
	r.Handle("POST /api/copilotkit/info", http.HandlerFunc(c.info))
	r.Handle("GET /api/copilotkit/conversations", http.HandlerFunc(c.list))
	r.Handle("GET /api/copilotkit/conversations/{conv_id}", http.HandlerFunc(c.get))
	r.Handle("DELETE /api/copilotkit/conversations/{conv_id}", http.HandlerFunc(c.remove))
	r.Handle("GET /api/copilotkit/memories", http.HandlerFunc(c.memories))
}

// writeCopilotJSON writes v like Starlette's JSONResponse: compact, no HTML escaping and non-ASCII kept as is.
// Go's encoder still escapes U+2028/U+2029, which Python leaves raw, so those two are restored.
func writeCopilotJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writeCopilotRaw(w, status, restoreLineSeparators(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
}

func writeCopilotRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// restoreLineSeparators turns the encoder's   and   escapes back into the raw characters. A
// backslash before the escape that is itself escaped (an even run) means the text was a literal " ".
func restoreLineSeparators(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' {
			if i+5 < len(b) && b[i+1] == 'u' && b[i+2] == '2' && b[i+3] == '0' && b[i+4] == '2' && (b[i+5] == '8' || b[i+5] == '9') {
				if b[i+5] == '8' {
					out = append(out, " "...)
				} else {
					out = append(out, " "...)
				}
				i += 5
				continue
			}
			// any other escape: copy both bytes so an escaped backslash never pairs with what follows
			out = append(out, b[i])
			if i+1 < len(b) {
				i++
				out = append(out, b[i])
			}
			continue
		}
		out = append(out, b[i])
	}
	return out
}

func (c *copilotRoutes) info(w http.ResponseWriter, _ *http.Request) {
	writeCopilotRaw(w, http.StatusOK, copilot.InfoResponse())
}

// resolve is _resolve_chat_workspace: it runs before anything else and answers its own refusal.
func (c *copilotRoutes) resolve(w http.ResponseWriter, r *http.Request) (copilotauth.Resolved, bool) {
	res, aerr := copilotauth.Resolve(r, copilotauth.Deps{Signer: c.d.Signer, Identity: c.d.Identity, DB: c.d.Pool,
		RequireAuth: c.d.Flags.Copilot.ChatRequireAuth})
	if aerr != nil {
		aerr.Write(w)
		return res, false
	}
	return res, true
}

func (c *copilotRoutes) fail(w http.ResponseWriter, what string, err error) {
	c.log.Error("copilot: "+what+" failed", "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

func (c *copilotRoutes) list(w http.ResponseWriter, r *http.Request) {
	res, ok := c.resolve(w, r)
	if !ok {
		return
	}
	var convs []chathistory.Conversation
	err := tenancy.InWorkspace(r.Context(), c.d.Pool, res.WorkspaceID, func(tx pgx.Tx) (e error) {
		convs, e = chathistory.List(r.Context(), tx, res.WorkspaceID, res.UserID, copilotListLimit)
		return e
	})
	if err != nil {
		c.fail(w, "list conversations", err)
		return
	}
	if convs == nil {
		convs = []chathistory.Conversation{}
	}
	writeCopilotJSON(w, http.StatusOK, map[string]any{"conversations": convs})
}

// badConvID reports a path id that can never name a conversation (NUL bytes or invalid UTF-8 would
// make Postgres fail); the route answers 404 before any query.
func badConvID(id string) bool { return strings.ContainsRune(id, 0) || !utf8.ValidString(id) }

func (c *copilotRoutes) get(w http.ResponseWriter, r *http.Request) {
	res, ok := c.resolve(w, r)
	if !ok {
		return
	}
	if badConvID(r.PathValue("conv_id")) {
		writeCopilotRaw(w, http.StatusNotFound, []byte(`{"error":"Not found"}`))
		return
	}
	var (
		conv *chathistory.Conversation
		msgs []chathistory.Message
	)
	err := tenancy.InWorkspace(r.Context(), c.d.Pool, res.WorkspaceID, func(tx pgx.Tx) (e error) {
		if conv, e = chathistory.Get(r.Context(), tx, r.PathValue("conv_id"), res.WorkspaceID, res.UserID); e != nil || conv == nil {
			return e
		}
		msgs, e = chathistory.Messages(r.Context(), tx, conv.ID, res.WorkspaceID, res.UserID)
		return e
	})
	if err != nil {
		c.fail(w, "get conversation", err)
		return
	}
	if conv == nil {
		writeCopilotRaw(w, http.StatusNotFound, []byte(`{"error":"Not found"}`))
		return
	}
	if msgs == nil {
		msgs = []chathistory.Message{}
	}
	writeCopilotJSON(w, http.StatusOK, struct {
		Conversation *chathistory.Conversation `json:"conversation"`
		Messages     []chathistory.Message     `json:"messages"`
	}{conv, msgs})
}

func (c *copilotRoutes) remove(w http.ResponseWriter, r *http.Request) {
	res, ok := c.resolve(w, r)
	if !ok {
		return
	}
	if badConvID(r.PathValue("conv_id")) {
		writeCopilotRaw(w, http.StatusNotFound, []byte(`{"error":"Not found"}`))
		return
	}
	err := tenancy.InWorkspace(r.Context(), c.d.Pool, res.WorkspaceID, func(tx pgx.Tx) error {
		return chathistory.Delete(r.Context(), tx, r.PathValue("conv_id"), res.WorkspaceID, res.UserID)
	})
	if err != nil {
		c.fail(w, "delete conversation", err)
		return
	}
	writeCopilotRaw(w, http.StatusOK, []byte(`{"ok":true}`))
}

func (c *copilotRoutes) memories(w http.ResponseWriter, r *http.Request) {
	res, ok := c.resolve(w, r)
	if !ok {
		return
	}
	ns, err := chatmemory.Namespace(res.WorkspaceID, res.UserID)
	if err != nil {
		c.fail(w, "memory namespace", err)
		return
	}
	var mems []chatmemory.Memory
	err = tenancy.InWorkspace(r.Context(), c.d.Pool, res.WorkspaceID, func(tx pgx.Tx) (e error) {
		mems, e = chatmemory.All(r.Context(), tx, ns)
		return e
	})
	if err != nil {
		c.fail(w, "list memories", err)
		return
	}
	type memoryOut struct {
		Memory    string          `json:"memory"`
		Metadata  json.RawMessage `json:"metadata"`
		CreatedAt string          `json:"created_at"`
	}
	out := make([]memoryOut, len(mems))
	for i, m := range mems {
		out[i] = memoryOut{m.Memory, m.MetadataJSON, m.CreatedAt}
	}
	writeCopilotJSON(w, http.StatusOK, struct {
		Memories  []memoryOut `json:"memories"`
		Available bool        `json:"available"`
	}{out, chatmemory.Available()})
}
