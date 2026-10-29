package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// applyDoc is a document that compiles cleanly. `name` is templated so a directory can hold
// several distinct workflows.
func applyDoc(name string) string {
	return `
name: ` + name + `
input:
  schema:
    type: object
    properties:
      raw: { type: string }
nodes:
  only:
    target: { product: otium-small }
    body:
      messages:
        - { role: user, content: "${input.raw}" }
    output:
      validators:
        - { type: json_schema, schema: { type: object, required: [title], properties: { title: { type: string } } } }
`
}

// writeDir builds a workflow directory from name → contents.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// A dry run is the `terraform plan` half of apply, and the property that makes it useful in
// a PR check is that it WRITES NOTHING — a plan that published would be a plan nobody could
// run on an untrusted branch.
func TestADryRunReportsWhatWouldChangeAndTouchesTheServer(t *testing.T) {
	api := newFakeAPI(t, `{}`)
	dir := writeDir(t, map[string]string{"enrich.yaml": applyDoc("enrich")})

	out, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--dry-run", "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	if api.method != "" {
		t.Errorf("a dry run made a %s request to %s", api.method, api.path)
	}
	if !strings.Contains(out, "would publish") || !strings.Contains(out, "enrich") {
		t.Errorf("output = %q", out)
	}
	// The plan headline is what makes the diff reviewable: node count, p95, per-item cost.
	if !strings.Contains(out, "nodes") || !strings.Contains(out, "/item") {
		t.Errorf("no plan headline in the dry run: %q", out)
	}
}

// The apply itself distinguishes "minted a new version" from "the digest was already
// live". A CI job runs on every commit, so "unchanged" is the NORMAL outcome and reporting
// it as a change would make the log useless.
func TestApplyDistinguishesAPublishFromANoOp(t *testing.T) {
	api := newFakeAPI(t, `{"ok":true,"created":true,"version":{"name":"enrich","version":4,"digest":"abc"}}`)
	api.status = http.StatusCreated
	dir := writeDir(t, map[string]string{"enrich.yaml": applyDoc("enrich")})

	out, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/workflows" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	// The document goes up verbatim, in the envelope the portal's editor already posts, so
	// one path through the server serves both.
	var sent documentEnvelope
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if !strings.Contains(sent.Document, "name: enrich") {
		t.Errorf("the document was not sent verbatim: %q", sent.Document)
	}
	if !strings.Contains(out, "published enrich") || !strings.Contains(out, "v4") {
		t.Errorf("output = %q", out)
	}

	// The second run of the same CI job over an unchanged tree.
	api.status = http.StatusOK
	api.reply = `{"ok":true,"created":false,"version":{"name":"enrich","version":4,"digest":"abc"}}`
	out, err = capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("a re-apply of an unchanged document did not say so: %q", out)
	}
	// The per-document line, not the summary tally (which legitimately reads "0
	// published"): a no-op must not be listed as a change.
	if strings.Contains(out, "published enrich") {
		t.Errorf("a no-op was reported as a publish: %q", out)
	}
}

// A directory with one broken document must name WHICH one and exit non-zero, and it must
// not have published the others first, which is why every document is validated before any
// publish goes out.
func TestOneBrokenDocumentIsNamedAndNothingIsPublished(t *testing.T) {
	api := newFakeAPI(t, `{"ok":true,"created":true,"version":{"name":"good","version":1}}`)
	api.validateReply = `{"ok":false,"name":"broken","diagnostics":[{"severity":"error","code":"unknown_node","message":"nodes.missing does not exist","node":"a","path":"body"}]}`
	dir := writeDir(t, map[string]string{
		"broken.yaml": "name: broken\nnodes:\n  a:\n    body: {messages: [{role: user, content: \"${nodes.missing.output.x}\"}]}\n",
	})

	out, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--api", api.URL, "--key", "sk"})
	})
	if err == nil {
		t.Fatal("a directory with a broken document exited zero")
	}
	if api.method != "" {
		t.Errorf("a rejected document was still published: %s %s", api.method, api.path)
	}
	if !strings.Contains(out, "REJECTED") || !strings.Contains(out, "broken") {
		t.Errorf("the broken document was not named: %q", out)
	}
	// The finding, not just "invalid": every rejection here otherwise costs a round trip
	// to work out.
	if !strings.Contains(strings.ToLower(out), "missing") && !strings.Contains(out, "[") {
		t.Errorf("the rejection carries no finding: %q", out)
	}
}

// --max-critical-path is the assertion a customer's CI wants to make: a graph whose
// critical path no longer fits their window is a deadline miss discovered at submit, on
// every item, at scale. Catching it in the PR is the entire point.
func TestTheCriticalPathAssertionFailsBeforeTheChangeShips(t *testing.T) {
	api := newFakeAPI(t, `{"ok":true,"created":true,"version":{"name":"enrich","version":1}}`)
	dir := writeDir(t, map[string]string{"enrich.yaml": applyDoc("enrich")})

	// One nanosecond: any real plan exceeds it, so this asserts the CHECK rather than a
	// particular estimate — the estimate is the compiler's business and is tested there.
	out, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--max-critical-path", "1ns",
			"--api", api.URL, "--key", "sk"})
	})
	if err == nil {
		t.Fatal("a graph over the budget was applied")
	}
	if api.method != "" {
		t.Errorf("a rejected graph was still published: %s", api.path)
	}
	if !strings.Contains(out, "critical path") {
		t.Errorf("the rejection does not name the reason: %q", out)
	}

	// A generous budget passes, so the flag is a real comparison and not an always-fail.
	if _, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--max-critical-path", "72h",
			"--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Errorf("a graph inside the budget was rejected: %v", err)
	}
}

func TestApplyResolvesADirectoryOrASingleFile(t *testing.T) {
	api := newFakeAPI(t, `{"ok":true,"created":false,"version":{"name":"x","version":1}}`)
	dir := writeDir(t, map[string]string{
		"a.yaml":     applyDoc("alpha"),
		"b.yml":      applyDoc("bravo"),
		"README.md":  "not a workflow",
		"notes.json": "{}",
	})
	if err := os.Mkdir(filepath.Join(dir, "fixtures"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "c.yaml"), []byte(applyDoc("charlie")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--dry-run", "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Both YAML extensions, and nothing else — a README or a fixtures/ folder swept into a
	// publish is how a customer's test data becomes a live workflow.
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "bravo") {
		t.Errorf("output = %q", out)
	}
	if strings.Contains(out, "charlie") {
		t.Errorf("the walk recursed into a subdirectory: %q", out)
	}
	if !strings.Contains(out, "2 document(s)") {
		t.Errorf("count = %q", out)
	}

	// A single file works too, so the verb is usable before there is a directory.
	if _, err := capture(t, func() error {
		return workflowApply([]string{"-f", filepath.Join(dir, "a.yaml"), "--dry-run",
			"--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("apply a single file: %v", err)
	}
}

func TestApplyRefusesWithoutATarget(t *testing.T) {
	api := newFakeAPI(t, `{}`)
	if _, err := capture(t, func() error {
		return workflowApply([]string{"--api", api.URL, "--key", "sk"})
	}); err == nil {
		t.Error("apply with no -f was accepted")
	}
	if _, err := capture(t, func() error {
		return workflowApply([]string{"-f", filepath.Join(t.TempDir(), "nope"), "--api", api.URL, "--key", "sk"})
	}); err == nil {
		t.Error("apply against a missing path was accepted")
	}
	// An empty directory is an error rather than a silent success: a CI step whose glob
	// stopped matching should fail loudly, not report "applied 0 documents".
	if _, err := capture(t, func() error {
		return workflowApply([]string{"-f", t.TempDir(), "--api", api.URL, "--key", "sk"})
	}); err == nil {
		t.Error("an empty directory was reported as a successful apply")
	}
}

// --json is what a pipeline reads, so its shape is part of the contract.
func TestApplyEmitsMachineReadableOutcomes(t *testing.T) {
	api := newFakeAPI(t, `{"ok":true,"created":true,"version":{"name":"enrich","version":7,"digest":"d7"}}`)
	dir := writeDir(t, map[string]string{"enrich.yaml": applyDoc("enrich")})

	out, err := capture(t, func() error {
		return workflowApply([]string{"-f", dir, "--json", "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var outcomes []applyOutcome
	if err := json.Unmarshal([]byte(out), &outcomes); err != nil {
		t.Fatalf("--json is not JSON: %v (%s)", err, out)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d", len(outcomes))
	}
	got := outcomes[0]
	if got.Action != "published" || got.Name != "enrich" || got.Version != 7 {
		t.Errorf("outcome = %+v", got)
	}
	if got.Path == "" {
		t.Error("the outcome does not say which file it came from")
	}
}
