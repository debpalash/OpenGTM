// Package kernels runs OpenGTM's Rust data-plane kernels. The kernels are
// compiled to WebAssembly (crates/opengtm-kernels) and executed in-process
// through the Extism runtime, so the server binary stays free of cgo.
package kernels

// ExtractRequest selects records and fields from one HTML or JSON document.
//
// Selector specs use a prefix:
//
//	css:<selector>                text content of the first match
//	css:<selector>::text          same as above, explicit
//	css:<selector>::attr(<name>)  attribute value of the first match
//	css:<selector>::html          inner HTML of the first match
//	json:<JSONPath>               value at a JSONPath (JSON documents only)
//	re:<regex>                    first capture group (or whole match) in the text
//
// When Items is set, each match of Items becomes one record and field
// selectors run relative to it. When Items is empty, the whole document is one
// record. Relative URLs in ::attr(href|src) values resolve against BaseURL.
type ExtractRequest struct {
	Document string            `json:"document"`
	Format   string            `json:"format"` // "html" (default) or "json"
	BaseURL  string            `json:"base_url,omitempty"`
	Items    string            `json:"items,omitempty"`
	Fields   map[string]string `json:"fields"`
	Limit    int               `json:"limit,omitempty"` // 0 means the kernel default (1000)
}

// ExtractResult holds one map per record. A field with no match is omitted.
type ExtractResult struct {
	Records []map[string]string `json:"records"`
}

// PersonName is the normalized form of a person's name.
type PersonName struct {
	Full  string `json:"full"`
	First string `json:"first"`
	Last  string `json:"last"`
}
