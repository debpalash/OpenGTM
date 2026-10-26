// Package ssestream frames server-sent events byte-identically to the Python copilot stream:
// each event is "data: " + json.dumps(v) (default ", " and ": " separators, ensure_ascii,
// insertion order) + "\n\n"; keepalives are ": keepalive\n\n"; the terminal frame is
// "data: [DONE]\n\n".
package ssestream

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// writeTimeout bounds each frame so a client that stops reading cannot pin a goroutine
// (best effort: ignored when the ResponseWriter has no deadline support).
const writeTimeout = 10 * time.Second

// KV is one key/value of an Obj.
type KV struct {
	Key string
	Val any
}

// Obj is a JSON object that keeps insertion order, like a Python dict. Plain
// map[string]any is also accepted by Event but is emitted in sorted key order.
type Obj []KV

// Writer serialises frames onto one response. It is safe for concurrent use (a keepalive
// goroutine and the event producer). After the first write error every call returns it.
type Writer struct {
	mu    sync.Mutex
	w     http.ResponseWriter
	flush func()
	rc    *http.ResponseController
	err   error
}

// New returns a Writer. It fails when w cannot flush. flush may be nil (w.Flush is used).
func New(w http.ResponseWriter, flush func()) (*Writer, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("ssestream: response writer does not support flushing")
	}
	if flush == nil {
		flush = f.Flush
	}
	return &Writer{w: w, flush: flush, rc: http.NewResponseController(w)}, nil
}

// Headers sets the four headers the Python stream sends (Starlette adds the charset).
func Headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func (w *Writer) raw(frame []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	_ = w.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := w.w.Write(frame); err != nil {
		w.err = err
		return err
	}
	w.flush()
	return nil
}

// Event writes one data frame.
func (w *Writer) Event(v any) error {
	var b bytes.Buffer
	b.WriteString("data: ")
	if err := dumps(&b, v); err != nil {
		return err
	}
	b.WriteString("\n\n")
	return w.raw(b.Bytes())
}

// EventRaw relays one JSON value (for example a Python worker frame) as a data frame via
// Reformat, preserving its key order. An invalid value writes nothing.
func (w *Writer) EventRaw(raw []byte) error {
	r, err := Reformat(raw)
	if err != nil {
		return err
	}
	b := make([]byte, 0, len(r)+8)
	b = append(b, "data: "...)
	b = append(b, r...)
	b = append(b, '\n', '\n')
	return w.raw(b)
}

// Done writes the terminal frame.
func (w *Writer) Done() error { return w.raw([]byte("data: [DONE]\n\n")) }

// Keepalive writes a comment frame.
func (w *Writer) Keepalive() error { return w.raw([]byte(": keepalive\n\n")) }

func dumps(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil, bool, string, float64:
		s, err := canonjson.Marshal(x, false)
		if err != nil {
			return err
		}
		b.Write(s)
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case []any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := dumps(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case Obj:
		return dumpPairs(b, x)
	case map[string]any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		kv := make(Obj, 0, len(x))
		for k, e := range x {
			kv = append(kv, KV{k, e})
		}
		sort.Slice(kv, func(i, j int) bool { return kv[i].Key < kv[j].Key })
		return dumpPairs(b, kv)
	default:
		return fmt.Errorf("ssestream: unsupported type %T", v)
	}
	return nil
}

func dumpPairs(b *bytes.Buffer, o Obj) error {
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteString(", ")
		}
		k, err := canonjson.Marshal(p.Key, false)
		if err != nil {
			return err
		}
		b.Write(k)
		b.WriteString(": ")
		if err := dumps(b, p.Val); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}
