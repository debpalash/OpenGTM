package ssestream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// MaxFrameBytes caps one relayed input frame (8 MiB + 64 KiB, the per-line cap of the retired Python worker
// stream).
var MaxFrameBytes = 8<<20 + 64<<10

// outputFactor bounds the output: ensure_ascii turns a 2-4 byte rune into up to 12 bytes
// (surrogate pair) or 6 bytes (BMP), so output is capped at a multiple of the input cap.
const outputFactor = 6

// MaxDepth is the deepest container nesting Reformat accepts.
const MaxDepth = 64

// Reformat re-serialises one JSON value the way Python's json.dumps does (", " and ": "
// separators, ensure_ascii escapes), keeping object keys in source order (duplicates included).
// Floats are re-rendered like repr(float); integers are copied as written ("-0" becomes "0",
// as a Python int). NaN/Infinity literals, overflowing floats, invalid UTF-8, lone surrogate
// escapes, nesting beyond MaxDepth, trailing data and values larger than MaxFrameBytes are errors.
func Reformat(raw []byte) ([]byte, error) {
	if len(raw) > MaxFrameBytes {
		return nil, errors.New("ssestream: frame too large")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("ssestream: invalid UTF-8")
	}
	if err := checkSurrogates(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	type frame struct {
		obj   bool
		n     int // children written (keys count in objects)
		inVal bool
	}
	var st []frame
	// sep writes the separator owed before the next element of the enclosing container.
	sep := func() {
		if len(st) == 0 {
			return
		}
		f := &st[len(st)-1]
		if f.obj && f.inVal {
			out.WriteString(": ")
			f.inVal = false
			return
		}
		if f.n > 0 {
			out.WriteString(", ")
		}
	}
	// done marks a completed value in the enclosing container.
	done := func() {
		if len(st) > 0 {
			st[len(st)-1].n++
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(st) >= MaxDepth {
					return nil, errors.New("ssestream: nesting too deep")
				}
				sep()
				out.WriteByte(byte(d))
				st = append(st, frame{obj: d == '{'})
			default:
				out.WriteByte(byte(d))
				st = st[:len(st)-1]
				done()
			}
		} else {
			top := len(st) > 0 && st[len(st)-1].obj && !st[len(st)-1].inVal
			if top { // object key
				s, _ := tok.(string)
				sep()
				k, err := canonjson.Marshal(s, false)
				if err != nil {
					return nil, err
				}
				out.Write(k)
				st[len(st)-1].inVal = true
			} else {
				sep()
				if err := writeScalar(&out, tok); err != nil {
					return nil, err
				}
				done()
			}
		}
		if len(st) == 0 {
			break
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("ssestream: trailing data")
	}
	if out.Len() > MaxFrameBytes*outputFactor {
		return nil, errors.New("ssestream: frame too large")
	}
	return out.Bytes(), nil
}

func writeScalar(b *bytes.Buffer, tok any) error {
	switch x := tok.(type) {
	case json.Number:
		s := x.String()
		for i := 0; i < len(s); i++ {
			if c := s[i]; c == '.' || c == 'e' || c == 'E' {
				f, err := strconv.ParseFloat(s, 64)
				if err != nil {
					return errors.New("ssestream: number out of range")
				}
				r, err := canonjson.FloatRepr(f)
				if err != nil {
					return err
				}
				b.WriteString(r)
				return nil
			}
		}
		if s == "-0" {
			s = "0"
		}
		b.WriteString(s)
		return nil
	case nil, bool, string:
		out, err := canonjson.Marshal(x, false)
		if err != nil {
			return err
		}
		b.Write(out)
		return nil
	}
	return errors.New("ssestream: unexpected token")
}

// checkSurrogates rejects \uD800-\uDFFF escapes that are not a proper high+low pair (Python
// would keep a lone surrogate, which cannot be represented as UTF-8 and is not relayed).
func checkSurrogates(raw []byte) error {
	hex4 := func(i int) (rune, bool) {
		if i+4 > len(raw) {
			return 0, false
		}
		v, err := strconv.ParseUint(string(raw[i:i+4]), 16, 32)
		return rune(v), err == nil
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue // other escapes (and the escaped char) are skipped
		}
		r, ok := hex4(i + 1)
		if !ok {
			continue // the decoder reports the syntax error
		}
		i += 4
		switch {
		case r >= 0xD800 && r < 0xDC00:
			if i+2 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
				if lo, ok := hex4(i + 3); ok && lo >= 0xDC00 && lo < 0xE000 {
					i += 6
					continue
				}
			}
			return errors.New("ssestream: lone surrogate")
		case r >= 0xDC00 && r < 0xE000:
			return errors.New("ssestream: lone surrogate")
		}
	}
	return nil
}
