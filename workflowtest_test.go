package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// suiteDir is a workflow and its test suite in a fresh directory.
func suiteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"enrich.yaml":      applyDoc("enrich"),
		"enrich.test.yaml": "cases:\n  - name: a new product\n    input: { raw: x }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// report is the API's answer for one case, with the given verdict and rendered prompt.
func report(pass bool, prompt string) string {
	failed, passed, failures := 0, 1, `[]`
	if !pass {
		failed, passed, failures = 1, 0, `["only: output did not match"]`
	}
	return `{"passed":` + itoa(passed) + `,"failed":` + itoa(failed) + `,"cases":[{"name":"a new product","pass":` +
		boolStr(pass) + `,"failures":` + failures + `,"run":{"status":"succeeded","nodes":[{"key":"only","status":"succeeded","prompt":` +
		jsonStr(prompt) + `}]}}]}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// The command finds the suite by directory, sends the workflow beside it with the suite and the
// --run filter, and exits by the report.
func TestWorkflowTestCommandSendsTheSuiteAndExitsByTheReport(t *testing.T) {
	dir := suiteDir(t)
	api := newFakeAPI(t, `{}`)
	api.testReply = report(true, "[user] x")
	if _, err := capture(t, func() error {
		return workflowTest([]string{dir, "--run", "new", "--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("a passing report: %v", err)
	}
	var sent struct{ Document, Tests, Run string }
	if err := json.Unmarshal(api.testBody, &sent); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent.Document, "name: enrich") || !strings.Contains(sent.Tests, "a new product") || sent.Run != "new" {
		t.Errorf("sent = %+v", sent)
	}

	api.testReply = report(false, "[user] x")
	_, err := capture(t, func() error { return workflowTest([]string{dir, "--api", api.URL, "--key", "sk"}) })
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("a failing report = %v, want a failure", err)
	}
}

// --update writes a snapshot of every rendered prompt; a later prompt change fails --snapshots
// until it is accepted with --update.
func TestWorkflowTestCommandSnapshots(t *testing.T) {
	dir := suiteDir(t)
	api := newFakeAPI(t, `{}`)
	api.testReply = report(true, "[user] parts catalog for x")
	run := func(flag string) error {
		_, err := capture(t, func() error { return workflowTest([]string{dir, flag, "--api", api.URL, "--key", "sk"}) })
		return err
	}
	if err := run("--update"); err != nil {
		t.Fatalf("--update: %v", err)
	}
	snap := filepath.Join(dir, "__snapshots__", "enrich", "a-new-product", "only.txt")
	if b, err := os.ReadFile(snap); err != nil || string(b) != "[user] parts catalog for x" {
		t.Fatalf("snapshot %s = %q, %v", snap, b, err)
	}
	if err := run("--snapshots"); err != nil {
		t.Fatalf("an unchanged prompt failed its snapshot: %v", err)
	}
	api.testReply = report(true, "[user] catalogue for x")
	if err := run("--snapshots"); err == nil {
		t.Fatal("a changed prompt passed its snapshot")
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()
	if got := slug("Unparseable output retries, then falls back!"); got != "unparseable-output-retries-then-falls-back" {
		t.Errorf("slug = %q", got)
	}
}
