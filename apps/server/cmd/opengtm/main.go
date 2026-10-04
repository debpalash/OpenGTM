// Command opengtm is the single multi-role OpenGTM binary.
//
// Each subcommand lives in its own file and registers itself from init(), so
// roles (serve, worker, plugin, ...) can be added without touching this file.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string) error
}

var commands = map[string]command{}

func register(name, summary string, run func(ctx context.Context, args []string) error) {
	if _, dup := commands[name]; dup {
		panic("opengtm: duplicate command " + name)
	}
	commands[name] = command{summary: summary, run: run}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: opengtm <command> [flags]\n\ncommands:")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", name, commands[name].summary)
	}
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "opengtm: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.run(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "opengtm:", err)
		os.Exit(1)
	}
}
