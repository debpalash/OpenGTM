package install_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/install"
)

func initDir(t *testing.T, o install.InitOptions) (string, *install.InitResult) {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	o.Yes = true
	o.Version = "3.1.0"
	res, err := install.Init(o)
	if err != nil {
		t.Fatal(err)
	}
	return o.Dir, res
}

func TestInitLiteWritesSecureConfig(t *testing.T) {
	dir, res := initDir(t, install.InitOptions{})

	st, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600", st.Mode().Perm())
	}
	env, err := install.ReadEnv(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for key, minLen := range map[string]int{
		"SECRET_KEY": 96, "SECRETS_MASTER_KEY": 44, "POSTGRES_PASSWORD": 48,
		"YUPCHA_RUNTIME_DB_PASSWORD": 48, "SEED_ADMIN_PASSWORD": 24,
	} {
		if v := env.Value(key); len(v) < minLen {
			t.Errorf("%s = %q, want at least %d chars", key, v, minLen)
		}
	}
	// The owner and runtime passwords must differ, or the runtime role's
	// restrictions protect nothing.
	if env.Value("POSTGRES_PASSWORD") == env.Value("YUPCHA_RUNTIME_DB_PASSWORD") {
		t.Error("owner and runtime passwords are identical")
	}
	want := map[string]string{
		"OPENGTM_PROFILE": "lite", "COMPOSE_PROFILES": "", "OPENGTM_VERSION": "3.1.0",
		"OPENGTM_APP_IMAGE":    "ghcr.io/debpalash/opengtm:3.1.0",
		"OPENGTM_SERVER_IMAGE": "ghcr.io/debpalash/opengtm-server:3.1.0",
		"OPENGTM_BIND":         "127.0.0.1", "OPENGTM_PORT": "3000", "APP_ENV": "local",
	}
	for k, v := range want {
		if got, ok := env.Get(k); !ok || got != v {
			t.Errorf("%s = %q (set=%v), want %q", k, got, ok, v)
		}
	}
	if _, ok := env.Get("REDIS_URL"); ok {
		t.Error("lite must not point the API at Redis")
	}
	if !strings.Contains(env.Value("APP_DATABASE_URL"), "yupcha_runtime:"+env.Value("YUPCHA_RUNTIME_DB_PASSWORD")+"@postgres") {
		t.Errorf("APP_DATABASE_URL = %q", env.Value("APP_DATABASE_URL"))
	}

	creds, err := os.ReadFile(res.CredentialsFile)
	if err != nil || !strings.Contains(string(creds), "PASSWORD="+env.Value("SEED_ADMIN_PASSWORD")) {
		t.Errorf("credentials file = %q, %v", creds, err)
	}
	if st, _ := os.Stat(res.CredentialsFile); st.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode = %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		t.Errorf("data directory not created: %v", err)
	}

	// opengtm.yaml is accepted by the server's own loader (unknown keys are errors).
	cfg, err := config.LoadFrom(filepath.Join(dir, "opengtm.yaml"), func(k string) (string, bool) {
		if k == "DATABASE_URL" {
			return env.Value("APP_DATABASE_URL"), true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("opengtm.yaml rejected by config.Load: %v", err)
	}
	if cfg.Listen != ":8080" || cfg.LegacyAPIURL != "http://api:8000" {
		t.Errorf("config = %+v", cfg)
	}

	inst, err := install.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State.Profile != install.Lite || inst.State.Version != "3.1.0" {
		t.Errorf("state = %+v", inst.State)
	}
}

func TestInitNeverOverwritesWithoutConfirmation(t *testing.T) {
	dir, _ := initDir(t, install.InitOptions{})
	before, _ := os.ReadFile(filepath.Join(dir, ".env"))

	// --yes alone is not consent to overwrite.
	_, err := install.Init(install.InitOptions{Dir: dir, Yes: true, Version: "3.2.0", Profile: install.Standard})
	if !errors.Is(err, install.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !bytes.Equal(before, after) {
		t.Fatal(".env changed without confirmation")
	}

	// An interactive "no" is also a refusal.
	_, err = install.Init(install.InitOptions{
		Dir: dir, Interactive: true, In: strings.NewReader("standard\n\n\nn\n"), Out: &bytes.Buffer{},
	})
	if !errors.Is(err, install.ErrExists) {
		t.Fatalf("interactive no: err = %v", err)
	}
	if after, _ = os.ReadFile(filepath.Join(dir, ".env")); !bytes.Equal(before, after) {
		t.Fatal(".env changed after answering no")
	}
}

func TestForceBacksUpAndKeepsSecretsUnlessRotated(t *testing.T) {
	dir, _ := initDir(t, install.InitOptions{})
	old, _ := install.ReadEnv(filepath.Join(dir, ".env"))
	oldPG, oldKey := old.Value("POSTGRES_PASSWORD"), old.Value("SECRETS_MASTER_KEY")

	res, err := install.Init(install.InitOptions{
		Dir: dir, Yes: true, Force: true, Version: "3.2.0", Profile: install.Standard,
		Now: func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.BackedUp) != 3 {
		t.Fatalf("backed up %v", res.BackedUp)
	}
	bak, err := os.ReadFile(filepath.Join(dir, ".env.bak-20260304T050607Z"))
	if err != nil || !strings.Contains(string(bak), oldPG) {
		t.Fatalf("backup of .env: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(dir, ".env.bak-20260304T050607Z")); st.Mode().Perm() != 0o600 {
		t.Errorf("secret backup mode = %v", st.Mode().Perm())
	}
	env, _ := install.ReadEnv(filepath.Join(dir, ".env"))
	if env.Value("POSTGRES_PASSWORD") != oldPG || env.Value("SECRETS_MASTER_KEY") != oldKey {
		t.Error("existing secrets were not reused; the database volume would no longer accept the new password")
	}
	if env.Value("OPENGTM_PROFILE") != "standard" || env.Value("COMPOSE_PROFILES") != "standard" || env.Value("RUN_INLINE_WORKER") != "0" {
		t.Errorf("standard settings missing: %v", env.Keys())
	}
	if res.CredentialsFile != "" {
		t.Error("a re-init that reused the admin password must not write a new credentials file")
	}

	res, err = install.Init(install.InitOptions{Dir: dir, Yes: true, Force: true, RotateSecrets: true, Version: "3.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	env, _ = install.ReadEnv(filepath.Join(dir, ".env"))
	if env.Value("POSTGRES_PASSWORD") == oldPG {
		t.Error("--rotate-secrets kept the old password")
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "PostgreSQL volume") {
		t.Errorf("rotation must warn about the existing volume: %v", res.Warnings)
	}
}

func TestInteractivePromptsAndProduction(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	res, err := install.Init(install.InitOptions{
		Dir: dir, Interactive: true, Version: "v4.0.0", Out: &out,
		In: strings.NewReader("full\n4100\nhttps://gtm.example.com/\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := install.ReadEnv(filepath.Join(dir, ".env"))
	for k, v := range map[string]string{
		"OPENGTM_PROFILE": "full", "OPENGTM_PORT": "4100", "APP_ENV": "production",
		"CORS_ORIGINS": "https://gtm.example.com", "OPENGTM_VERSION": "4.0.0",
		"COMPOSE_PROFILES": "full", "SEARXNG_URL": "http://searxng:8080", "REACHER_ENABLED": "1",
	} {
		if got := env.Value(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if res.URL != "https://gtm.example.com" {
		t.Errorf("URL = %q", res.URL)
	}
	for _, f := range []string{"searxng-settings.yml", "otel-collector.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("full profile is missing %s", f)
		}
	}
	if !strings.Contains(out.String(), "Profile [lite]") {
		t.Errorf("prompt output:\n%s", out.String())
	}
}

func TestInitRejectsBadInput(t *testing.T) {
	for name, o := range map[string]install.InitOptions{
		"port":      {Port: 70000},
		"bind":      {Bind: "not a host!"},
		"publicurl": {PublicURL: "gtm.example.com"},
	} {
		o.Dir, o.Yes = t.TempDir(), true
		if _, err := install.Init(o); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if ents, _ := os.ReadDir(o.Dir); len(ents) != 0 {
			t.Errorf("%s: files written despite invalid input: %v", name, ents)
		}
	}
	if _, err := install.ParseProfile("enterprise"); err == nil {
		t.Error("unknown profile accepted")
	}
}

func TestEnvFileSetKeepsCommentsAndUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	body := "# keep me\nA=1\nCUSTOM=\"quoted value\"\nOPENGTM_APP_IMAGE=old\n\nB=2\nOPENGTM_APP_IMAGE=dup\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := install.ReadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Value("CUSTOM") != "quoted value" {
		t.Errorf("quoted value = %q", e.Value("CUSTOM"))
	}
	e.Set("OPENGTM_APP_IMAGE", "new")
	e.Set("ADDED", "x")
	if err := e.Save(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "# keep me\nA=1\nCUSTOM=\"quoted value\"\nOPENGTM_APP_IMAGE=new\n\nB=2\nADDED=x\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after save = %v", st.Mode().Perm())
	}
}

func TestInstallLockIsExclusive(t *testing.T) {
	dir, _ := initDir(t, install.InitOptions{})
	inst, err := install.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := inst.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Lock(); !errors.Is(err, install.ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	release, err = inst.Lock()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release()
}

func TestLoadRejectsNonInstall(t *testing.T) {
	if _, err := install.Load(t.TempDir()); !errors.Is(err, install.ErrNotInstalled) {
		t.Fatalf("err = %v", err)
	}
}

// The compose file defines the profiles. Check them structurally (no Docker
// needed) so a service added without a profile cannot silently grow `lite`.
func TestComposeProfilesMatchTheDocumentedMatrix(t *testing.T) {
	raw, err := install.Asset("compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Profiles    []string `yaml:"profiles"`
			Healthcheck any      `yaml:"healthcheck"`
			Restart     string   `yaml:"restart"`
			Ports       []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	inProfile := func(profile string) []string {
		var names []string
		for n, s := range doc.Services {
			if len(s.Profiles) == 0 {
				names = append(names, n)
				continue
			}
			for _, p := range s.Profiles {
				if p == profile {
					names = append(names, n)
				}
			}
		}
		sort.Strings(names)
		return names
	}
	want := map[string]string{
		"lite":     "api migrate postgres seed server",
		"standard": "api migrate postgres redis scheduler seed server worker",
		"full":     "api migrate otel-collector postgres reacher redis scheduler searxng seed server worker",
	}
	for profile, services := range want {
		if got := strings.Join(inProfile(profile), " "); got != services {
			t.Errorf("profile %s = %s, want %s", profile, got, services)
		}
	}
	// Every long-running service has a healthcheck, except the two images
	// that ship no shell or client to run one (documented in the file).
	noHealth := map[string]string{"reacher": "", "otel-collector": "", "migrate": "", "seed": ""}
	for n, s := range doc.Services {
		if _, skip := noHealth[n]; skip {
			continue
		}
		if s.Healthcheck == nil {
			t.Errorf("service %s has no healthcheck", n)
		}
	}
	// Nothing but the Go server is published on the host.
	for n, s := range doc.Services {
		if n != "server" && len(s.Ports) > 0 {
			t.Errorf("service %s publishes ports %v", n, s.Ports)
		}
	}
}

// With Docker available, let Compose itself judge the generated project for
// every profile: interpolation, merge keys, depends_on with required: false.
func TestComposeConfigValidForEveryProfile(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose not available")
	}
	want := map[install.Profile]string{
		install.Lite:     "api migrate postgres seed server",
		install.Standard: "api migrate postgres redis scheduler seed server worker",
		install.Full:     "api migrate otel-collector postgres reacher redis scheduler searxng seed server worker",
	}
	for _, p := range install.Profiles {
		t.Run(string(p), func(t *testing.T) {
			dir, _ := initDir(t, install.InitOptions{Profile: p})
			run := func(args ...string) string {
				cmd := exec.Command("docker", append([]string{"compose", "--project-directory", dir, "-f", "compose.yml"}, args...)...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("docker compose %v: %v\n%s", args, err, out)
				}
				return string(out)
			}
			run("config", "--quiet")
			services := strings.Fields(run("config", "--services"))
			sort.Strings(services)
			if got := strings.Join(services, " "); got != want[p] {
				t.Errorf("services = %s, want %s", got, want[p])
			}
		})
	}
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestRefreshComposeUpdatesPristineAndKeepsCustomized(t *testing.T) {
	dir, _ := initDir(t, install.InitOptions{})
	inst, _ := install.Load(dir)
	want, _ := install.Asset("compose.yml")

	// Same as shipped: nothing to do.
	if r, err := inst.RefreshCompose("t1"); err != nil || r.Action != install.RefreshUnchanged {
		t.Fatalf("unchanged: %+v %v", r, err)
	}

	// An older shipped compose.yml that nobody edited is replaced, with a copy kept.
	old := []byte("# an older release's compose\nservices: {}\n")
	if err := os.WriteFile(inst.ComposePath(), old, 0o644); err != nil {
		t.Fatal(err)
	}
	inst.State.ComposeSHA256 = sha(old)
	r, err := inst.RefreshCompose("t2")
	if err != nil || r.Action != install.RefreshUpdated {
		t.Fatalf("updated: %+v %v", r, err)
	}
	if got, _ := os.ReadFile(inst.ComposePath()); !bytes.Equal(got, want) {
		t.Error("compose.yml was not replaced with the shipped version")
	}
	if got, _ := os.ReadFile(r.Previous); !bytes.Equal(got, old) {
		t.Error("previous compose.yml not kept for rollback")
	}
	if err := inst.RestoreCompose(r.Previous); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(inst.ComposePath()); !bytes.Equal(got, old) {
		t.Error("RestoreCompose did not put the old file back")
	}

	// An edited compose.yml is never overwritten.
	edited := []byte("# my changes\nservices: {}\n")
	if err := os.WriteFile(inst.ComposePath(), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	inst.State.ComposeSHA256 = sha(old) // differs from the edited file's hash
	r, err = inst.RefreshCompose("t3")
	if err != nil || r.Action != install.RefreshCustomized {
		t.Fatalf("customized: %+v %v", r, err)
	}
	if got, _ := os.ReadFile(inst.ComposePath()); !bytes.Equal(got, edited) {
		t.Error("customized compose.yml was overwritten")
	}
	if got, _ := os.ReadFile(r.Proposed); !bytes.Equal(got, want) {
		t.Error("proposed compose.yml.new is not the shipped version")
	}
}
