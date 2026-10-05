package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type pluginCommand struct {
	name, args, summary string
	run                 func(ctx context.Context, args []string) error
}

var pluginCommands []pluginCommand

func init() {
	pluginCommands = []pluginCommand{
		{"new", "<kind> <name> [--runtime declarative|wasm] [--dir path]", "scaffold a plugin with fixtures and a test case", pluginNew},
		{"validate", "<paths...> [--json] [--signature-policy optional|required] [--trust-store file]", "validate manifests (v1 connector directories or v2 plugins)", pluginValidate},
		{"test", "<path> [--case name] [--json]", "run the plugin offline against fixtures/<case>/case.yaml", pluginTest},
		{"record", "<path> --input k=v ... [--case name] [--secret-env NAME] [--overwrite]", "run live and save a new fixture case", pluginRecord},
		{"run", "<path> --input k=v ... [--secret-env NAME]", "run live and print the JSON result with evidence", pluginRun},
		{"pack", "<path> [--output file.ogc]", "build a deterministic signed .ogc bundle", pluginPack},
		{"sign", "<path> --private-key key.pem --key-id id", "write the detached Ed25519 signature (<manifest>.sig)", pluginSign},
		{"verify", "<path> [--trust-store file] [--signature-policy ...]", "verify a manifest signature against the trust store", pluginVerify},
		{"keygen", "--key-id id [--out key.pem] [--publisher name]", "create an Ed25519 publisher key and print its trust entry", pluginKeygen},
		{"dev", "<path> [--interval 1s]", "watch the plugin and rerun its tests on every change", pluginDev},
	}
	register("plugin", "build, test, sign and run plugins", runPlugin)
}

func pluginUsage() {
	fmt.Fprintln(stderr, "usage: opengtm plugin <command> [arguments]\n\ncommands:")
	for _, c := range pluginCommands {
		fmt.Fprintf(stderr, "  %-9s %s\n            %s\n", c.name, c.summary, c.args)
	}
}

func runPlugin(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		pluginUsage()
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	for _, c := range pluginCommands {
		if c.name == args[0] {
			err := c.run(ctx, args[1:])
			if errors.Is(err, errUsage) {
				fmt.Fprintf(stderr, "usage: opengtm plugin %s %s\n", c.name, c.args)
			}
			return err
		}
	}
	pluginUsage()
	return fmt.Errorf("unknown plugin command %q (want one of: %s)", args[0], commandNames())
}

func commandNames() string {
	names := make([]string, len(pluginCommands))
	for i, c := range pluginCommands {
		names[i] = c.name
	}
	return strings.Join(names, ", ")
}
