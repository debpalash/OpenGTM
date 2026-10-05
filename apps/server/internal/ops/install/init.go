// Package install creates and inspects an OpenGTM install directory: the
// .env with generated secrets, opengtm.yaml, a Compose file for the chosen
// profile, and the state file that `opengtm upgrade` journals into.
package install

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed assets/*
var assets embed.FS

// Asset returns an embedded deployment file (compose.yml, ...).
func Asset(name string) ([]byte, error) { return assets.ReadFile("assets/" + name) }

// Profile is a deployment profile.
type Profile string

// The deployment profiles.
const (
	Lite     Profile = "lite"
	Standard Profile = "standard"
	Full     Profile = "full"
)

// Profiles lists the valid profiles in order of size.
var Profiles = []Profile{Lite, Standard, Full}

// ParseProfile validates a profile name.
func ParseProfile(s string) (Profile, error) {
	p := Profile(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range Profiles {
		if p == v {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown profile %q (choose lite, standard or full)", s)
}

// Default image repositories published by the release workflow.
const (
	DefaultAppRepo    = "ghcr.io/debpalash/opengtm"
	DefaultServerRepo = "ghcr.io/debpalash/opengtm-server"
)

// ErrExists means init would overwrite files and was not confirmed.
var ErrExists = errors.New("install files already exist")

// InitOptions configures Init.
type InitOptions struct {
	Dir     string
	Profile Profile
	// Version is the release to install; images are tagged with it. Empty or
	// "dev" (a source build) falls back to the "latest" tag.
	Version     string
	AppImage    string // override of the full app image reference
	ServerImage string // override of the full server image reference

	PublicURL  string // https://gtm.example.com; implies production
	Production bool
	Bind       string
	Port       int

	// Yes accepts every default and never prompts.
	Yes bool
	// Force confirms overwriting existing files (they are backed up first).
	Force bool
	// RotateSecrets generates new secrets even when an existing .env has them.
	// The database volume keeps its old password; see the warning in the docs.
	RotateSecrets bool

	In  io.Reader
	Out io.Writer
	// Interactive allows prompting for values not given as options.
	Interactive bool

	Now  func() time.Time
	Rand io.Reader
	UID  int
	GID  int
}

// InitResult reports what Init did.
type InitResult struct {
	Dir             string
	Profile         Profile
	URL             string
	Written         []string
	BackedUp        []string
	CredentialsFile string // set when a new admin password was generated
	Warnings        []string
}

func (o *InitOptions) rand() io.Reader {
	if o.Rand != nil {
		return o.Rand
	}
	return rand.Reader
}

func (o *InitOptions) hex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(o.rand(), b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// fernet returns a urlsafe-base64 32-byte key, the format SECRETS_MASTER_KEY expects.
func (o *InitOptions) fernet() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(o.rand(), b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p *prompter) ask(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return def, nil
		}
		return "", err
	}
	if line = strings.TrimSpace(line); line != "" {
		return line, nil
	}
	return def, nil
}

func (p *prompter) confirm(question string) (bool, error) {
	a, err := p.ask(question+" [y/N]", "")
	return strings.EqualFold(a, "y") || strings.EqualFold(a, "yes"), err
}

// Init writes a new install into o.Dir.
func Init(o InitOptions) (*InitResult, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var pr *prompter
	if o.Interactive && !o.Yes {
		in := o.In
		if in == nil {
			in = os.Stdin
		}
		pr = &prompter{in: bufio.NewReader(in), out: o.Out}
	}

	// Collect missing choices.
	if o.Profile == "" {
		o.Profile = Lite
		if pr != nil {
			fmt.Fprintln(o.Out, "Profiles: lite (server + PostgreSQL), standard (+ Redis, workers, scheduler), full (+ SearXNG, Reacher, OpenTelemetry)")
			a, err := pr.ask("Profile", string(Lite))
			if err != nil {
				return nil, err
			}
			if o.Profile, err = ParseProfile(a); err != nil {
				return nil, err
			}
		}
	}
	if o.Port == 0 {
		o.Port = 3000
		if pr != nil {
			a, err := pr.ask("Port", "3000")
			if err != nil {
				return nil, err
			}
			if o.Port, err = strconv.Atoi(a); err != nil {
				return nil, fmt.Errorf("port %q is not a number", a)
			}
		}
	}
	if o.PublicURL == "" && pr != nil {
		a, err := pr.ask("Public URL behind your TLS proxy (blank for local use only)", "")
		if err != nil {
			return nil, err
		}
		o.PublicURL = a
	}
	if o.Port < 1 || o.Port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", o.Port)
	}
	if o.Bind == "" {
		o.Bind = "127.0.0.1"
	}
	if net.ParseIP(o.Bind) == nil && !validHostname(o.Bind) {
		return nil, fmt.Errorf("bind address %q is not an IP address or host name", o.Bind)
	}
	if o.PublicURL != "" {
		u, err := url.Parse(o.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("public URL %q must be an absolute http(s) URL", o.PublicURL)
		}
		o.PublicURL = strings.TrimRight(o.PublicURL, "/")
		o.Production = true
	}

	version := strings.TrimPrefix(o.Version, "v")
	tag := version
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	if o.AppImage == "" {
		o.AppImage = DefaultAppRepo + ":" + tag
	}
	if o.ServerImage == "" {
		o.ServerImage = DefaultServerRepo + ":" + tag
	}

	// Existing files are never overwritten without confirmation.
	names := managedFiles(o.Profile)
	var existing []string
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			existing = append(existing, n)
		}
	}
	if len(existing) > 0 && !o.Force {
		confirmed := false
		if pr != nil {
			fmt.Fprintf(o.Out, "These files already exist in %s: %s\n", dir, strings.Join(existing, ", "))
			fmt.Fprintln(o.Out, "Overwriting keeps a .bak copy and reuses the existing secrets.")
			if confirmed, err = pr.confirm("Overwrite them?"); err != nil {
				return nil, err
			}
		}
		if !confirmed {
			return nil, fmt.Errorf("%w in %s: %s. Nothing was changed; pass --force to overwrite (copies are kept as .bak), "+
				"or use `opengtm upgrade` to move an existing install to a new release",
				ErrExists, dir, strings.Join(existing, ", "))
		}
	}

	// Reuse secrets from an existing .env unless rotation was asked for: the
	// PostgreSQL volume keeps the password it was created with, so a new
	// POSTGRES_PASSWORD in .env would lock the stack out of its own database.
	var prev *EnvFile
	if !o.RotateSecrets {
		prev, _ = ReadEnv(filepath.Join(dir, ".env"))
	}
	secret := func(key string, gen func() (string, error)) (string, bool, error) {
		if prev != nil {
			if v := prev.Value(key); v != "" {
				return v, false, nil
			}
		}
		v, err := gen()
		return v, true, err
	}
	hex24 := func() (string, error) { return o.hex(24) }
	var newAdmin bool
	env := NewEnv(filepath.Join(dir, ".env"))
	res := &InitResult{Dir: dir, Profile: o.Profile}

	var secretKey, masterKey, pgPass, rtPass, adminPass string
	if secretKey, _, err = secret("SECRET_KEY", func() (string, error) { return o.hex(48) }); err != nil {
		return nil, err
	}
	if masterKey, _, err = secret("SECRETS_MASTER_KEY", o.fernet); err != nil {
		return nil, err
	}
	if pgPass, _, err = secret("POSTGRES_PASSWORD", hex24); err != nil {
		return nil, err
	}
	if rtPass, _, err = secret("YUPCHA_RUNTIME_DB_PASSWORD", hex24); err != nil {
		return nil, err
	}
	if adminPass, newAdmin, err = secret("SEED_ADMIN_PASSWORD", func() (string, error) { return o.hex(12) }); err != nil {
		return nil, err
	}
	if o.RotateSecrets && prev == nil {
		if old, rerr := ReadEnv(filepath.Join(dir, ".env")); rerr == nil && old.Value("POSTGRES_PASSWORD") != "" {
			res.Warnings = append(res.Warnings, "secrets were rotated, but an existing PostgreSQL volume keeps its old password: "+
				"run `ALTER ROLE` for the owner and runtime roles, or remove the volume (this deletes the database)")
		}
	}

	uid, gid := o.UID, o.GID
	if uid == 0 && gid == 0 {
		uid, gid = os.Getuid(), os.Getgid()
	}
	if uid <= 0 { // root or Windows (-1): containers run as 1000
		uid, gid = 1000, 1000
	}

	pgUser, pgDB := "yupcha", "yupcha"
	runtimeUser := "yupcha_runtime"
	appEnv := "local"
	if o.Production {
		appEnv = "production"
	}
	publicURL := o.PublicURL
	if publicURL == "" {
		publicURL = fmt.Sprintf("http://localhost:%d", o.Port)
	}

	env.Comment("Generated by `opengtm init` on " + now().UTC().Format(time.RFC3339) + ". Contains secrets:")
	env.Comment("keep it mode 0600 and never commit it. `opengtm upgrade` edits only the image lines.")
	env.Blank()
	env.Comment("Deployment")
	env.Set("COMPOSE_PROJECT_NAME", "opengtm")
	if o.Profile == Lite {
		env.Set("COMPOSE_PROFILES", "")
	} else {
		env.Set("COMPOSE_PROFILES", string(o.Profile))
	}
	env.Set("OPENGTM_PROFILE", string(o.Profile))
	env.Set("OPENGTM_VERSION", tag)
	env.Set("OPENGTM_APP_IMAGE", o.AppImage)
	env.Set("OPENGTM_SERVER_IMAGE", o.ServerImage)
	env.Set("OPENGTM_BIND", o.Bind)
	env.Set("OPENGTM_PORT", strconv.Itoa(o.Port))
	env.Set("OPENGTM_DATA_DIR", "./data")
	env.Set("APP_UID", strconv.Itoa(uid))
	env.Set("APP_GID", strconv.Itoa(gid))
	env.Blank()
	env.Comment("Application")
	env.Set("APP_ENV", appEnv)
	if o.PublicURL != "" {
		env.Set("CORS_ORIGINS", o.PublicURL)
	}
	env.Set("SECRET_KEY", secretKey)
	env.Set("SECRETS_MASTER_KEY", masterKey)
	env.Set("SEED_ADMIN_USERNAME", "admin")
	env.Set("SEED_ADMIN_PASSWORD", adminPass)
	env.Blank()
	env.Comment("PostgreSQL. DATABASE_URL is the owner (migrations); APP_DATABASE_URL is the")
	env.Comment("NOSUPERUSER NOBYPASSRLS runtime role every service uses, so row-level security applies.")
	env.Set("POSTGRES_USER", pgUser)
	env.Set("POSTGRES_PASSWORD", pgPass)
	env.Set("POSTGRES_DB", pgDB)
	env.Set("DATABASE_URL", fmt.Sprintf("postgresql+psycopg://%s:%s@postgres:5432/%s", pgUser, pgPass, pgDB))
	env.Set("YUPCHA_RUNTIME_DB_USER", runtimeUser)
	env.Set("YUPCHA_RUNTIME_DB_PASSWORD", rtPass)
	env.Set("APP_DATABASE_URL", fmt.Sprintf("postgresql+psycopg://%s:%s@postgres:5432/%s", runtimeUser, rtPass, pgDB))
	if o.Profile != Lite {
		env.Blank()
		env.Comment("standard and full: jobs run in the worker service, not inside the API")
		env.Set("RUN_INLINE_WORKER", "0")
		env.Set("REDIS_URL", "redis://redis:6379")
	}
	if o.Profile == Full {
		reacherKey, _, err := secret("REACHER_API_KEY", hex24)
		if err != nil {
			return nil, err
		}
		searxSecret, _, err := secret("SEARXNG_SECRET", hex24)
		if err != nil {
			return nil, err
		}
		env.Blank()
		env.Comment("full: self-hosted search, email verification and an OTLP endpoint")
		env.Set("SEARXNG_URL", "http://searxng:8080")
		env.Set("SEARXNG_SECRET", searxSecret)
		env.Set("REACHER_URL", "http://reacher:8080")
		env.Set("REACHER_API_KEY", reacherKey)
		env.Set("REACHER_ENABLED", "1")
		env.Set("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318")
	}
	env.Blank()
	env.Comment("Add provider keys, SMTP and other optional settings below, or in the app's Settings.")

	// Files to write, in order. The .env goes last so a failure part-way never
	// leaves a .env without the compose file it belongs to.
	type file struct {
		name string
		data []byte
		mode fs.FileMode
	}
	var files []file
	for _, n := range names {
		switch n {
		case ".env":
			continue
		case "opengtm.yaml":
			files = append(files, file{n, []byte(renderYAML(o.Profile)), 0o644})
		default:
			b, err := Asset(n)
			if err != nil {
				return nil, fmt.Errorf("embedded %s: %w", n, err)
			}
			files = append(files, file{n, b, 0o644})
		}
	}
	files = append(files, file{".env", env.Bytes(), 0o600})

	ts := now().UTC().Format("20060102T150405Z")
	for _, n := range existing {
		src := filepath.Join(dir, n)
		bak := src + ".bak-" + ts
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		mode := fs.FileMode(0o644)
		if n == ".env" {
			mode = 0o600
		}
		if err := writeFileAtomic(bak, raw, mode); err != nil {
			return nil, fmt.Errorf("back up %s: %w", n, err)
		}
		res.BackedUp = append(res.BackedUp, bak)
	}
	for _, f := range files {
		if err := writeFileAtomic(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return nil, err
		}
		res.Written = append(res.Written, filepath.Join(dir, f.name))
	}

	dataDir := filepath.Join(dir, "data")
	if _, err := os.Stat(dataDir); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dataDir, 0o755); err != nil {
			return nil, err
		}
		// Containers run as APP_UID; root-owned bind mounts would be read-only to them.
		if os.Geteuid() == 0 {
			_ = os.Chown(dataDir, uid, gid)
		}
		res.Written = append(res.Written, dataDir)
	}

	if newAdmin {
		res.CredentialsFile = filepath.Join(dir, ".opengtm-initial-credentials")
		body := fmt.Sprintf("URL=%s\nUSERNAME=admin\nPASSWORD=%s\n", publicURL, adminPass)
		if err := writeFileAtomic(res.CredentialsFile, []byte(body), 0o600); err != nil {
			return nil, err
		}
	}
	res.URL = publicURL

	inst, err := Load(dir)
	if err != nil {
		return nil, err
	}
	inst.State = &State{
		Schema: 1, Profile: o.Profile, Version: tag, AppImage: o.AppImage, ServerImage: o.ServerImage,
		InstalledAt: now().UTC(),
	}
	if prevInst := loadStateIfAny(dir); prevInst != nil {
		inst.State.History = prevInst.History // keep the upgrade journal across a re-init
	}
	if err := inst.SaveState(); err != nil {
		return nil, err
	}
	return res, nil
}

func loadStateIfAny(dir string) *State {
	i := &Install{Dir: dir}
	raw, err := os.ReadFile(i.StatePath())
	if err != nil {
		return nil
	}
	var s State
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// managedFiles lists the files init owns for a profile (relative to the dir).
func managedFiles(p Profile) []string {
	names := []string{".env", "opengtm.yaml", "compose.yml"}
	if p == Full {
		names = append(names, "searxng-settings.yml", "otel-collector.yaml")
	}
	return names
}

func renderYAML(p Profile) string {
	return fmt.Sprintf(`# OpenGTM configuration (profile: %s). Environment variables override these values;
# secrets such as DATABASE_URL belong in .env, not here. Reference: docs/self-hosting.md.
listen: ":8080"
legacy_api_url: "http://api:8000"
log_level: info
worker:
  concurrency: 2
  max_active_per_workspace: 2
  shutdown_grace_seconds: 30
plugins:
  signature_policy: optional
`, p)
}

func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
