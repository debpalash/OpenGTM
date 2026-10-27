// Package config loads worker configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RequiredGoSchemaVersion is the goose migration version (internal/migrate) the
// Go api/worker/mcp require at startup: goose_db_version must be at least this.
// Bump it together with a migration the Go code depends on.
const RequiredGoSchemaVersion int64 = 3

// RequiredGoSchemaRevision is the Alembic revision the Go baseline (migration 1)
// reproduces. The Go runner stamps it into alembic_version and adopts databases
// at it; Go binaries no longer read alembic_version. It must equal
// REQUIRED_GO_SCHEMA_REVISION in the Python go_executor module, which still
// checks alembic_version during the canary. Frozen: later schema changes are Go
// migrations and do not touch alembic_version.
const RequiredGoSchemaRevision = "e7b2c4d6f8a0"

const (
	minServiceTokenLen       = 32
	defaultJobTimeoutSeconds = 1800
	minJobTimeoutSeconds     = 60
	maxJobTimeoutSeconds     = 7200
)

const (
	defaultWorkerConcurrency = 4
	maxWorkerConcurrency     = 64
	defaultDBMaxConns        = 10
	defaultShutdownGrace     = 30 * time.Second
	maxShutdownGrace         = 300 * time.Second
	heartbeatInterval        = 30 * time.Second

	// Shared with Python (settings.WORKER_MAX_ACTIVE_PER_WORKSPACE): default 2,
	// clamped to [0, 64]; 0 disables the per-workspace cap.
	defaultMaxActivePerWorkspace = 2
	maxMaxActivePerWorkspace     = 64
)

type Config struct {
	DatabaseURL           string
	RedisURL              string
	InternalServiceToken  string
	WorkerConcurrency     int
	MaxActivePerWorkspace int
	DBMaxConns            int
	ShutdownGrace         time.Duration
	JobTimeout            time.Duration // wall-clock limit per job (Python JOB_TIMEOUTS["run_workbook"])
	HeartbeatInterval     time.Duration
	DebugAddr             string
	AppEnv                string
	EgressAllowHosts      []string
	// Flags carries, on the worker, AutomationsEnabled, IntentPollerEnabled, PGLeadStore,
	// AutomationsAllowLegacyOutreach and Automations (all strict-parsed).
	Flags Flags
	// BillingEnabled is BILLING_ENABLED: the automation and poller debits are no-ops without it.
	BillingEnabled bool
	// Executor carries the signal scan caps (job routing is permanently Go since the cutover).
	Executor ExecutorSettings

	// CredentialsSource is always "local" (resolve in-process from PostgreSQL via internal/secrets). The
	// former "http" mode read credentials from the Python API, which no longer exists (plan 15 WP-CUTOVER).
	CredentialsSource string
	// Secrets envelope settings.
	SecretKey, SecretsProvider, SecretsMasterKey                                        string
	VaultAddr, VaultToken, VaultTokenFile, VaultTransitKey, VaultNamespace, VaultCACert string
}

// CredentialsSourceLocal is the only credentials source.
const CredentialsSourceLocal = "local"

// Load reads configuration from the environment and validates it.
func Load() (Config, error) { return load(os.Getenv) }

func load(get func(string) string) (Config, error) {
	get = AllNative(get)
	c := Config{
		DatabaseURL:          get("DATABASE_URL"),
		RedisURL:             get("REDIS_URL"),
		InternalServiceToken: get("INTERNAL_SERVICE_TOKEN"),
		AppEnv:               get("APP_ENV"),
		DebugAddr:            get("GO_WORKER_DEBUG_ADDR"),
	}
	c.CredentialsSource = strings.ToLower(strings.TrimSpace(get("CREDENTIALS_SOURCE")))
	if c.CredentialsSource == "" {
		c.CredentialsSource = CredentialsSourceLocal
	}
	c.SecretKey = get("SECRET_KEY")
	c.SecretsProvider = strings.ToLower(strings.TrimSpace(get("SECRETS_PROVIDER")))
	c.SecretsMasterKey = get("SECRETS_MASTER_KEY")
	c.VaultAddr = get("VAULT_ADDR")
	c.VaultToken = get("VAULT_TOKEN")
	c.VaultTokenFile = get("VAULT_TOKEN_FILE")
	c.VaultTransitKey = get("VAULT_TRANSIT_KEY")
	c.VaultNamespace = get("VAULT_NAMESPACE")
	c.VaultCACert = get("VAULT_CACERT")
	var err error
	if c.WorkerConcurrency, err = intVar(get, "GO_WORKER_CONCURRENCY", defaultWorkerConcurrency, 1, maxWorkerConcurrency); err != nil {
		return Config{}, err
	}
	if c.DBMaxConns, err = intVar(get, "GO_WORKER_DB_MAX_CONNS", defaultDBMaxConns, 1, 1000); err != nil {
		return Config{}, err
	}
	if c.MaxActivePerWorkspace, err = clampVar(get, "WORKER_MAX_ACTIVE_PER_WORKSPACE", defaultMaxActivePerWorkspace, 0, maxMaxActivePerWorkspace); err != nil {
		return Config{}, err
	}
	secs, err := intVar(get, "GO_WORKER_SHUTDOWN_GRACE_SECONDS", int(defaultShutdownGrace/time.Second), 1, int(maxShutdownGrace/time.Second))
	if err != nil {
		return Config{}, err
	}
	c.ShutdownGrace = time.Duration(secs) * time.Second

	// Python JOB_TIMEOUTS["run_workbook"] is 1800s; clamp so a typo cannot
	// disable the watchdog or wedge a slot for days.
	jt, err := clampVar(get, "GO_WORKER_JOB_TIMEOUT_SECONDS", defaultJobTimeoutSeconds, minJobTimeoutSeconds, maxJobTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	c.JobTimeout = time.Duration(jt) * time.Second

	hb, err := intVar(get, "GO_WORKER_HEARTBEAT_SECONDS", int(heartbeatInterval/time.Second), 1, 3600)
	if err != nil {
		return Config{}, err
	}
	c.HeartbeatInterval = time.Duration(hb) * time.Second
	if c.DebugAddr == "" {
		c.DebugAddr = "127.0.0.1:6060"
	}

	c.EgressAllowHosts = parseHostList(get("EGRESS_ALLOW_HOSTS"))
	if c.Flags.AutomationsEnabled, err = boolVar(get, "AUTOMATIONS_ENABLED", false); err != nil {
		return Config{}, err
	}
	if c.Flags.Automations, err = loadAutomations(get); err != nil {
		return Config{}, err
	}
	if c.Flags.Governance, err = loadGovernance(get); err != nil {
		return Config{}, err
	}
	if c.Flags.IntentPollerEnabled, err = boolVar(get, "INTENT_POLLER_ENABLED", false); err != nil {
		return Config{}, err
	}
	if c.Flags.PGLeadStore, err = boolVar(get, "PG_LEAD_STORE", true); err != nil {
		return Config{}, err
	}
	if c.Flags.AutomationsAllowLegacyOutreach, err = boolVar(get, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", false); err != nil {
		return Config{}, err
	}
	if c.Flags.Outreach, err = loadOutreach(get); err != nil {
		return Config{}, err
	}
	if c.BillingEnabled, err = boolVar(get, "BILLING_ENABLED", false); err != nil {
		return Config{}, err
	}
	if c.Executor, err = loadExecutor(get); err != nil {
		return Config{}, err
	}
	if c.InternalServiceToken == "" {
		return Config{}, errors.New("config: INTERNAL_SERVICE_TOKEN is required")
	}
	if len(c.InternalServiceToken) < minServiceTokenLen || strings.HasPrefix(c.InternalServiceToken, "change-me") {
		return Config{}, fmt.Errorf("config: INTERNAL_SERVICE_TOKEN must be at least %d characters and must not be a change-me placeholder", minServiceTokenLen)
	}
	// EGRESS_ALLOW_HOSTS is an operator allow-list (documented deviation in
	// testdata/contract/README.md): the provider client honours it in every
	// APP_ENV (internal/egress NewOperatorClient); the http column and SMTP
	// probes still honour it only in test/bench.
	if err := c.validateCredentialsSource(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validateCredentialsSource checks the CREDENTIALS_SOURCE selection. Values are
// never echoed into errors.
func (c Config) validateCredentialsSource() error {
	switch c.CredentialsSource {
	case CredentialsSourceLocal:
	case "http":
		return errors.New(`config: CREDENTIALS_SOURCE=http was removed with the Python API; unset it or set "local"`)
	default:
		return fmt.Errorf("config: CREDENTIALS_SOURCE must be %q", CredentialsSourceLocal)
	}
	if c.DatabaseURL == "" {
		return errors.New("config: CREDENTIALS_SOURCE=local requires DATABASE_URL")
	}
	if c.SecretKey == "" {
		return errors.New("config: CREDENTIALS_SOURCE=local requires SECRET_KEY")
	}
	switch c.SecretsProvider {
	case "", "local":
	case "vault_transit":
		if strings.TrimSpace(c.VaultAddr) == "" {
			return errors.New("config: SECRETS_PROVIDER=vault_transit requires VAULT_ADDR")
		}
	default:
		return errors.New("config: unsupported SECRETS_PROVIDER (want local or vault_transit)")
	}
	return nil
}

// clampVar is like intVar but clamps out-of-range values (Python's
// max(lo, min(hi, n))); a non-integer is still an error.
func clampVar(get func(string) string, key string, def, lo, hi int) (int, error) {
	s := get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return max(lo, min(hi, n)), nil
}

func intVar(get func(string) string, key string, def, lo, hi int) (int, error) {
	s := get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("config: %s=%d out of range [%d, %d]", key, n, lo, hi)
	}
	return n, nil
}

// redactURL masks the password component of a connection URL.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable url redacted]"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	if q := u.Query(); q.Has("password") {
		q.Set("password", "xxxxx")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// redacted returns a copy safe to print or log.
func (c Config) redacted() Config {
	c.DatabaseURL = redactURL(c.DatabaseURL)
	c.RedisURL = redactURL(c.RedisURL)
	if c.InternalServiceToken != "" {
		c.InternalServiceToken = "[redacted]"
	}
	for _, p := range []*string{&c.SecretKey, &c.SecretsMasterKey, &c.VaultToken} {
		if *p != "" {
			*p = "[redacted]"
		}
	}
	return c
}

type plain Config // no methods: avoids String recursion

// String implements fmt.Stringer with secrets redacted (covers %v and %+v).
func (c Config) String() string { return fmt.Sprintf("%+v", plain(c.redacted())) }

// LogValue implements slog.LogValuer with secrets redacted.
func (c Config) LogValue() slog.Value {
	r := c.redacted()
	return slog.GroupValue(
		slog.String("database_url", r.DatabaseURL),
		slog.String("redis_url", r.RedisURL),
		slog.String("internal_service_token", r.InternalServiceToken),
		slog.Int("worker_concurrency", r.WorkerConcurrency),
		slog.Int("max_active_per_workspace", r.MaxActivePerWorkspace),
		slog.Int("db_max_conns", r.DBMaxConns),
		slog.Duration("shutdown_grace", r.ShutdownGrace),
		slog.Duration("heartbeat_interval", r.HeartbeatInterval),
		slog.String("debug_addr", r.DebugAddr),
		slog.String("app_env", r.AppEnv),
		slog.Any("egress_allow_hosts", r.EgressAllowHosts),
		slog.String("credentials_source", r.CredentialsSource),
		slog.String("secrets_provider", r.SecretsProvider),
	)
}

// InsecureDefaultSecretKey is the never-safe-in-prod default signing key
// (Python INSECURE_DEFAULT_SECRET_KEY).
const InsecureDefaultSecretKey = "INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY"

// minSecretKeyBytes is the minimum HS256 signing-key length outside explicit dev.
const minSecretKeyBytes = 32

// devEnvs mirrors _DEV_ENVS in legacy/python/apps/api/core/config.py: the only
// APP_ENV values where the insecure default SECRET_KEY is tolerated.
var devEnvs = map[string]bool{"dev": true, "development": true, "test": true, "testing": true, "local": true}

// CheckSecretKey is the shared startup gate of the api, worker and mcp binaries: SECRET_KEY must be
// set, not the insecure default and at least minSecretKeyBytes long. Only APP_ENV=test is exempt (dev,
// development, testing and local are not: a dev-labelled compose stack still signs real tokens).
// The key never appears in the error.
func CheckSecretKey(secretKey, appEnv string) error {
	if strings.EqualFold(strings.TrimSpace(appEnv), "test") {
		return nil
	}
	if strings.Contains(secretKey, InsecureDefaultSecretKey) || len(secretKey) < minSecretKeyBytes {
		return fmt.Errorf("config: refusing to start: SECRET_KEY must be set, not the insecure default, and at least %d bytes unless APP_ENV=test; APP_ENV=%q", minSecretKeyBytes, appEnv)
	}
	return nil
}

// IsDevEnv reports whether appEnv is one of Python's _DEV_ENVS (case and edge whitespace ignored).
func IsDevEnv(appEnv string) bool { return devEnvs[strings.ToLower(strings.TrimSpace(appEnv))] }

// APIConfig is the configuration of cmd/api. Names mirror the Python settings.
type APIConfig struct {
	DatabaseURL      string
	Addr             string // API_ADDR
	SecretKey        string
	Algorithm        string // only HS256
	AccessTTL        time.Duration
	RefreshTTL       time.Duration
	CORSOrigins      string
	AppEnv           string
	MCPTokenTTLDays  int
	ScimTokenTTLDays int // OPENGTM_SCIM_TOKEN_TTL_DAYS, clamped 1..365
	DBMaxConns       int
	// RLSRequireSafeRole is PG_RLS_REQUIRE_SAFE_ROLE (default true): refuse to
	// start when the DB role is superuser/BYPASSRLS; false only logs CRITICAL.
	RLSRequireSafeRole bool

	// Billing (Python settings.BILLING_ENABLED / STRIPE_*). The keys are secrets:
	// redacted in String/LogValue and never echoed.
	BillingEnabled      bool
	StripeSecretKey     string
	StripeWebhookSecret string
	StripeAPIBase       string // STRIPE_API_BASE; honoured only when APP_ENV is "test"

	// Enrichment jobs (plan 06).
	WSMaxMessageBytes int // WS_MAX_MESSAGE_BYTES (default 65536)

	// Workbook CRUD (plan 04).
	Flags   Flags
	DataDir string // DATA_DIR; default <cwd>/data

	// Leads, signals and events (plan 05).
	RedisURL      string // REDIS_URL; empty means heartbeat-only SSE
	SSEMaxStreams int    // SSE_MAX_STREAMS (default 1000, >= 1)
	// Hardening beyond Python (no limits there): per-user and per-workspace stream caps
	// (429 when exceeded) and a maximum stream lifetime after which the server closes the
	// stream so EventSource reconnects and re-authenticates.
	SSEMaxPerUser      int           // SSE_MAX_STREAMS_PER_USER (default 10)
	SSEMaxPerWorkspace int           // SSE_MAX_STREAMS_PER_WORKSPACE (default 200)
	SSEMaxLifetime     time.Duration // SSE_MAX_LIFETIME_SECONDS (default 1800, 30..86400)
	// EgressAllowHosts is EGRESS_ALLOW_HOSTS: exact host[:port] operator entries that skip the private-address
	// block on API-side outbound clients (copilot LLM, person intel, scrape stats), as on the worker.
	EgressAllowHosts []string
}

// parseHostList splits a comma list, trimming blanks.
func parseHostList(v string) []string {
	var out []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// FilesAPIModeHardened is the only files-API mode: auth + global admin and a safe path
// check on /api/files and /api/system. FILES_API_MODE=legacy (unauthenticated FastAPI
// parity) was removed (S-M4) and is rejected at startup in every APP_ENV.
const FilesAPIModeHardened = "hardened"

// Flags are the feature flags shared with the Python settings.
type Flags struct {
	// ImportMemoryBudgetMB is IMPORT_MEMORY_BUDGET_MB (default 512, 64..65536): upload
	// bytes the workbook import/preview routes may hold in flight at once; a decoded
	// import costs roughly ten times its body size.
	ImportMemoryBudgetMB int
	// Hardening beyond Python (plan 04 final review); each zero value means the default.
	ExportMaxConcurrent            int    // EXPORT_MAX_CONCURRENT (default 8): CSV exports streaming at once
	ExportMaxPerWorkspace          int    // EXPORT_MAX_PER_WORKSPACE (default 2)
	ExportWriteTimeoutSeconds      int    // EXPORT_WRITE_TIMEOUT_SECONDS (default 15): per-write deadline of a CSV export
	WorkbookMaxAddRows             int    // WORKBOOK_MAX_ADD_ROWS (default 10000): rows per add-rows request
	WorkbookMaxRowKeys             int    // WORKBOOK_MAX_ROW_KEYS (default 2000): keys in one added row
	WorkbookMaxViewFilters         int    // WORKBOOK_MAX_VIEW_FILTERS (default 50)
	WorkbookMaxViewSorts           int    // WORKBOOK_MAX_VIEW_SORTS (default 10)
	WorkbookMaxColumns             int    // WORKBOOK_MAX_COLUMNS (default 500)
	PGLeadStore                    bool   // PG_LEAD_STORE (default true); Go supports only the Postgres lead store
	GoRunRoutesEnabled             bool   // GO_RUN_ROUTES_ENABLED (default false): enrichment run/ingest/ws routes
	GoConnectorRoutesEnabled       bool   // GO_CONNECTOR_ROUTES_ENABLED (default false): connectors, hubspot, /api/data, ambitionbox routes
	CertificationPath              string // OPENGTM_INTEGRATION_CERTIFICATIONS
	CertificationKey               Secret // OPENGTM_INTEGRATION_CERTIFICATION_KEY (secret; never logged)
	BuildSHA                       string // OPENGTM_BUILD_SHA
	CRMImportEnabled               bool   // CRM_IMPORT_ENABLED (read-only mirror; crm_import stays on Python)
	AutomationsEnabled             bool   // AUTOMATIONS_ENABLED
	IngestAPIEnabled               bool   // INGEST_API_ENABLED (default false): inbound-rows API (ingest-token, rows/ingest)
	IntentPollerEnabled            bool   // INTENT_POLLER_ENABLED
	AutomationsAllowLegacyOutreach bool   // AUTOMATIONS_ALLOW_LEGACY_OUTREACH
	FilesAPIMode                   string
	Automations                    AutomationsSettings // plan 08; see automations.go
	Outreach                       OutreachSettings    // plan 09; see outreach.go
	Audiences                      AudienceSettings    // plan 10; see audiences.go
	Governance                     GovernanceSettings  // plan 11; see governance.go
	Copilot                        CopilotSettings     // plan 12; see copilot.go
	Leadgen                        LeadgenSettings     // plan 14; see leadgen.go
}

// DefaultStripeAPIBase is the production Stripe API origin.
const DefaultStripeAPIBase = "https://api.stripe.com"

// LoadAPI reads the API configuration from the environment and validates it.
func LoadAPI() (APIConfig, error) { return loadAPI(os.Getenv) }

func loadAPI(get func(string) string) (APIConfig, error) {
	get = AllNative(get)
	c := APIConfig{
		DatabaseURL: get("DATABASE_URL"),
		Addr:        get("API_ADDR"),
		SecretKey:   get("SECRET_KEY"),
		Algorithm:   get("ALGORITHM"),
		CORSOrigins: get("CORS_ORIGINS"),
		AppEnv:      get("APP_ENV"),
	}
	c.EgressAllowHosts = parseHostList(get("EGRESS_ALLOW_HOSTS"))
	if c.Addr == "" {
		c.Addr = ":8000"
	}
	if strings.TrimSpace(get("TRUST_PROXY")) != "" {
		// S-L18: TRUST_PROXY never changed anything (client-IP trust is TRUST_PROXY_CIDRS, default loopback only).
		slog.Warn("config: TRUST_PROXY is deprecated and ignored; set TRUST_PROXY_CIDRS to the proxy network (docker bridge subnet or pod CIDR)")
	}
	if c.SecretKey == "" {
		c.SecretKey = InsecureDefaultSecretKey
	}
	if c.Algorithm == "" {
		c.Algorithm = "HS256"
	}
	if c.CORSOrigins == "" {
		c.CORSOrigins = "*"
	}
	appEnvRaw := c.AppEnv
	if c.AppEnv == "" {
		c.AppEnv = "dev"
	}
	if c.DatabaseURL == "" {
		return APIConfig{}, errors.New("config: DATABASE_URL is required")
	}
	if c.Algorithm != "HS256" {
		return APIConfig{}, errors.New("config: ALGORITHM must be HS256")
	}
	// R-secret: only an explicit APP_ENV=test tolerates a missing or weak key (an unset APP_ENV does not).
	if err := CheckSecretKey(c.SecretKey, appEnvRaw); err != nil {
		return APIConfig{}, err
	}
	const maxMinutes = 60 * 24 * 365 * 10
	am, err := intVar(get, "ACCESS_TOKEN_EXPIRE_MINUTES", 30, 1, maxMinutes)
	if err != nil {
		return APIConfig{}, err
	}
	rm, err := intVar(get, "REFRESH_TOKEN_EXPIRE_MINUTES", 60*24*14, 1, maxMinutes)
	if err != nil {
		return APIConfig{}, err
	}
	c.AccessTTL, c.RefreshTTL = time.Duration(am)*time.Minute, time.Duration(rm)*time.Minute
	// Python parses any int: <= 0 means tokens never expire (handler checks ttl > 0).
	if c.MCPTokenTTLDays, err = intVar(get, "MCP_TOKEN_TTL_DAYS", 90, math.MinInt32, math.MaxInt32); err != nil {
		return APIConfig{}, err
	}
	if c.ScimTokenTTLDays, err = clampVar(get, "OPENGTM_SCIM_TOKEN_TTL_DAYS", 90, 1, 365); err != nil {
		return APIConfig{}, err
	}
	if c.RLSRequireSafeRole, err = boolVar(get, "PG_RLS_REQUIRE_SAFE_ROLE", true); err != nil {
		return APIConfig{}, err
	}
	if c.DBMaxConns, err = intVar(get, "API_DB_MAX_CONNS", defaultDBMaxConns, 1, 1000); err != nil {
		return APIConfig{}, err
	}
	if c.BillingEnabled, err = boolVar(get, "BILLING_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	c.StripeSecretKey = strings.TrimSpace(get("STRIPE_SECRET_KEY"))
	c.StripeWebhookSecret = strings.TrimSpace(get("STRIPE_WEBHOOK_SECRET"))
	c.StripeAPIBase = DefaultStripeAPIBase
	if strings.EqualFold(strings.TrimSpace(c.AppEnv), "test") {
		if base := strings.TrimRight(strings.TrimSpace(get("STRIPE_API_BASE")), "/"); base != "" {
			c.StripeAPIBase = base
		}
	}
	if c.Flags.PGLeadStore, err = boolVar(get, "PG_LEAD_STORE", true); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.Copilot, err = loadCopilot(get, c.Flags.PGLeadStore); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.Leadgen, err = loadLeadgen(get); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.ImportMemoryBudgetMB, err = intVar(get, "IMPORT_MEMORY_BUDGET_MB", 512, 64, 65536); err != nil {
		return APIConfig{}, err
	}
	for _, v := range []struct {
		dst  *int
		name string
		def  int
		max  int
	}{
		{&c.Flags.ExportMaxConcurrent, "EXPORT_MAX_CONCURRENT", 8, 1000},
		{&c.Flags.ExportMaxPerWorkspace, "EXPORT_MAX_PER_WORKSPACE", 2, 1000},
		{&c.Flags.ExportWriteTimeoutSeconds, "EXPORT_WRITE_TIMEOUT_SECONDS", 15, 3600},
		{&c.Flags.WorkbookMaxAddRows, "WORKBOOK_MAX_ADD_ROWS", 10000, 1000000},
		{&c.Flags.WorkbookMaxRowKeys, "WORKBOOK_MAX_ROW_KEYS", 2000, 100000},
		{&c.Flags.WorkbookMaxViewFilters, "WORKBOOK_MAX_VIEW_FILTERS", 50, 10000},
		{&c.Flags.WorkbookMaxViewSorts, "WORKBOOK_MAX_VIEW_SORTS", 10, 1000},
		{&c.Flags.WorkbookMaxColumns, "WORKBOOK_MAX_COLUMNS", 500, 100000},
	} {
		if *v.dst, err = intVar(get, v.name, v.def, 1, v.max); err != nil {
			return APIConfig{}, err
		}
	}
	if c.Flags.GoConnectorRoutesEnabled, err = boolVar(get, "GO_CONNECTOR_ROUTES_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.CRMImportEnabled, err = boolVar(get, "CRM_IMPORT_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	c.Flags.CertificationPath = strings.TrimSpace(get("OPENGTM_INTEGRATION_CERTIFICATIONS"))
	c.Flags.CertificationKey = Secret(strings.TrimSpace(get("OPENGTM_INTEGRATION_CERTIFICATION_KEY")))
	c.Flags.BuildSHA = strings.TrimSpace(get("OPENGTM_BUILD_SHA"))
	if c.Flags.AutomationsEnabled, err = boolVar(get, "AUTOMATIONS_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.IngestAPIEnabled, err = boolVar(get, "INGEST_API_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.Automations, err = loadAutomations(get); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.Outreach, err = loadOutreach(get); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.Audiences, err = loadAudiences(get); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.Governance, err = loadGovernance(get); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.IntentPollerEnabled, err = boolVar(get, "INTENT_POLLER_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	if c.Flags.AutomationsAllowLegacyOutreach, err = boolVar(get, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", false); err != nil {
		return APIConfig{}, err
	}
	c.Flags.FilesAPIMode = strings.ToLower(strings.TrimSpace(get("FILES_API_MODE")))
	switch c.Flags.FilesAPIMode {
	case "":
		c.Flags.FilesAPIMode = FilesAPIModeHardened
	case FilesAPIModeHardened:
	default:
		return APIConfig{}, fmt.Errorf("config: FILES_API_MODE must be %q (the unauthenticated legacy mode was removed)", FilesAPIModeHardened)
	}
	if c.DataDir = strings.TrimSpace(get("DATA_DIR")); c.DataDir == "" {
		c.DataDir = "data"
	}
	c.RedisURL = strings.TrimSpace(get("REDIS_URL"))
	if c.Flags.GoRunRoutesEnabled, err = boolVar(get, "GO_RUN_ROUTES_ENABLED", false); err != nil {
		return APIConfig{}, err
	}
	if c.WSMaxMessageBytes, err = intVar(get, "WS_MAX_MESSAGE_BYTES", 65536, 1, 1<<30); err != nil {
		return APIConfig{}, err
	}
	if c.SSEMaxStreams, err = intVar(get, "SSE_MAX_STREAMS", 1000, 1, 1000000); err != nil {
		return APIConfig{}, err
	}
	if c.SSEMaxPerUser, err = intVar(get, "SSE_MAX_STREAMS_PER_USER", 10, 1, 1000000); err != nil {
		return APIConfig{}, err
	}
	if c.SSEMaxPerWorkspace, err = intVar(get, "SSE_MAX_STREAMS_PER_WORKSPACE", 200, 1, 1000000); err != nil {
		return APIConfig{}, err
	}
	life, err := intVar(get, "SSE_MAX_LIFETIME_SECONDS", 1800, 30, 86400)
	if err != nil {
		return APIConfig{}, err
	}
	c.SSEMaxLifetime = time.Duration(life) * time.Second
	return c, nil
}

func (c APIConfig) redacted() APIConfig {
	c.DatabaseURL = redactURL(c.DatabaseURL)
	c.RedisURL = redactURL(c.RedisURL)
	if c.StripeSecretKey != "" {
		c.StripeSecretKey = "[redacted]"
	}
	if c.StripeWebhookSecret != "" {
		c.StripeWebhookSecret = "[redacted]"
	}
	if c.SecretKey != "" {
		c.SecretKey = "[redacted]"
	}
	return c
}

type plainAPI APIConfig

// String implements fmt.Stringer with secrets redacted (covers %v and %+v).
func (c APIConfig) String() string { return fmt.Sprintf("%+v", plainAPI(c.redacted())) }

// LogValue implements slog.LogValuer with secrets redacted.
func (c APIConfig) LogValue() slog.Value {
	r := c.redacted()
	return slog.GroupValue(
		slog.String("database_url", r.DatabaseURL),
		slog.String("addr", r.Addr),
		slog.String("secret_key", r.SecretKey),
		slog.String("algorithm", r.Algorithm),
		slog.Duration("access_ttl", r.AccessTTL),
		slog.Duration("refresh_ttl", r.RefreshTTL),
		slog.String("cors_origins", r.CORSOrigins),
		slog.String("app_env", r.AppEnv),
		slog.Int("mcp_token_ttl_days", r.MCPTokenTTLDays),
		slog.Int("scim_token_ttl_days", r.ScimTokenTTLDays),
		slog.Bool("billing_enabled", r.BillingEnabled),
		slog.String("stripe_secret_key", r.StripeSecretKey),
		slog.String("stripe_webhook_secret", r.StripeWebhookSecret),
		slog.String("stripe_api_base", r.StripeAPIBase),
		slog.Bool("pg_lead_store", r.Flags.PGLeadStore),
		slog.Bool("automations_enabled", r.Flags.AutomationsEnabled),
		slog.Bool("intent_poller_enabled", r.Flags.IntentPollerEnabled),
		slog.String("files_api_mode", r.Flags.FilesAPIMode),
		slog.String("data_dir", r.DataDir),
		slog.String("redis_url", r.RedisURL),
		slog.Int("sse_max_streams", r.SSEMaxStreams),
		slog.Int("sse_max_per_user", r.SSEMaxPerUser),
		slog.Int("sse_max_per_workspace", r.SSEMaxPerWorkspace),
		slog.Duration("sse_max_lifetime", r.SSEMaxLifetime),
	)
}

// boolVar parses a pydantic-style boolean (true/false/1/0/yes/no/on/off, any case).
func boolVar(get func(string) string, key string, def bool) (bool, error) {
	s := strings.ToLower(strings.TrimSpace(get(key)))
	switch s {
	case "":
		return def, nil
	case "true", "1", "yes", "on", "t", "y":
		return true, nil
	case "false", "0", "no", "off", "f", "n":
		return false, nil
	}
	return false, fmt.Errorf("config: %s: not a boolean", key)
}

// Secret is a string that never renders: fmt (any verb), slog, JSON and text
// marshalling all yield "[redacted]". Reveal returns the value.
type Secret string

// Reveal returns the underlying secret.
func (s Secret) Reveal() string { return string(s) }

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return `"[redacted]"` }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte("[redacted]")) }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
