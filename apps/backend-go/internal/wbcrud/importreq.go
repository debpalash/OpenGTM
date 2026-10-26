package wbcrud

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// MaxImportRows is ImportRowsRequest.rows max_length.
const MaxImportRows = 50000

// ErrImportBodyTooLarge reports a request body above the configured cap. The
// upload is never read past the cap.
var ErrImportBodyTooLarge = errors.New("import body too large")

// ImportRequest is ImportRowsRequest after validation.
type ImportRequest struct {
	Rows          []*Obj
	Mapping       CSVMapping
	CreateColumns bool
	Dedupe        bool
	FileName      *string
	SourceSystem  string
}

// capReader fails with ErrImportBodyTooLarge as soon as more than left bytes
// are available, without consuming more than one extra buffer.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var probe [512]byte
	buf := p
	if c.left <= 0 {
		buf = probe[:]
	} else if int64(len(buf)) > c.left {
		buf = buf[:c.left]
	}
	for i := 0; i < 100; i++ { // a reader answering (0, nil) must not spin its caller
		n, err := c.r.Read(buf)
		if n > 0 || err != nil {
			if c.left <= 0 {
				if n > 0 {
					return 0, ErrImportBodyTooLarge
				}
				return 0, err
			}
			c.left -= int64(n)
			return n, err
		}
	}
	return 0, io.ErrNoProgress
}

func invalidJSON() []httpx.FieldError {
	return fe("json_invalid", "JSON decode error", []any{"body", 0}, map[string]any{})
}

// ReadImportRequest streams and validates an import body: the JSON is decoded
// token by token straight from r (never buffered whole), each row becomes an
// ordered object as it is read, and at most limit bytes are consumed. It
// returns pydantic-shaped errors for a bad body, or ErrImportBodyTooLarge /
// the reader's own error.
func ReadImportRequest(r io.Reader, limit int64) (ImportRequest, []httpx.FieldError, error) {
	br := bufio.NewReaderSize(&capReader{r: r, left: limit}, 64<<10)
	var first byte
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			return ImportRequest{}, fe("missing", "Field required", []any{"body"}, nil), nil
		}
		if err != nil {
			return ImportRequest{}, nil, err
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		first = b
		_ = br.UnreadByte()
		break
	}
	if first != '{' { // not a model body: small, decode it whole for the error input
		raw, err := io.ReadAll(br)
		if err != nil {
			return ImportRequest{}, nil, err
		}
		doc, errs := DecodeBody(raw)
		if errs != nil {
			return ImportRequest{}, errs, nil
		}
		_, errs = bodyModel(doc)
		return ImportRequest{}, errs, nil
	}

	o, err := streamImportObject(br)
	if err != nil {
		if errors.Is(err, ErrImportBodyTooLarge) {
			return ImportRequest{}, nil, err
		}
		var syn *json.SyntaxError
		if errors.As(err, &syn) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, errBadImportJSON) || errors.Is(err, errJSONTooDeep) {
			return ImportRequest{}, invalidJSON(), nil
		}
		return ImportRequest{}, nil, err
	}
	req, errs := validateImport(o)
	if errs != nil {
		return ImportRequest{}, errs, nil
	}
	return req, nil, nil
}

var errBadImportJSON = errors.New("invalid JSON body")

// streamImportObject decodes the top-level object; "rows" elements are decoded
// one at a time, so no raw copy of the array exists.
func streamImportObject(r io.Reader) (*Obj, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // '{'
		return nil, err
	}
	o := NewObj()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errBadImportJSON
		}
		if key != "rows" {
			v, err := decodeOrdered(dec, true)
			if err != nil {
				return nil, err
			}
			o.Set(key, v)
			continue
		}
		v, err := streamRows(dec)
		if err != nil {
			return nil, err
		}
		o.Set(key, v)
	}
	if _, err := dec.Token(); err != nil { // '}'
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errBadImportJSON // trailing data
	}
	return o, nil
}

func streamRows(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, isDelim := tok.(json.Delim)
	switch {
	case isDelim && d == '[':
		elems := []any{}
		for dec.More() {
			v, err := decodeOrdered(dec, true)
			if err != nil {
				return nil, err
			}
			elems = append(elems, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return elems, nil
	case isDelim: // '{'
		obj := NewObj()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := decodeOrdered(dec, true)
			if err != nil {
				return nil, err
			}
			obj.Set(k, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	}
	return tok, nil
}

var sourceSystems = map[string]bool{"auto": true, "clay": true, "generic": true}

func validateImport(o *Obj) (ImportRequest, []httpx.FieldError) {
	req := ImportRequest{CreateColumns: true, Dedupe: true, SourceSystem: "auto"}
	var errs []httpx.FieldError

	switch rv, present := o.Get("rows"); {
	case !present:
		errs = append(errs, missing(o, "body", "rows")...)
	default:
		list, isList := rv.([]any)
		if !isList {
			errs = append(errs, fe("list_type", "Input should be a valid list", []any{"body", "rows"}, rv)...)
			break
		}
		n := len(errs)
		req.Rows = make([]*Obj, 0, len(list))
		for i, el := range list {
			if row, ok := el.(*Obj); ok {
				req.Rows = append(req.Rows, row)
			} else {
				errs = append(errs, fe("dict_type", "Input should be a valid dictionary", []any{"body", "rows", i}, el)...)
			}
		}
		if len(errs) == n {
			switch {
			case len(list) == 0:
				e := tooShort("List", list)
				e[0].Loc = []any{"body", "rows"}
				errs = append(errs, e...)
			case len(list) > MaxImportRows:
				errs = append(errs, httpx.FieldError{Type: "too_long", Loc: []any{"body", "rows"}, Input: list,
					Msg: fmt.Sprintf("List should have at most %d items after validation, not %d", MaxImportRows, len(list)),
					Ctx: map[string]any{"field_type": "List", "max_length": MaxImportRows, "actual_length": len(list)}})
			}
		}
	}

	if mv, present := o.Get("mapping"); present && mv != nil {
		mo, ok := mv.(*Obj)
		if !ok {
			errs = append(errs, fe("dict_type", "Input should be a valid dictionary", []any{"body", "mapping"}, mv)...)
		} else {
			m := make(CSVMapping, mo.Len())
			bad := false
			for _, k := range mo.Keys() {
				v, _ := mo.Get(k)
				switch t := v.(type) {
				case nil:
					m[k] = nil
				case string:
					s := t
					m[k] = &s
				default:
					bad = true
					errs = append(errs, fe("string_type", "Input should be a valid string", []any{"body", "mapping", k}, v)...)
				}
			}
			if !bad {
				req.Mapping = m
			}
		}
	}

	for _, f := range []struct {
		name string
		dst  *bool
	}{{"create_columns", &req.CreateColumns}, {"dedupe", &req.Dedupe}} {
		if v, present := o.Get(f.name); present {
			b, e := parseBool(v, []any{"body", f.name})
			if e != nil {
				errs = append(errs, e...)
			} else {
				*f.dst = b.(bool)
			}
		}
	}

	if v, present := o.Get("file_name"); present && v != nil {
		s, e := strMax(v, []any{"body", "file_name"}, 255)
		if e != nil {
			errs = append(errs, e...)
		} else {
			req.FileName = &s
		}
	}

	if v, present := o.Get("source_system"); present {
		if s, ok := v.(string); ok && sourceSystems[s] {
			req.SourceSystem = s
		} else {
			errs = append(errs, httpx.FieldError{Type: "literal_error", Loc: []any{"body", "source_system"}, Input: v,
				Msg: "Input should be 'auto', 'clay' or 'generic'", Ctx: map[string]any{"expected": "'auto', 'clay' or 'generic'"}})
		}
	}
	if errs != nil {
		return ImportRequest{}, errs
	}
	return req, nil
}
