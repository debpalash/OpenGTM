package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Replaceable by tests.
var (
	resolveSession           = Resolve
	stdioStderr    io.Writer = os.Stderr
)

// stdioMaxFrame bounds one frame body or one line-delimited message (D4).
const stdioMaxFrame = MaxMessageBytes

// stdioMaxDigits bounds a Content-Length value; longer values are invalid.
const stdioMaxDigits = 18

// stdioEvent is one unit read from stdin: a message to handle, or a framing
// error to answer with -32600. nd marks line-delimited style.
type stdioEvent struct {
	body []byte
	nd   bool
	bad  bool
}

// ServeStdio serves line-delimited and Content-Length framed JSON-RPC over
// in/out. It authenticates once with token (a refusal is reported on stderr and
// returned as an error), then handles messages strictly in order, revalidating
// the session before tools/list and tools/call at most every
// Cfg.StdioRevalidateSeconds (D8). EOF and ctx cancellation return nil. Only
// protocol bytes are written to out.
func ServeStdio(ctx context.Context, s *Server, d Deps, in io.Reader, out io.Writer, token string) error {
	c, err := resolveSession(ctx, d, token)
	if err != nil {
		msg := "authentication unavailable"
		var ae *AuthError
		if errors.As(err, &ae) {
			msg = ae.Msg
		}
		fmt.Fprintf(stdioStderr, "MCP stdio refused: %s. Set OPENGTM_MCP_TOKEN to a valid token.\n", msg)
		return fmt.Errorf("mcp stdio refused: %s", msg)
	}
	interval := time.Duration(d.Cfg.StdioRevalidateSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	validated := now()

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan stdioEvent)
	go readStdio(rctx, bufio.NewReaderSize(in, 64<<10), events)

	for {
		var ev stdioEvent
		var ok bool
		select {
		case <-ctx.Done():
			return nil
		case ev, ok = <-events:
			if !ok {
				return nil
			}
		}
		if ev.bad {
			if err := writeStdio(out, ev.nd, errorBytes(idNull, rpcError{codeInvalidRequest, "Invalid Request"})); err != nil {
				return err
			}
			continue
		}
		if t := now(); t.Sub(validated) >= interval {
			if req, id, perr := parseRequest(ev.body); perr == nil && (req.Method == "tools/list" || req.Method == "tools/call") {
				nc, rerr := resolveSession(ctx, d, token)
				var ae *AuthError
				switch {
				case rerr == nil:
					c, validated = nc, t
				case errors.As(rerr, &ae):
					msg := errorBytes(id, rpcError{-32001, "unauthorized: " + ae.Msg})
					if err := writeStdio(out, ev.nd, msg); err != nil {
						return err
					}
					return fmt.Errorf("mcp stdio session ended: %s", ae.Msg)
				default: // transient failure: this request fails, the session survives
					if err := writeStdio(out, ev.nd, internalErrorBytes(id)); err != nil {
						return err
					}
					continue
				}
			}
		}
		resp, respond := s.Handle(ctx, c, ev.body)
		if !respond {
			continue
		}
		if err := writeStdio(out, ev.nd, resp); err != nil {
			return err
		}
	}
}

// writeStdio writes one message in the request's framing with a single Write and
// flushes writers that buffer.
func writeStdio(out io.Writer, nd bool, body []byte) error {
	var b bytes.Buffer
	if nd {
		b.Grow(len(body) + 1)
		b.Write(body)
		b.WriteByte('\n')
	} else {
		b.Grow(len(body) + 32)
		b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
		b.Write(body)
	}
	if _, err := out.Write(b.Bytes()); err != nil {
		return err
	}
	if f, ok := out.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// readLine reads one line (without its terminator). Data past max bytes is
// discarded and reported as tooLong. err is io.EOF at end of input; a final
// unterminated line is returned together with io.EOF.
func readLine(br *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, e := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max+2 { // room for the terminator
				tooLong = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			return nil, true, e
		}
		line = bytes.TrimRight(line, "\r\n")
		return line, false, e
	}
}

// parseContentLength accepts only 1..18 ASCII digits.
func parseContentLength(v string) (int64, bool) {
	if v == "" || len(v) > stdioMaxDigits {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// readStdio turns stdin into events until EOF or cancel, then closes events.
func readStdio(ctx context.Context, br *bufio.Reader, events chan<- stdioEvent) {
	defer close(events)
	send := func(ev stdioEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	pending := int64(-1) // declared Content-Length awaiting its blank line
	for {
		line, tooLong, err := readLine(br, stdioMaxFrame)
		switch {
		case tooLong:
			// An oversize line: if it is a message it is refused, a header line is junk.
			if !send(stdioEvent{nd: true, bad: true}) {
				return
			}
			pending = -1
		case len(bytes.TrimSpace(line)) > 0:
			trimmed := bytes.TrimSpace(line)
			if trimmed[0] == '{' {
				pending = -1
				if !send(stdioEvent{body: trimmed, nd: true}) {
					return
				}
				break
			}
			name, val, ok := strings.Cut(string(line), ":")
			if !ok {
				break // junk line: ignored, as Python does
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "content-length":
				n, valid := parseContentLength(strings.TrimSpace(val))
				if !valid {
					pending = -1
					if !send(stdioEvent{bad: true}) {
						return
					}
					break
				}
				pending = n
			}
		case pending >= 0 && err == nil:
			// Blank line after the headers: the body follows.
			n := pending
			pending = -1
			if n > stdioMaxFrame {
				if !send(stdioEvent{bad: true}) {
					return
				}
				if _, derr := io.CopyN(io.Discard, br, n); derr != nil {
					return
				}
				break
			}
			buf := make([]byte, n)
			if _, rerr := io.ReadFull(br, buf); rerr != nil {
				return // partial frame at EOF: dropped, as Python
			}
			if !send(stdioEvent{body: buf}) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
