package chatmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// SelfHostUser is Python's SELF_HOST_USER partition for user_id=None.
const SelfHostUser = "self-host"

// ErrEmptyNamespace: an empty workspace or unnamespaced key would share one
// global partition, so every entry point refuses it.
var ErrEmptyNamespace = errors.New("chatmemory: memory operations require a workspace")

func checkNS(ns string) error {
	if ns == "" || strings.HasPrefix(ns, ":") {
		return ErrEmptyNamespace
	}
	return nil
}

// Memory is one recalled memory; JSON key order matches Python's dict.
type Memory struct {
	Memory    string         `json:"memory"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt string         `json:"created_at"`
	// MetadataJSON is the stored jsonb as compact JSON in jsonb key order (what Python's dict iterates in).
	MetadataJSON json.RawMessage `json:"-"`
}

// Available is always true: the builtin store needs no SDK or key.
func Available() bool { return true }

// Namespace is Python _namespace: "{workspace}:{user}" with the self-host
// sentinel for a nil user. An empty workspace is an error.
func Namespace(ws string, uid *int32) (string, error) {
	if ws == "" {
		return "", ErrEmptyNamespace // Python raises ValueError
	}
	u := SelfHostUser
	if uid != nil {
		u = strconv.FormatInt(int64(*uid), 10)
	}
	return ws + ":" + u, nil
}

// Add stores a memory. Memory writes are best-effort: callers ignore the
// error (as Python's callers swallow exceptions); it must never fail a chat.
func Add(ctx context.Context, tx pgx.Tx, ns, text string, md map[string]any) error {
	if err := checkNS(ns); err != nil {
		return err
	}
	if md == nil {
		md = map[string]any{}
	}
	b, err := json.Marshal(md)
	if err != nil {
		return err
	}
	return gen.New(tx).MemAdd(ctx, gen.MemAddParams{
		UserID:    ns,
		Text:      text,
		Metadata:  b,
		CreatedAt: pgtype.Timestamptz{Time: time.Now().UTC().Truncate(time.Microsecond), Valid: true},
	})
}

// Search recalls the last 500 rows of the namespace (id DESC), keeps those
// sharing at least one token with the query, stable-sorted by overlap
// descending so recency breaks ties, capped to limit.
func Search(ctx context.Context, tx pgx.Tx, ns, query string, limit int) ([]Memory, error) {
	if err := checkNS(ns); err != nil {
		return nil, err
	}
	q := Tokens(query)
	if len(q) == 0 || limit <= 0 {
		return []Memory{}, nil
	}
	rows, err := gen.New(tx).MemRecent(ctx, ns)
	if err != nil {
		return nil, err
	}
	type scored struct {
		n int
		m Memory
	}
	var hits []scored
	for _, r := range rows {
		n := 0
		for tok := range Tokens(r.Text) {
			if _, ok := q[tok]; ok {
				n++
			}
		}
		if n == 0 {
			continue
		}
		m, err := toMemory(r.Text, r.Metadata, r.CreatedAt)
		if err != nil {
			return nil, err
		}
		hits = append(hits, scored{n, m})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Memory, len(hits))
	for i, h := range hits {
		out[i] = h.m
	}
	return out, nil
}

// All returns every memory of the namespace, newest first.
func All(ctx context.Context, tx pgx.Tx, ns string) ([]Memory, error) {
	if err := checkNS(ns); err != nil {
		return nil, err
	}
	rows, err := gen.New(tx).MemAll(ctx, ns)
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, len(rows))
	for _, r := range rows {
		m, err := toMemory(r.Text, r.Metadata, r.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func toMemory(text string, md []byte, ts pgtype.Timestamptz) (Memory, error) {
	m := Memory{Memory: text, Metadata: map[string]any{}, MetadataJSON: json.RawMessage("{}")}
	if len(md) > 0 {
		var c bytes.Buffer
		if err := json.Compact(&c, md); err == nil && c.String() != "null" {
			m.MetadataJSON = c.Bytes()
		}
		dec := json.NewDecoder(bytes.NewReader(md))
		dec.UseNumber() // keep ints as ints, like Python
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			return m, fmt.Errorf("chatmemory: metadata: %w", err)
		}
		if v != nil {
			m.Metadata = v
		}
	}
	m.CreatedAt = isoUTC(ts.Time)
	return m, nil
}

// isoUTC is Python astimezone(utc).isoformat(): microseconds only when
// non-zero, "+00:00" offset.
func isoUTC(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}
