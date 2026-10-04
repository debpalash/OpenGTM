// Command opengtm is the single multi-role OpenGTM binary.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "opengtm: no subcommands yet")
	os.Exit(2)
}
