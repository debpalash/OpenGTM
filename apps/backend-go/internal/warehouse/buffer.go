// Package warehouse is the portable, checksum-verified JSONL warehouse batch export: a spill
// buffer that renders the audience export byte-identically to Python's
// WarehouseExportBuffer, and a guarded streaming POST to the destination.
package warehouse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"math/big"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// DefaultMaxMemory is the in-memory bound before records spill to disk.
const DefaultMaxMemory = 1_000_000

// ErrClosed is returned by operations on a closed Buffer.
var ErrClosed = errors.New("warehouse export buffer is closed")

// DestRef is the slice of a destination the export needs.
type DestRef struct {
	ID         string
	AudienceID string
	Config     map[string]any
}

// Buffer accumulates JSONL records, spilling to an unlinked 0600 temp file past maxMemory so
// PII is never left in a named file. It is not safe for concurrent use.
type Buffer struct {
	runID, wsID string
	dest        DestRef
	max         int
	mem         bytes.Buffer
	file        *os.File
	size        int64
	rows        int
	digest      hash.Hash
	closed      bool
}

// NewBuffer returns an empty Buffer; maxMemory <= 0 selects DefaultMaxMemory.
func NewBuffer(runID, workspaceID string, dest DestRef, maxMemory int) *Buffer {
	if maxMemory <= 0 {
		maxMemory = DefaultMaxMemory
	}
	return &Buffer{runID: runID, wsID: workspaceID, dest: dest, max: maxMemory, digest: sha256.New()}
}

// Append adds {"_opengtm_lead_id": leadID, **payload} as one sorted-key compact JSON line. A
// payload key "_opengtm_lead_id" overrides the id, as in Python.
func (b *Buffer) Append(leadID int, payload map[string]any) error {
	if b.closed {
		return ErrClosed
	}
	rec := make(map[string]any, len(payload)+1)
	rec["_opengtm_lead_id"] = leadID
	for k, v := range payload {
		rec[k] = v
	}
	raw, err := pyJSON(rec)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := b.write(raw); err != nil {
		return err
	}
	b.digest.Write(raw)
	b.rows++
	return nil
}

func (b *Buffer) write(line []byte) error {
	if b.file != nil {
		n, err := b.file.Write(line)
		b.size += int64(n)
		if err != nil {
			return fmt.Errorf("warehouse export spill write failed: %w", errors.Unwrap(err))
		}
		return nil
	}
	b.mem.Write(line)
	b.size += int64(len(line))
	if b.mem.Len() > b.max { // Python's SpooledTemporaryFile rolls when size > max_size
		return b.spill()
	}
	return nil
}

// spill moves the in-memory records to an already-unlinked, owner-only temp file.
func (b *Buffer) spill() error {
	f, err := os.CreateTemp("", "opengtm-warehouse-*")
	if err != nil {
		return errors.New("warehouse export spill failed")
	}
	// Unlink at once: the data is reachable only through this descriptor.
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return errors.New("warehouse export spill failed")
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return errors.New("warehouse export spill failed")
	}
	if _, err := f.Write(b.mem.Bytes()); err != nil {
		f.Close()
		return errors.New("warehouse export spill failed")
	}
	b.file = f
	b.mem = bytes.Buffer{}
	return nil
}

// RowCount is the number of records appended.
func (b *Buffer) RowCount() int { return b.rows }

// RolledToDisk reports whether the records have spilled out of memory.
func (b *Buffer) RolledToDisk() bool { return b.file != nil }

// Manifest is the export manifest; records_sha256 covers the record lines only.
func (b *Buffer) Manifest() map[string]any {
	ds, mode := any("opengtm_audience"), any("snapshot")
	if v := b.dest.Config["dataset"]; pyTruthy(v) {
		ds = v
	}
	if v := b.dest.Config["mode"]; pyTruthy(v) {
		mode = v
	}
	return map[string]any{
		"type":           "opengtm.audience.export",
		"version":        1,
		"run_id":         b.runID,
		"workspace_id":   b.wsID,
		"audience_id":    b.dest.AudienceID,
		"destination_id": b.dest.ID,
		"dataset":        ds,
		"mode":           mode,
		"row_count":      b.rows,
		"records_sha256": hex.EncodeToString(b.digest.Sum(nil)),
	}
}

func (b *Buffer) manifestLine() ([]byte, error) {
	raw, err := pyJSON(map[string]any{"_manifest": b.Manifest()})
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// Reader streams the manifest line followed by the records. Each call starts from the top and
// the manifest reflects the rows appended so far. After Close it yields ErrClosed.
func (b *Buffer) Reader() io.Reader {
	if b.closed {
		return errReader{ErrClosed}
	}
	line, err := b.manifestLine()
	if err != nil {
		return errReader{err}
	}
	var body io.Reader
	if b.file != nil {
		body = io.NewSectionReader(b.file, 0, b.size)
	} else {
		body = bytes.NewReader(b.mem.Bytes())
	}
	return io.MultiReader(bytes.NewReader(line), body)
}

// Close releases the data; the spill file is already unlinked so nothing remains on disk.
func (b *Buffer) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.mem = bytes.Buffer{}
	if b.file != nil {
		err := b.file.Close()
		return err
	}
	return nil
}

// pyTruthy is bool(v) for JSON-shaped config values.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, ok := new(big.Float).SetString(string(x))
		return !ok || f.Sign() != 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() != 0
	case reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() > 0
	}
	return true
}

// ---- Python-exact JSON ----

// pyJSON renders v like Python's json.dumps(v, sort_keys=True, separators=(",", ":"),
// default=str): ensure_ascii, repr floats, NaN/Infinity allowed, arbitrary-size ints, and str()
// for values JSON cannot represent. json.Number follows json.loads: a '.', 'e' or 'E' makes it a
// float, anything else an int.
func pyJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := pyWrite(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func pyWrite(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return nil
	case bool:
		b.WriteString(strconv.FormatBool(x))
		return nil
	case string:
		return pyString(b, x)
	case json.Number:
		return pyNumber(b, string(x))
	case float64:
		pyFloat(b, x)
		return nil
	case float32:
		pyFloat(b, float64(x)) // Python floats are doubles; a float32 widens exactly
		return nil
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
		return nil
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
		return nil
	case *big.Int:
		b.WriteString(x.String())
		return nil
	case time.Time:
		return pyString(b, pyDatetime(x))
	case map[string]any:
		return pyMap(b, reflect.ValueOf(x))
	case []any:
		return pySlice(b, reflect.ValueOf(x))
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int8, reflect.Int16, reflect.Int32:
		b.WriteString(strconv.FormatInt(rv.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(rv.Uint(), 10))
		return nil
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			return pyMap(b, rv)
		}
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			break // bytes are not JSON: default=str
		}
		return pySlice(b, rv)
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			b.WriteString("null")
			return nil
		}
		return pyWrite(b, rv.Elem().Interface())
	}
	return pyString(b, fmt.Sprint(v)) // default=str
}

func pySlice(b *bytes.Buffer, rv reflect.Value) error {
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		b.WriteString("null")
		return nil
	}
	b.WriteByte('[')
	for i := 0; i < rv.Len(); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := pyWrite(b, rv.Index(i).Interface()); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

func pyMap(b *bytes.Buffer, rv reflect.Value) error {
	if rv.IsNil() {
		b.WriteString("null")
		return nil
	}
	keys := make([]string, 0, rv.Len())
	byKey := make(map[string]reflect.Value, rv.Len())
	it := rv.MapRange()
	for it.Next() {
		k := it.Key().String()
		keys = append(keys, k)
		byKey[k] = it.Value()
	}
	sort.Strings(keys) // UTF-8 byte order equals Python's code point order
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := pyString(b, k); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := pyWrite(b, byKey[k].Interface()); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// pyString writes an ensure_ascii JSON string. canonjson implements exactly Python's escaping
// (short escapes, \u00XX for control and DEL, surrogate pairs); invalid UTF-8 is rejected.
func pyString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("warehouse payload contains invalid UTF-8")
	}
	raw, err := canonjson.Marshal(s, true)
	if err != nil {
		return err
	}
	b.Write(raw)
	return nil
}

func pyFloat(b *bytes.Buffer, f float64) {
	switch {
	case math.IsNaN(f):
		b.WriteString("NaN")
	case math.IsInf(f, 1):
		b.WriteString("Infinity")
	case math.IsInf(f, -1):
		b.WriteString("-Infinity")
	default:
		s, _ := canonjson.FloatRepr(f)
		b.WriteString(s)
	}
}

func pyNumber(b *bytes.Buffer, s string) error {
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !math.IsInf(f, 0) { // 1e999 parses as inf in Python too
			return errors.New("warehouse payload contains an invalid number")
		}
		pyFloat(b, f)
		return nil
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return errors.New("warehouse payload contains an invalid number")
	}
	b.WriteString(n.String())
	return nil
}

// pyDatetime is str(datetime): "YYYY-MM-DD HH:MM:SS[.ffffff]+HH:MM".
func pyDatetime(t time.Time) string {
	s := t.Format("2006-01-02 15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + t.Format("-07:00")
}
