package config

// SourceWbSettings are the plan 15 WP-SOURCEWB switches (source_workbook job and the
// source preview route), plus the two Python feature flags the source engine reads.
//
// GO_EXECUTOR_SOURCE_WORKBOOK (default false): source_workbook jobs are enqueued with
// executor 'go' for workspaces in the GO_EXECUTOR_ENABLED + GO_EXECUTOR_WORKSPACES cohort,
// and the Go worker runs them (otherwise it hands them back to Python).
// GO_SOURCE_PREVIEW_ENABLED (default false): Go serves
// GET /api/workbooks/{id}/sources/{col}/preview instead of FastAPI.
// CRM_IMPORT_ENABLED and PEOPLE_SEARCH_SOURCE_ENABLED are settings.py's flags of the
// crm_import and people_search source kinds (default off: clean refusal, no network).
type SourceWbSettings struct {
	GoExecutor          bool // GO_EXECUTOR_SOURCE_WORKBOOK
	PreviewEnabled      bool // GO_SOURCE_PREVIEW_ENABLED
	CRMImportEnabled    bool // CRM_IMPORT_ENABLED
	PeopleSearchEnabled bool // PEOPLE_SEARCH_SOURCE_ENABLED
}

// LoadSourceWb reads the switches strictly; a malformed value is a startup error.
func LoadSourceWb(get func(string) string) (SourceWbSettings, error) {
	get = AllNative(get) // retired flags read as on
	var s SourceWbSettings
	for _, f := range []struct {
		dst *bool
		key string
	}{
		{&s.GoExecutor, "GO_EXECUTOR_SOURCE_WORKBOOK"},
		{&s.PreviewEnabled, "GO_SOURCE_PREVIEW_ENABLED"},
		{&s.CRMImportEnabled, "CRM_IMPORT_ENABLED"},
		{&s.PeopleSearchEnabled, "PEOPLE_SEARCH_SOURCE_ENABLED"},
	} {
		v, err := boolVar(get, f.key, false)
		if err != nil {
			return s, err
		}
		*f.dst = v
	}
	return s, nil
}

// SourceWbFrom reads the switches leniently (pydantic bool; malformed = off), for
// request handlers that resolve them per call.
func SourceWbFrom(get func(string) string) SourceWbSettings {
	get = AllNative(get) // retired flags read as on
	return SourceWbSettings{
		GoExecutor:          pyBool(get("GO_EXECUTOR_SOURCE_WORKBOOK")),
		PreviewEnabled:      pyBool(get("GO_SOURCE_PREVIEW_ENABLED")),
		CRMImportEnabled:    pyBool(get("CRM_IMPORT_ENABLED")),
		PeopleSearchEnabled: pyBool(get("PEOPLE_SEARCH_SOURCE_ENABLED")),
	}
}

// SourceWorkbookExecutor is the executor of a new source_workbook job: always "go"
// (GO_EXECUTOR_SOURCE_WORKBOOK is retired).
func SourceWorkbookExecutor(func(string) string, string) string { return ExecutorGo }
