package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The run view has to show BOTH clocks. "7 days of execution time — the clock stops while
// a run is waiting on you" is only a checkable promise if a customer can see the two
// numbers separately, and a CLI that collapses them into one is where that promise starts
// being unverifiable.
func TestRunGetShowsBothClocksAndPointsAtARejectedOutput(t *testing.T) {
	api := newFakeAPI(t, `{
		"id":"run-1","run_set_id":"rs-1","custom_id":"sku-9","status":"partial",
		"outcome":"node_failed","budget_consumed_seconds":7200,"parked_seconds":432000,
		"cost_usd":0.0125,
		"nodes":[
			{"node_key":"extract","status":"succeeded","disposition":"ok","inherited_from_run_id":"run-0"},
			{"node_key":"classify","status":"failed","terminal_reason":"invalid_output",
			 "disposition":"degraded","rejected_output":{"nam":"x"}}
		]}`)

	out, err := capture(t, func() error {
		return runGet([]string{"run-1", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run get: %v", err)
	}
	if api.path != "/v1/runs/run-1" {
		t.Errorf("path = %s", api.path)
	}
	if !strings.Contains(out, "window used 2.0h") || !strings.Contains(out, "parked 5.0d") {
		t.Errorf("both clocks are not visible: %q", out)
	}
	// The carried node says so: a reader working out what this run cost needs to know it
	// bought nothing for that node.
	if !strings.Contains(out, "inherited from run-0") {
		t.Errorf("provenance missing: %q", out)
	}
	// A rejected completion is pointed at, not printed: a model output in the middle of a
	// status table buries the rest of it.
	if !strings.Contains(out, "run attempts run-1 classify") {
		t.Errorf("no pointer to the attempts verb: %q", out)
	}
	if strings.Contains(out, `"nam"`) {
		t.Errorf("the rejected body was dumped into the status table: %q", out)
	}
}

// The verb for "why did my validator reject this?", which is the question a status alone
// cannot answer and the likeliest reason anyone reaches for this CLI.
func TestRunAttemptsShowsWhatRanWhatItSaidAndWhyItWasRefused(t *testing.T) {
	api := newFakeAPI(t, `{
		"run_id":"run-1","node_key":"classify","status":"failed","cost_usd":0.5,
		"attempts":[
			{"attempt":1,"policy_step":0,"status":"failed","terminal_reason":"invalid_output",
			 "identity":{"model":"qwen3-8b","model_revision":"abc123","region":"us-east-1"},
			 "output":"{\"nam\":\"x\"}","output_tokens":40,"cost_usd":0.25,
			 "verdicts":[{"ok":false,"code":"missing_field","pointer":"/name","expected":"string"}]},
			{"attempt":2,"policy_step":1,"status":"succeeded","terminal_reason":"ok",
			 "identity":{"model":"qwen3-32b"},"from_run_id":"run-0",
			 "output":"{\"name\":\"x\"}","output_tokens":41,"cost_usd":0.25,"verdicts":[]}
		]}`)

	out, err := capture(t, func() error {
		return runAttempts([]string{"run-1", "classify", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run attempts: %v", err)
	}
	if api.path != "/v1/runs/run-1/nodes/classify/attempts" {
		t.Errorf("path = %s", api.path)
	}
	// A verdict names a location and a constraint and never a value, so printing
	// it verbatim is safe — and it is the whole answer the customer came for.
	if !strings.Contains(out, "missing_field at /name (expected string)") {
		t.Errorf("verdict not rendered: %q", out)
	}
	// Both attempts' bodies, because a customer working out why attempt 2 passed cannot do
	// it with half the evidence.
	if !strings.Contains(out, `nam`) || !strings.Contains(out, `name`) {
		t.Errorf("outputs missing: %q", out)
	}
	// The identity is what makes "why did the fallback pass?" answerable.
	if !strings.Contains(out, "qwen3-8b@abc123 in us-east-1") || !strings.Contains(out, "qwen3-32b") {
		t.Errorf("identity missing: %q", out)
	}
	if !strings.Contains(out, "produced by run run-0") {
		t.Errorf("a carried attempt does not say where it came from: %q", out)
	}
}

// Truncation is a scan aid, not a limit: one 4,000-token completion would bury the five
// lines around it, and --full is there for when the body IS the thing being read.
func TestAttemptOutputIsPreviewedUnlessFullIsAsked(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 400)
	if got := previewOutput(long, false); !strings.HasSuffix(got, "(--full for all of it)") {
		t.Errorf("long output was not truncated: %d chars", len(got))
	}
	if got := previewOutput(long, true); len(got) < 400 {
		t.Errorf("--full truncated anyway: %d chars", len(got))
	}
	// A multi-line body collapses to its first line, so the table stays a table.
	if got := previewOutput("first\nsecond", false); strings.Contains(got, "second") {
		t.Errorf("preview kept the second line: %q", got)
	}
}

func TestRunSetOutputStreamsToAFile(t *testing.T) {
	api := newFakeAPI(t, "{\"custom_id\":\"sku-1\"}\n{\"custom_id\":\"sku-2\"}\n")
	path := filepath.Join(t.TempDir(), "out.jsonl")

	if _, err := capture(t, func() error {
		return runSetOutput([]string{"rs-1", "--out", path, "--api", api.URL, "--key", "sk-test"})
	}); err != nil {
		t.Fatalf("run-set output: %v", err)
	}
	if api.path != "/v1/run-sets/rs-1/output" {
		t.Errorf("path = %s", api.path)
	}
	body, err := os.ReadFile(path) //nolint:gosec // a path this test just made
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	// Written through unchanged: this is the only way to read a CONTINUATION's results,
	// and a CLI that reformatted them would make the file a customer downloads from a
	// batch and the file they get here disagree.
	if string(body) != "{\"custom_id\":\"sku-1\"}\n{\"custom_id\":\"sku-2\"}\n" {
		t.Errorf("output = %q", body)
	}
}

func TestContinueReportsWhatIsCarriedAndWhatIsReboughtAndIsIdempotent(t *testing.T) {
	// The API's answer, as it sends it (the RunSetContinuation schema).
	api := newFakeAPI(t, `{"run_set_id":"rs-2","source_run_set_id":"rs-1","existing":false,"runs":40,"inherited_nodes":120,`+
		`"rerunning_nodes":40,"skipped_runs":0,"held_runs":15,"rerun":[{"node_key":"classify","reason":"invalid_output","runs":40}]}`)

	out, err := capture(t, func() error {
		return runContinue("/v1/run-sets/", []string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/run-sets/rs-1/continue" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	// What was bought and what will be bought again — the number a customer needs before
	// they press it, and the reason the set-level verb exists at all.
	if !strings.Contains(out, "continuing as rs-2") ||
		!strings.Contains(out, "120 node(s) carried forward, 40 to re-run") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "classify") || !strings.Contains(out, "invalid_output") {
		t.Errorf("per-node reasons missing: %q", out)
	}
	if !strings.Contains(out, "15 run(s) held until your balance or monthly spend cap covers them") {
		t.Errorf("held runs missing: %q", out)
	}

	// A retried call recovers the continuation it already made rather than buying the
	// graph a second time, and the CLI has to say so or the operator presses it again.
	api.reply = `{"run_set_id":"rs-2","existing":true,"runs":40,"inherited_nodes":120,"rerunning_nodes":40,` +
		`"skipped_runs":0,"held_runs":0,"rerun":[]}`
	out, err = capture(t, func() error {
		return runContinue("/v1/run-sets/", []string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !strings.Contains(out, "already continued as rs-2") {
		t.Errorf("a replay did not say so: %q", out)
	}
}

// "Nothing to continue" and "still running" are answers, not malfunctions, and the CLI has
// to say which without the operator reading a stack of HTTP status codes.
func TestAContinueRefusalIsReportedAsARefusal(t *testing.T) {
	api := newFakeAPI(t, `{"error":"nothing to continue: every run already finished"}`)
	api.status = http.StatusConflict

	_, err := capture(t, func() error {
		return runContinue("/v1/run-sets/", []string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	})
	if err == nil {
		t.Fatal("continue succeeded against a 409")
	}
	if !strings.Contains(err.Error(), "refused: nothing to continue") {
		t.Errorf("error = %q", err)
	}
}

func TestHumanSeconds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{45, "45s"}, {90, "2m"}, {7200, "2.0h"}, {432000, "5.0d"},
	} {
		if got := humanSeconds(tc.in); got != tc.want {
			t.Errorf("humanSeconds(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The migration lever as a file. It is a separate verb from `output` because the two are
// different formats for different readers, and this one goes to a collect job that was
// written against OpenAI long before Otium existed.
func TestNodeOutputDownloadsTheStockBatchFormat(t *testing.T) {
	api := newFakeAPI(t, `{"custom_id":"sku-1","response":{"status_code":200,"body":{"choices":[]}}}`+"\n")
	path := filepath.Join(t.TempDir(), "extract.jsonl")

	if _, err := capture(t, func() error {
		return runSetNodeOutput([]string{"rs-1", "extract", "--out", path, "--api", api.URL, "--key", "sk-test"})
	}); err != nil {
		t.Fatalf("node-output: %v", err)
	}
	if api.path != "/v1/run-sets/rs-1/nodes/extract/output.jsonl" {
		t.Errorf("path = %s", api.path)
	}
	body, err := os.ReadFile(path) //nolint:gosec // a path this test just made
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Byte-for-byte: a CLI that reformatted these lines would make the file a customer
	// downloads here and the one they get from the API disagree, in the format whose whole
	// value is that their existing parser already handles it.
	if string(body) != api.reply {
		t.Errorf("file = %q, want the response verbatim", body)
	}

	// Both ids are required. Guessing a node key is not a convenience.
	if _, err := capture(t, func() error {
		return runSetNodeOutput([]string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	}); err == nil {
		t.Error("node-output without a node key was accepted")
	}
}

// The 2026-10-13 batch read `running` for 6.5 hours behind the gather gate and then ran for a
// minute. The views say the work is held and until when, and keep the window's clock apart
// from the time on workers, so neither reads as a stuck or slow run.
func TestRunViewsShowHoldsAndTimeOnWorkers(t *testing.T) {
	run := newFakeAPI(t, `{
		"id":"run-1","run_set_id":"rs-1","custom_id":"sku-1","status":"running",
		"budget_consumed_seconds":23400,"parked_seconds":0,"execution_seconds":60.5,"cost_usd":0,
		"nodes":[{"node_key":"search","status":"ready","disposition":"ok","execution_seconds":0,
		  "hold":{"reason":"gathering","latest_start":"2026-10-13T22:00:00Z"}}]}`)
	out, err := capture(t, func() error {
		return runGet([]string{"run-1", "--api", run.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run get: %v", err)
	}
	if !strings.Contains(out, "window used 6.5h, on workers 1m") {
		t.Errorf("the window's clock and the time on workers are not both shown: %q", out)
	}
	if !strings.Contains(out, "held: gathering work to share one GPU boot, starts by 2026-10-13T22:00:00Z") {
		t.Errorf("the held node does not say why or until when: %q", out)
	}

	set := newFakeAPI(t, `{
		"id":"rs-1","workflow_id":"wf","status":"running","execution_budget_seconds":86400,
		"expires_at":"2026-10-20T00:00:00Z","total":5,"completed":0,"failed":0,
		"counts":{"running":5},
		"holds":[{"node_key":"search","reason":"price","latest_start":"2026-10-13T22:00:00Z","runs":5}]}`)
	out, err = capture(t, func() error {
		return runSetGet([]string{"rs-1", "--api", set.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run-set get: %v", err)
	}
	if !strings.Contains(out, "search: 5 run(s) held: waiting for a cheaper GPU, starts by 2026-10-13T22:00:00Z") {
		t.Errorf("the set does not list its held work: %q", out)
	}

	attempts := newFakeAPI(t, `{
		"run_id":"run-1","node_key":"search","status":"succeeded","cost_usd":0,
		"attempts":[
		  {"attempt":1,"policy_step":0,"status":"succeeded","identity":{"model":"m"},"verdicts":[],
		   "started_at":"2026-10-13T15:00:00Z","leased_at":"2026-10-13T21:30:00Z","execution_seconds":40.5},
		  {"attempt":2,"policy_step":0,"status":"running","identity":{"model":"m"},"verdicts":[],
		   "started_at":"2026-10-13T21:31:00Z","execution_seconds":0}]}`)
	out, err = capture(t, func() error {
		return runAttempts([]string{"run-1", "search", "--api", attempts.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run attempts: %v", err)
	}
	if !strings.Contains(out, "queued 2026-10-13T15:00:00Z, leased 2026-10-13T21:30:00Z, ran 41s") {
		t.Errorf("the attempt does not separate the queue from the run: %q", out)
	}
	if !strings.Contains(out, "queued 2026-10-13T21:31:00Z, waiting for a worker") {
		t.Errorf("an unleased attempt does not say it is waiting: %q", out)
	}
}

// --json prints what the server sent, so a field this build does not know is not dropped on
// the way to a script.
func TestRunViewJSONKeepsFieldsTheCLIDoesNotKnow(t *testing.T) {
	api := newFakeAPI(t, `{"id":"run-1","status":"running","nodes":[],"a_future_field":{"x":1}}`)
	out, err := capture(t, func() error {
		return runGet([]string{"run-1", "--json", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run get --json: %v", err)
	}
	if !strings.Contains(out, `"a_future_field"`) {
		t.Errorf("--json dropped a field: %q", out)
	}
}

// The set run reads like any run once it exists, and before then says where the items are.
func TestRunSetSetRun(t *testing.T) {
	api := newFakeAPI(t, `{"id":"run-set","run_set_id":"rs-1","custom_id":"$set","status":"running",
		"nodes":[{"node_key":"brief","status":"ready","disposition":"ok"}]}`)
	out, err := capture(t, func() error {
		return runSetSetRun([]string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run-set set-run: %v", err)
	}
	if api.path != "/v1/run-sets/rs-1/set" {
		t.Errorf("path = %s", api.path)
	}
	if !strings.Contains(out, "run-set  running") || !strings.Contains(out, "custom_id=$set") || !strings.Contains(out, "brief") {
		t.Errorf("the set run is not shown as a run: %q", out)
	}

	pending := newFakeAPI(t, `{"status":"pending","items":{"total":3,"completed":1,"failed":1}}`)
	pending.status = http.StatusConflict
	out, err = capture(t, func() error {
		return runSetSetRun([]string{"rs-1", "--api", pending.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("a pending set run is not an error: %v", err)
	}
	if !strings.Contains(out, "2 of 3 finished (1 completed, 1 failed)") {
		t.Errorf("pending = %q, want where the items have got to", out)
	}
}

// The set view points at the set run when the workflow has set-scope nodes.
func TestRunSetGetNamesTheSetRun(t *testing.T) {
	api := newFakeAPI(t, `{"id":"rs-1","status":"running","total":2,"completed":2,"failed":0,
		"counts":{"succeeded":2},"holds":[],"set_run":{"id":"run-set","status":"running"}}`)
	out, err := capture(t, func() error {
		return runSetGet([]string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("run-set get: %v", err)
	}
	if !strings.Contains(out, "set run run-set  running") || !strings.Contains(out, "run-set set-run rs-1") {
		t.Errorf("the set run is not named: %q", out)
	}
}

// A rerun posts the server's request shape and reports what reruns, what is reused, the price
// and what was left out; a dry run says nothing was created.
func TestRunSetRerun(t *testing.T) {
	api := newFakeAPI(t, `{"run_set_id":"rs-3","source_run_set_id":"rs-1","existing":false,"dry_run":false,"runs":1000,`+
		`"held_runs":2,"rerun_steps":["publish"],"carried_steps":["extract","enrich"],`+
		`"excluded":[{"reason":"content_purged","runs":3}],"estimated_cost_usd":1.25}`)

	out, err := capture(t, func() error {
		return runSetRerun([]string{"rs-1", "--from", "publish", "--version", "2", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/run-sets/rs-1/rerun" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	var sent struct {
		Version int      `json:"version"`
		From    []string `json:"from"`
	}
	if err := json.Unmarshal(api.body, &sent); err != nil || sent.Version != 2 || len(sent.From) != 1 || sent.From[0] != "publish" {
		t.Errorf("sent %s (%v)", api.body, err)
	}
	for _, want := range []string{"rerunning as rs-3", "rerun: publish; reused: extract, enrich", "$1.2500", "2 run(s) held", "left out: 3 run(s), content_purged"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	if _, err := capture(t, func() error {
		return runSetRerun([]string{"rs-1", "--api", api.URL, "--key", "sk-test"})
	}); err == nil {
		t.Error("a rerun with no --from ran")
	}
}
