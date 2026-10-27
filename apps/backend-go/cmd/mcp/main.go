// Command mcp is the OpenGTM MCP server: stdio by default, `--sse [port]` for
// the loopback HTTP transport. stdout carries protocol bytes only; every log
// and diagnostic goes to stderr.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const defaultSSEPort = 3100

// parseTransport mirrors the Python argv handling: `--sse` selects SSE and an
// all-digit next argument is the port (anything else means the default 3100).
func parseTransport(args []string) (sse bool, port int, err error) {
	for i, a := range args {
		if a != "--sse" {
			continue
		}
		port = defaultSSEPort
		if i+1 < len(args) && isDigits(args[i+1]) {
			p, perr := strconv.Atoi(args[i+1])
			if perr != nil || p < 1 || p > 65535 {
				return false, 0, fmt.Errorf("invalid --sse port %q", args[i+1])
			}
			port = p
		}
		return true, port, nil
	}
	return false, 0, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// loopbackHost reports whether host names only the loopback interface.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// runtime is everything a transport needs, built once per process.
type runtime struct {
	deps  mcp.Deps
	srv   *mcp.Server
	close func()
}

const (
	dbLockTimeout = "30s"
	dbStmtTimeout = "60s"
	dbMaxConns    = 8
)

// openRuntime connects with the runtime role and assembles the server: pgx pool
// (refused for a superuser or BYPASSRLS role), Python worker client, production
// ports and the verified 15-tool registry. Errors never carry credentials.
func openRuntime(ctx context.Context, cfg config.MCP, get func(string) string) (rt *runtime, err error) {
	dsn := cfg.DatabaseURL.Reveal()
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse DATABASE_URL: invalid connection string")
	}
	pcfg.MaxConns = dbMaxConns
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["lock_timeout"] = dbLockTimeout
	pcfg.ConnConfig.RuntimeParams["statement_timeout"] = dbStmtTimeout
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, errors.New("connect to database: " + redactErr(err))
	}
	defer func() {
		if err != nil {
			pool.Close()
		}
	}()
	if err := queue.CheckSchema(ctx, pool, config.RequiredGoSchemaRevision); err != nil {
		return nil, err
	}
	// RLS is inert for a superuser/BYPASSRLS role: always refuse (the API only
	// downgrades this with PG_RLS_REQUIRE_SAFE_ROLE=false; the MCP server never does).
	if _, err := queue.AssertSafeRole(ctx, pool, true); err != nil {
		return nil, err
	}
	py := pyworker.New(get("PY_WORKER_URL"), get("PY_WORKER_TOKEN"), nil)
	envelope, err := secrets.NewEnvelope(secrets.Config{
		Provider: strings.ToLower(strings.TrimSpace(get("SECRETS_PROVIDER"))), MasterKey: get("SECRETS_MASTER_KEY"),
		SecretKey: get("SECRET_KEY"), AppEnv: get("APP_ENV"),
		VaultAddr: get("VAULT_ADDR"), VaultToken: get("VAULT_TOKEN"), VaultTokenFile: get("VAULT_TOKEN_FILE"),
		VaultTransitKey: get("VAULT_TRANSIT_KEY"), VaultNamespace: get("VAULT_NAMESPACE"), VaultCACert: get("VAULT_CACERT"),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets envelope: %w", err)
	}
	verify := emailverify.NewLive(liveConfig(pool, get, py, secrets.NewResolver(envelope, secrets.NewPgStore(pool), get)))
	deps := mcp.NewProductionDeps(cfg, pool, py, verify)
	var reg mcp.Registry
	if err := func() (e error) {
		defer func() {
			if r := recover(); r != nil {
				e = fmt.Errorf("%v", r)
			}
		}()
		reg = mcp.BuildRegistry(deps)
		return nil
	}(); err != nil {
		return nil, err
	}
	return &runtime{deps: deps, srv: mcp.NewServer(deps, reg), close: pool.Close}, nil
}

// secretGetter resolves one credential or setting (secrets.Resolver.Get):
// workspace, then global (app_settings), then env. ws == "" skips the workspace level.
type secretGetter interface {
	Get(ctx context.Context, ws, key string) (value, source string, err error)
}

// liveConfig is the email cascade wiring. API keys and REACHER_ENABLED resolve
// per workspace through sec; REACHER_URL and REACHER_TIMEOUT are global-only (a
// tenant must not be able to aim the verifier at an internal host). A lookup or
// decrypt failure is an error (Keys) or leaves Reacher disabled (settings): there
// is no fallback to the environment on failure. The HTTP client is the operator
// egress client (EGRESS_ALLOW_HOSTS exact host[:port] entries skip the
// private-IP block; everything else is vetted).
func liveConfig(pool *pgxpool.Pool, get func(string) string, py *pyworker.Client, sec secretGetter) emailverify.LiveConfig {
	var allow []string
	for _, h := range strings.Split(get("EGRESS_ALLOW_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
	}
	return emailverify.LiveConfig{
		Pool:        pool,
		AppEnv:      get("APP_ENV"),
		EgressAllow: allow,
		HTTP:        egress.NewOperatorClient(allow),
		Py:          py,
		Keys: func(ctx context.Context, ws, envVar string) (string, error) {
			v, _, err := sec.Get(ctx, ws, envVar)
			if err != nil {
				return "", err
			}
			return v, nil
		},
		ReacherSettings: func(ctx context.Context, ws string) map[string]any {
			out := map[string]any{}
			for _, k := range []string{"REACHER_URL", "REACHER_TIMEOUT"} {
				v, _, err := sec.Get(ctx, "", k)
				if err != nil {
					return map[string]any{} // fail closed: Reacher stays off
				}
				if v != "" {
					out[k] = v
				}
			}
			v, _, err := sec.Get(ctx, ws, "REACHER_ENABLED")
			if err != nil {
				return map[string]any{}
			}
			if v != "" {
				out["REACHER_ENABLED"] = v
			}
			return out
		},
		SMTPDisabled: get("LEADGEN_SMTP_VERIFY") == "0",
		HeloHost:     get("LEADGEN_SMTP_HELO"),
		MailFrom:     get("LEADGEN_SMTP_MAIL_FROM"),
	}
}

// redactErr keeps connection details (host, user, password) out of messages.
func redactErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return "connection failed"
}

// stopReader reads r in the background and turns into EOF when stop closes, so
// a blocked stdin read cannot hold up a graceful shutdown: the in-flight call
// finishes, then the transport sees end of input.
type stopReader struct {
	ch   chan chunk
	stop <-chan struct{}
	rest []byte
	err  error
}

type chunk struct {
	b   []byte
	err error
}

func newStopReader(r io.Reader, stop <-chan struct{}) *stopReader {
	s := &stopReader{ch: make(chan chunk), stop: stop}
	go func() {
		for {
			buf := make([]byte, 32<<10)
			n, err := r.Read(buf)
			select {
			case s.ch <- chunk{buf[:n], err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *stopReader) Read(p []byte) (int, error) {
	if len(s.rest) > 0 {
		n := copy(p, s.rest)
		s.rest = s.rest[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	select {
	case c := <-s.ch:
		s.err = c.err
		n := copy(p, c.b)
		s.rest = c.b[n:]
		if n == 0 && c.err != nil {
			return 0, c.err
		}
		return n, nil
	case <-s.stop:
		return 0, io.EOF
	}
}

// Transport starters; tests may replace them.
var (
	startStdio = runStdio
	startSSE   = runSSE
)

func runStdio(ctx context.Context, cfg config.MCP, get func(string) string, stdout, stderr io.Writer) error {
	if cfg.RequireAuth && cfg.Token.Reveal() == "" {
		fmt.Fprintln(stderr, "MCP stdio refused: missing MCP token. Set OPENGTM_MCP_TOKEN to a valid token.")
		return errors.New("missing MCP token")
	}
	rt, err := openRuntime(ctx, cfg, get)
	if err != nil {
		return err
	}
	defer rt.close()
	slog.Info("mcp stdio server started")
	// The serve context is NOT the signal context: a signal must not cancel the
	// in-flight call. It only ends the input (stopReader), and ServeStdio returns
	// once the call in progress has been answered.
	stop := make(chan struct{})
	go func() { <-ctx.Done(); close(stop) }()
	return mcp.ServeStdio(context.Background(), rt.srv, rt.deps, newStopReader(os.Stdin, stop), stdout, cfg.Token.Reveal())
}

func runSSE(ctx context.Context, cfg config.MCP, get func(string) string, host string, port int, _ io.Writer) error {
	rt, err := openRuntime(ctx, cfg, get)
	if err != nil {
		return err
	}
	defer rt.close()
	slog.Info("mcp sse server starting", "host", host, "port", port)
	return mcp.ListenSSE(ctx, mcp.SSEHandler(rt.srv, rt.deps), host, port)
}

// run is main without os.Exit; it returns the process exit code.
func run(args []string, get func(string) string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadMCPFrom(get)
	if err != nil {
		fmt.Fprintln(stderr, "mcp: config:", err)
		return 1
	}
	if err := config.CheckSecretKey(get("SECRET_KEY"), get("APP_ENV")); err != nil {
		fmt.Fprintln(stderr, "mcp:", err)
		return 1
	}
	sse, port, err := parseTransport(args)
	if err != nil {
		fmt.Fprintln(stderr, "mcp:", err)
		return 1
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() { <-ctx.Done(); stop() }() // a second signal takes the default action
	if sse {
		if !loopbackHost(cfg.SSEHost) && !cfg.SSEAllowNonLoopback {
			fmt.Fprintf(stderr, "mcp: refusing to bind SSE to non-loopback host %q; set OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK=true to override\n", cfg.SSEHost)
			return 1
		}
		if cfg.SSEAllowNonLoopback && !cfg.RequireAuth {
			fmt.Fprintln(stderr, "mcp: refusing OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK=true with MCP_REQUIRE_AUTH=false: a keyless server must stay on loopback")
			return 1
		}
		err = startSSE(ctx, cfg, get, cfg.SSEHost, port, stderr)
	} else {
		err = startStdio(ctx, cfg, get, stdout, stderr)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "mcp:", err)
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)) }
