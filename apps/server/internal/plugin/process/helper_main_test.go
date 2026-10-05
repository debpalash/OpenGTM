//go:build unix

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"os"
	"testing"
	"time"
)

// The test binary doubles as a stand-in worker process: tests re-execute it
// with OPENGTM_PROCESS_TEST_HELPER set. A re-executed process has the planted
// secrets in its *initial* environment, which is what /proc/<pid>/environ
// exposes (os.Setenv in a running process does not change it), so it models a
// real worker faithfully; and it can be SIGKILLed to test crash recovery.

const helperEnv = "OPENGTM_PROCESS_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "scan":
		helperScan()
	case "serve":
		helperServe()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

func helperSupervisor(extra func(*Options)) *Supervisor {
	opts := Options{
		StateDir:           os.Getenv("HELPER_STATE"),
		PythonPath:         []string{os.Getenv("HELPER_SDK")},
		KeepParentDumpable: os.Getenv("HELPER_KEEP_DUMPABLE") == "1",
		DisablePdeathsig:   os.Getenv("HELPER_NO_PDEATHSIG") == "1",
		Logger:             discardLog{},
	}
	if extra != nil {
		extra(&opts)
	}
	s, err := NewSupervisor(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	return s
}

type discardLog struct{}

func (discardLog) Info(string, ...any)  {}
func (discardLog) Warn(string, ...any)  {}
func (discardLog) Error(string, ...any) {}
func (discardLog) Debug(string, ...any) {}

// helperScan runs a plugin that hunts for HELPER_NEEDLE (planted in this
// process's initial environment) and prints what it found as JSON.
func helperScan() {
	s := helperSupervisor(nil)
	defer s.Close()
	p := pspec{}.buildForHelper()
	out, err := s.Run(context.Background(), p, Request{Inputs: map[string]any{"mode": "scan_fs", "needle": os.Getenv("HELPER_NEEDLE")}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(4)
	}
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "found": out.Records[0].Fields["found"]})
	fmt.Println(string(b))
	os.Exit(0)
}

// helperServe runs a plugin that spawns a process tree and then blocks until
// it is killed, printing READY once the tree exists.
func helperServe() {
	s := helperSupervisor(nil)
	p := pspec{timeout: 120}.buildForHelper()
	go func() {
		_, _ = s.Run(context.Background(), p, Request{Inputs: map[string]any{"mode": "tree", "pidfile": os.Getenv("HELPER_PIDFILE")}})
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(os.Getenv("HELPER_PIDFILE")); err == nil && json.Valid(b) {
			fmt.Println("READY", os.Getpid())
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "tree never started")
			os.Exit(5)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {} // wait to be SIGKILLed
}

// buildForHelper is pspec.build without a *testing.T.
func (s pspec) buildForHelper() *manifest.Plugin {
	t := &fatalT{}
	return s.build(t)
}

type fatalT struct{ testing.TB }

func (*fatalT) Helper() {}
func (*fatalT) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(6)
}
