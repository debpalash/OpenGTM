package wbcrud

import (
	"fmt"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook/outurl"
)

// ValidateOutputColumns refuses a changed output column whose webhook URL template puts a placeholder in the
// scheme, userinfo, host or port (outurl.ValidateTemplate): the same rule delivery enforces, applied when the
// column is saved so the caller gets a 422 instead of a failed cell at run time. Columns equal to their
// counterpart in before are not re-checked, so an already stored column never blocks an unrelated save. The
// message names the column id, never the URL.
func ValidateOutputColumns(before, after []Column) *GuardError {
	old := map[string]string{}
	for _, c := range before {
		old[pyStr(c.Get("id"))] = DumpsSorted(c.o)
	}
	for _, c := range after {
		if pyStr(c.Get("type")) != "output" {
			continue
		}
		if prev, ok := old[pyStr(c.Get("id"))]; ok && prev == DumpsSorted(c.o) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(pyStr(c.Get("destination"))), "webhook") {
			continue
		}
		cfg, _ := c.Get("destination_config").(*Obj)
		if cfg == nil {
			continue
		}
		url, _ := cfg.Get("url")
		s, ok := url.(string)
		if !ok {
			continue
		}
		if err := outurl.ValidateTemplate(s); err != nil {
			return &GuardError{Status: 422, Detail: fmt.Sprintf("Output column %s: the webhook URL scheme, credentials, host and port must be written literally; placeholders are only allowed in the path or query", pyStr(c.Get("id")))}
		}
	}
	return nil
}
