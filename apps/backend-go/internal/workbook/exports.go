package workbook

// DefaultWaterfall returns a copy of enrichment.DEFAULT_WATERFALLS[target]
// (nil when the target has none), for the run planner's provider chains.
func DefaultWaterfall(target string) []string {
	return append([]string(nil), defaultWaterfalls[target]...)
}

// IsStructuredField reports a write-back-only target (STRUCTURED_FIELDS).
func IsStructuredField(target string) bool { return structuredFields[target] }
