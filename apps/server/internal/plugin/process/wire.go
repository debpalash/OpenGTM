// Package process runs manifest v2 plugins with runtime "process": a separate
// OS process (Python through the opengtm_sdk package, or any language that
// speaks the wire protocol) supervised by the Go host.
//
// The wire protocol is versioned, length-prefixed JSON over a unix socket
// inherited as file descriptor 3. It is specified in docs/plugins/process-abi.md
// and machine-readably in packages/contracts/plugin-process.v1.schema.json;
// tests validate every frame either side sends against that schema.
package process

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the wire protocol this host speaks. A plugin lists the
// versions it supports in its hello; the host picks the highest common one.
const ProtocolVersion = 1

// DefaultMaxFrameBytes bounds one frame in either direction.
const DefaultMaxFrameBytes = 16 << 20

// Message types. Plugin to host: hello, fetch, progress, log, records,
// result, failure. Host to plugin: init, fetch_result, cancel.
const (
	TypeHello       = "hello"
	TypeInit        = "init"
	TypeFetch       = "fetch"
	TypeFetchResult = "fetch_result"
	TypeProgress    = "progress"
	TypeLog         = "log"
	TypeRecords     = "records"
	TypeResult      = "result"
	TypeFailure     = "failure"
	TypeCancel      = "cancel"
)

// Error codes a plugin may report in a failure message. Codes outside this
// set are accepted and reported as given; these have defined retry semantics
// when the plugin does not say otherwise.
const (
	CodeInvalidInput    = "invalid_input"    // permanent: the inputs cannot work
	CodeAuthFailed      = "auth_failed"      // permanent: bad or missing credentials
	CodeUpstream        = "upstream_error"   // transient: the data source failed
	CodeRateLimited     = "rate_limited"     // transient
	CodeTimeout         = "timeout"          // transient
	CodeCancelled       = "cancelled"        // the host asked to stop
	CodePluginException = "plugin_exception" // permanent: unhandled exception in plugin code
	CodeProtocol        = "protocol_error"   // permanent: either side broke the contract
	CodeCrashed         = "plugin_crashed"   // transient: process died without a result
	CodeInvalidOutput   = "invalid_output"   // permanent: output did not match the manifest
	CodeBusy            = "pool_busy"        // transient: no process slot became free in time
	CodeLaunch          = "launch_failed"    // permanent: the command cannot be started
	CodeOutputTooLarge  = "output_too_large" // permanent: more records or bytes than the host accepts
)

// Hello is the first frame, plugin to host.
type Hello struct {
	Type      string   `json:"type"`
	Protocols []int    `json:"protocols"`
	SDK       SDKInfo  `json:"sdk"`
	Kinds     []string `json:"kinds,omitempty"`
}

// SDKInfo identifies the plugin-side library.
type SDKInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Language string `json:"language"`
}

// Init is the host's reply to hello and carries everything the run needs.
// Secrets holds only the values of secrets the manifest declares.
type Init struct {
	Type     string            `json:"type"`
	Protocol int               `json:"protocol"`
	RunID    string            `json:"run_id"`
	Plugin   InitPlugin        `json:"plugin"`
	Inputs   map[string]any    `json:"inputs"`
	Config   map[string]any    `json:"config,omitempty"`
	Secrets  map[string]string `json:"secrets"`
	Limits   InitLimits        `json:"limits"`
	// DeadlineUnixMS is when the host will cancel the run.
	DeadlineUnixMS int64 `json:"deadline_unix_ms"`
	// Proxy is set when the host offers a CONNECT tunnel for code that opens
	// its own TLS connections (see proxy.go). It is also exported to the
	// process as HTTPS_PROXY.
	Proxy *InitProxy `json:"proxy,omitempty"`
}

// InitProxy locates the per-run tunnel proxy.
type InitProxy struct {
	URL string `json:"url"`
}

// InitPlugin names the plugin being run.
type InitPlugin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// InitLimits are the manifest limits the host enforces.
type InitLimits struct {
	TimeoutSeconds   float64 `json:"timeout_seconds"`
	MaxPages         int     `json:"max_pages"`
	MaxResponseBytes int64   `json:"max_response_bytes"`
	MemoryMB         int     `json:"memory_mb"`
	MaxFrameBytes    int     `json:"max_frame_bytes"`
}

// Fetch asks the host to perform one HTTP request through the guarded egress
// client. The host answers with a fetch_result carrying the same id.
type Fetch struct {
	Type       string            `json:"type"`
	ID         int64             `json:"id"`
	Method     string            `json:"method,omitempty"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyBase64 string            `json:"body_base64,omitempty"`
}

// FetchEvidence is recorded by the host for every fetch (secrets redacted).
type FetchEvidence struct {
	URL         string `json:"url"`
	Status      int    `json:"status"`
	FetchedAt   string `json:"fetched_at"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// FetchError is a structured, plugin-visible failure of a fetch.
type FetchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FetchResult answers a Fetch.
type FetchResult struct {
	Type       string            `json:"type"`
	ID         int64             `json:"id"`
	Status     int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyBase64 string            `json:"body_base64,omitempty"`
	Evidence   *FetchEvidence    `json:"evidence,omitempty"`
	Error      *FetchError       `json:"error,omitempty"`
}

// Progress is best-effort; the host rate-limits what it forwards.
type Progress struct {
	Type     string  `json:"type"`
	Pages    *int    `json:"pages,omitempty"`
	Records  *int    `json:"records,omitempty"`
	Fraction float64 `json:"fraction,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// Log is a plugin log line; the host truncates, redacts and rate-limits it.
type Log struct {
	Type    string `json:"type"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message"`
}

// Record is one output row: normalized fields plus the plugin's own evidence.
// The host adds the evidence it observed itself (the fetches it performed).
type Record struct {
	Fields   map[string]any `json:"fields"`
	Evidence any            `json:"evidence,omitempty"`
}

// Records streams a batch of records before the final result.
type Records struct {
	Type    string   `json:"type"`
	Records []Record `json:"records"`
}

// Result ends a successful run. ProviderError is a provider's own "no
// result" answer (the run still succeeded).
type Result struct {
	Type          string   `json:"type"`
	Records       []Record `json:"records,omitempty"`
	CostUSD       *float64 `json:"cost_usd,omitempty"`
	Pages         *int     `json:"pages,omitempty"`
	Stopped       string   `json:"stopped,omitempty"`
	ProviderError string   `json:"provider_error,omitempty"`
}

// Failure ends an unsuccessful run with a structured error.
type Failure struct {
	Type      string         `json:"type"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable *bool          `json:"retryable,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Cancel asks the plugin to stop. The host kills the process group after
// GraceMS if the plugin is still running.
type Cancel struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"` // "cancelled", "timeout" or "shutdown"
	GraceMS int    `json:"grace_ms"`
}

// Frame errors.
var (
	ErrFrameTooLarge = errors.New("process: frame exceeds the size limit")
	ErrBadFrame      = errors.New("process: malformed frame")
)

// WriteFrame writes one length-prefixed JSON frame.
func WriteFrame(w io.Writer, msg any, maxBytes int) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	if len(body) > maxBytes {
		return fmt.Errorf("%w: %d > %d bytes", ErrFrameTooLarge, len(body), maxBytes)
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err = w.Write(buf) // one write: frames from concurrent writers must not interleave
	return err
}

// ReadFrame reads one frame. io.EOF means the peer closed between frames.
func ReadFrame(r *bufio.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: truncated header", ErrBadFrame)
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrBadFrame)
	}
	if int64(n) > int64(maxBytes) {
		return nil, fmt.Errorf("%w: %d > %d bytes", ErrFrameTooLarge, n, maxBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: truncated body", ErrBadFrame)
	}
	return body, nil
}

// frameType extracts the message type of a frame.
func frameType(body []byte) (string, error) {
	var env struct {
		Type string `json:"type"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&env); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadFrame, err)
	}
	if env.Type == "" {
		return "", fmt.Errorf("%w: missing type", ErrBadFrame)
	}
	return env.Type, nil
}

// decodeStrict decodes body into v and rejects trailing data. Unknown fields
// are accepted: the protocol evolves by adding fields.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadFrame, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data", ErrBadFrame)
	}
	return nil
}
