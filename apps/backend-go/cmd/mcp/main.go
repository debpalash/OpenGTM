// Command mcp is the OpenGTM MCP server: stdio by default, `--sse [port]` for
// the loopback HTTP transport. stdout carries protocol bytes only; every log
// and diagnostic goes to stderr.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
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

// Transport starters are wired by plan 13 Tasks 15-17; nil means not built yet.
var (
	startStdio func(cfg config.MCP, stdout, stderr io.Writer) error
	startSSE   func(cfg config.MCP, host string, port int, stderr io.Writer) error
)

// run is main without os.Exit; it returns the process exit code.
func run(args []string, get func(string) string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadMCPFrom(get)
	if err != nil {
		fmt.Fprintln(stderr, "mcp: config:", err)
		return 1
	}
	sse, port, err := parseTransport(args)
	if err != nil {
		fmt.Fprintln(stderr, "mcp:", err)
		return 1
	}
	if sse {
		if !loopbackHost(cfg.SSEHost) && !cfg.SSEAllowNonLoopback {
			fmt.Fprintf(stderr, "mcp: refusing to bind SSE to non-loopback host %q; set OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK=true to override\n", cfg.SSEHost)
			return 1
		}
		if startSSE == nil {
			fmt.Fprintln(stderr, "mcp: SSE transport not available in this build")
			return 1
		}
		err = startSSE(cfg, cfg.SSEHost, port, stderr)
	} else {
		if startStdio == nil {
			fmt.Fprintln(stderr, "mcp: stdio transport not available in this build")
			return 1
		}
		err = startStdio(cfg, stdout, stderr)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "mcp:", err)
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr)) }
