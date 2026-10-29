package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// The credential in the environment must never be a flag's default: a mistyped flag prints the
// usage text, defaults included, into terminals, CI logs and tool transcripts. An old build
// defaulted --key to OTIUM_API_KEY and printed it that way.
func TestAPIFlags_UsageNeverPrintsTheEnvToken(t *testing.T) {
	const secret = "sk-otium-do-not-print"
	t.Setenv("OTIUM_TOKEN", secret)
	t.Setenv("OTIUM_API_KEY", secret)

	fs := flag.NewFlagSet("otiumctl", flag.ContinueOnError)
	var usage bytes.Buffer
	fs.SetOutput(&usage)
	apiFlags(fs)
	if err := fs.Parse([]string{"--no-such-flag"}); err == nil {
		t.Fatal("an unknown flag parsed")
	}
	if !strings.Contains(usage.String(), "-key") {
		t.Fatalf("the usage text was not printed:\n%s", usage.String())
	}
	if strings.Contains(usage.String(), secret) {
		t.Errorf("usage text prints the token:\n%s", usage.String())
	}
}

// A release build reports the tag GoReleaser stamped; a build without one says what Go knows.
func TestVersionString(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })
	version = "v1.2.3"
	if got := versionString(); got != "v1.2.3" {
		t.Errorf("versionString() = %q, want the stamped tag", got)
	}
	version = ""
	if got := versionString(); got == "" {
		t.Error("an unstamped build reports nothing")
	}
}
