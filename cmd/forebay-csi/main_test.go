package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServingNeitherHalfIsRefused(t *testing.T) {
	// A driver that opened a socket and answered nothing would be registered
	// with the orchestrator and fail every call, which looks like a broken
	// driver rather than one that was never asked to do anything.
	if _, err := reporter("", "", time.Second); err != nil {
		t.Fatalf("no agent should be no error: %v", err)
	}
}

func TestAnAgentWithoutATokenIsRefused(t *testing.T) {
	// The agent refuses a report without one, so a plugin started this way
	// would report on every mount and be turned away every time, and the
	// failure would only ever show up in the plugin's own log.
	_, err := reporter("http://127.0.0.1:9099", "", time.Second)
	if err == nil {
		t.Fatal("an agent address with no token was accepted")
	}
	if !strings.Contains(err.Error(), "--agent-token-file") {
		t.Errorf("the message does not say what to set: %v", err)
	}
}

func TestAnEmptyTokenFileIsRefused(t *testing.T) {
	// An empty file is an operator who meant to set one. Reporting with an
	// empty token would be refused by the agent for a reason that reads like
	// a permissions problem.
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter("http://127.0.0.1:9099", path, time.Second); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestAMissingTokenFileIsRefused(t *testing.T) {
	_, err := reporter("http://127.0.0.1:9099", filepath.Join(t.TempDir(), "absent"), time.Second)
	if err == nil {
		t.Error("a token file that is not there was accepted")
	}
}

func TestATokenIsReadWithoutItsTrailingNewline(t *testing.T) {
	// A file written by an operator or a secret mount ends in a newline, and
	// a token compared with one in it never matches.
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := reporter("http://127.0.0.1:9099", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if o == nil {
		t.Fatal("no observer was built")
	}
}

func TestTheStartupLineSaysWhichHalvesAreServed(t *testing.T) {
	// An operator reading one line should be able to tell a controller that
	// is not mounting from a node plugin that is not resolving.
	for _, c := range []struct {
		controller, node bool
		want             string
	}{
		{true, true, "both halves"},
		{true, false, "the controller half"},
		{false, true, "the node half"},
	} {
		if got := halves(c.controller, c.node); got != c.want {
			t.Errorf("controller=%v node=%v gave %q, want %q", c.controller, c.node, got, c.want)
		}
	}
}
