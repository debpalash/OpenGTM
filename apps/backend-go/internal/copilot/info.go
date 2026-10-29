package copilot

import (
	"bytes"
	"encoding/json"
	"sync"
)

var (
	infoOnce sync.Once
	infoBody []byte
)

// InfoResponse is Python's _INFO_RESPONSE as compact JSON (key order as in the source dict), taken from
// catalog.json's info_response. The slice is a copy.
func InfoResponse() []byte {
	infoOnce.Do(func() {
		var f struct {
			Info json.RawMessage `json:"info_response"`
		}
		if err := json.Unmarshal(catalogJSON, &f); err != nil {
			panic("copilot: catalog.json: " + err.Error()) // embedded at build time
		}
		var b bytes.Buffer
		if err := json.Compact(&b, f.Info); err != nil {
			panic("copilot: catalog.json info_response: " + err.Error())
		}
		infoBody = b.Bytes()
	})
	return append([]byte(nil), infoBody...)
}
