package main

import (
	"context"
	"fmt"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	register("version", "print the build version", func(context.Context, []string) error {
		fmt.Println(version)
		return nil
	})
}
