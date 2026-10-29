package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
)

// The run verbs (WP-G).
//
// They exist because a stopped pipeline is debugged by a human, and every question that
// human asks — which node stopped, what did the model actually say, can I resume without
// paying twice — currently has an answer only over raw HTTP. The output and continue verbs
// matter most: a continuation has no batch and therefore no output file, so `run-set
// output` is the only way to read one.
//
//	otiumctl run get <run-id>
//	otiumctl run attempts <run-id> <node-key>
//	otiumctl run continue <run-id>
//	otiumctl run-set get <set-id>
//	otiumctl run-set output <set-id> [--out <file>]
//	otiumctl run-set node-output <set-id> <node-key> [--out <file>]
//	otiumctl run-set set-run <set-id>
//	otiumctl run-set continue <set-id>

func runCommand(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("run needs a verb")
	}
	switch args[0] {
	case "get":
		return runGet(args[1:])
	case "attempts":
		return runAttempts(args[1:])
	case "continue":
		return runContinue("/v1/runs/", args[1:])
	default:
		usage()
		return fmt.Errorf("unknown run verb %q", args[0])
	}
}

func runSetCommand(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("run-set needs a verb")
	}
	switch args[0] {
	case "get":
		return runSetGet(args[1:])
	case "output":
		return runSetOutput(args[1:])
	case "node-output":
		return runSetNodeOutput(args[1:])
	case "set-run":
		return runSetSetRun(args[1:])
	case "continue":
		return runContinue("/v1/run-sets/", args[1:])
	case "rerun":
		return runSetRerun(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown run-set verb %q", args[0])
	}
}

// runGet prints one item's journey through the graph.
func runGet(args []string) error {
	fs := flag.NewFlagSet("run get", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl run get <run-id>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out runView
	if done, err := getView(c, "/v1/runs/"+id, &out, *asJSON); done || err != nil {
		return err
	}
	printRunView(out)
	return nil
}

// runView is GET /v1/runs/{id}, and the set run at GET /v1/run-sets/{id}/set.
type runView struct {
	ID                    string  `json:"id"`
	RunSetID              string  `json:"run_set_id"`
	CustomID              string  `json:"custom_id"`
	Status                string  `json:"status"`
	Outcome               string  `json:"outcome"`
	BudgetConsumedSeconds int64   `json:"budget_consumed_seconds"`
	ParkedSeconds         int64   `json:"parked_seconds"`
	ExecutionSeconds      float64 `json:"execution_seconds"`
	CostUSD               float64 `json:"cost_usd"`
	Nodes                 []struct {
		NodeKey            string    `json:"node_key"`
		Status             string    `json:"status"`
		TerminalReason     string    `json:"terminal_reason"`
		Disposition        string    `json:"disposition"`
		InheritedFromRunID string    `json:"inherited_from_run_id"`
		RejectedOutput     any       `json:"rejected_output"`
		Hold               *viewHold `json:"hold"`
	} `json:"nodes"`
}

// printRunView prints one run: its status, both clocks, and every node.
func printRunView(out runView) {
	fmt.Printf("%s  %s", out.ID, out.Status)
	if out.Outcome != "" {
		fmt.Printf(" (%s)", out.Outcome)
	}
	fmt.Printf("  custom_id=%s  cost $%.6f\n", out.CustomID, out.CostUSD)
	// Both clocks, always. "7 days of execution time — the clock stops while a run is
	// waiting on you" is only a checkable promise if both numbers are visible. The
	// window's clock includes the wait for a GPU, so the time on workers is its own number.
	fmt.Printf("window used %s, on workers %s, parked %s\n",
		humanSeconds(out.BudgetConsumedSeconds), humanSeconds(int64(math.Round(out.ExecutionSeconds))),
		humanSeconds(out.ParkedSeconds))

	fmt.Printf("\n%-20s %-10s %-12s %-18s %s\n", "NODE", "STATUS", "DISPOSITION", "REASON", "NOTE")
	for _, n := range out.Nodes {
		note := ""
		if n.InheritedFromRunID != "" {
			note = "inherited from " + n.InheritedFromRunID
		}
		if n.RejectedOutput != nil {
			// Point at the verb that shows it rather than dumping a model completion into
			// the middle of a status table.
			note = "rejected output — see `otiumctl run attempts " + out.ID + " " + n.NodeKey + "`"
		}
		if n.Hold != nil {
			note = n.Hold.String()
		}
		fmt.Printf("%-20s %-10s %-12s %-18s %s\n",
			n.NodeKey, n.Status, n.Disposition, n.TerminalReason, note)
	}
}

// runSetSetRun prints a set's set run: the run that carries the workflow's set-scope nodes,
// once every item has finished. Until then the API answers 409 with
// the items' counts, which this prints as where the items have got to.
func runSetSetRun(args []string) error {
	fs := flag.NewFlagSet("run-set set-run", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl run-set set-run <set-id>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out runView
	done, err := getView(c, "/v1/run-sets/"+id+"/set", &out, *asJSON)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		var pending struct {
			Status string `json:"status"`
			Items  struct {
				Total, Completed, Failed int
			} `json:"items"`
		}
		if json.Unmarshal([]byte(apiErr.Message), &pending) == nil && pending.Status != "" {
			if *asJSON {
				return printJSON(json.RawMessage(apiErr.Message))
			}
			fmt.Printf("%s: set run %s — the items are still running: %d of %d finished (%d completed, %d failed)\n",
				id, pending.Status, pending.Items.Completed+pending.Items.Failed, pending.Items.Total,
				pending.Items.Completed, pending.Items.Failed)
			return nil
		}
	}
	if done || err != nil {
		return err
	}
	printRunView(out)
	return nil
}

// runAttempts prints every attempt of one node, with what it ran on and what it produced.
//
// This is the verb for "why did my validator reject this?", which is the question a
// customer cannot answer from a status alone — and the one they are most likely to be
// asking when they reach for a CLI at all.
func runAttempts(args []string) error {
	fs := flag.NewFlagSet("run attempts", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	full := fs.Bool("full", false, "print each attempt's whole output rather than a first line")
	client := apiFlags(fs)
	positional, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return errors.New("usage: otiumctl run attempts <run-id> <node-key>")
	}
	c, err := client()
	if err != nil {
		return err
	}

	var out struct {
		NodeKey  string  `json:"node_key"`
		Status   string  `json:"status"`
		CostUSD  float64 `json:"cost_usd"`
		Attempts []struct {
			Attempt        int    `json:"attempt"`
			PolicyStep     int    `json:"policy_step"`
			Status         string `json:"status"`
			TerminalReason string `json:"terminal_reason"`
			FromRunID      string `json:"from_run_id"`
			Identity       struct {
				Model         string `json:"model"`
				ModelRevision string `json:"model_revision"`
				Region        string `json:"region"`
			} `json:"identity"`
			Output   any `json:"output"`
			Verdicts []struct {
				OK       bool   `json:"ok"`
				Code     string `json:"code"`
				Pointer  string `json:"pointer"`
				Expected string `json:"expected"`
			} `json:"verdicts"`
			OutputTokens     int     `json:"output_tokens"`
			CostUSD          float64 `json:"cost_usd"`
			StartedAt        string  `json:"started_at"`
			LeasedAt         string  `json:"leased_at"`
			ExecutionSeconds float64 `json:"execution_seconds"`
		} `json:"attempts"`
	}
	path := "/v1/runs/" + positional[0] + "/nodes/" + positional[1] + "/attempts"
	if done, err := getView(c, path, &out, *asJSON); done || err != nil {
		return err
	}

	fmt.Printf("node %s: %s, %d attempt(s), $%.6f\n", out.NodeKey, out.Status, len(out.Attempts), out.CostUSD)
	for _, a := range out.Attempts {
		fmt.Printf("\nattempt %d (policy step %d): %s", a.Attempt, a.PolicyStep, a.Status)
		if a.TerminalReason != "" {
			fmt.Printf(" — %s", a.TerminalReason)
		}
		fmt.Println()
		fmt.Printf("  ran on %s", a.Identity.Model)
		if a.Identity.ModelRevision != "" {
			fmt.Printf("@%s", a.Identity.ModelRevision)
		}
		if a.Identity.Region != "" {
			fmt.Printf(" in %s", a.Identity.Region)
		}
		fmt.Printf(", %d output tokens, $%.6f\n", a.OutputTokens, a.CostUSD)
		// Queued and leased are different moments; the run time starts at the lease.
		switch {
		case a.LeasedAt != "":
			fmt.Printf("  queued %s, leased %s", a.StartedAt, a.LeasedAt)
			if a.ExecutionSeconds > 0 {
				fmt.Printf(", ran %s", humanSeconds(int64(math.Round(a.ExecutionSeconds))))
			}
			fmt.Println()
		case a.Status == "running":
			fmt.Printf("  queued %s, waiting for a worker\n", a.StartedAt)
		}
		if a.FromRunID != "" {
			fmt.Printf("  produced by run %s and carried into this one\n", a.FromRunID)
		}
		for _, v := range a.Verdicts {
			if v.OK {
				continue
			}
			// A verdict names a location and a constraint and never a value, so it
			// is safe to print verbatim.
			fmt.Printf("  rejected: %s at %s (expected %s)\n", v.Code, v.Pointer, v.Expected)
		}
		if a.Output != nil {
			fmt.Printf("  output: %s\n", previewOutput(a.Output, *full))
		}
	}
	return nil
}

// runSetGet prints a set's aggregate state.
func runSetGet(args []string) error {
	fs := flag.NewFlagSet("run-set get", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl run-set get <set-id>")
	}
	c, err := client()
	if err != nil {
		return err
	}

	var out struct {
		ID                     string         `json:"id"`
		WorkflowID             string         `json:"workflow_id"`
		Status                 string         `json:"status"`
		Outcome                string         `json:"outcome"`
		ExecutionBudgetSeconds int64          `json:"execution_budget_seconds"`
		ExpiresAt              string         `json:"expires_at"`
		Total                  int            `json:"total"`
		Completed              int            `json:"completed"`
		Failed                 int            `json:"failed"`
		Counts                 map[string]int `json:"counts"`
		Holds                  []struct {
			NodeKey string `json:"node_key"`
			Runs    int    `json:"runs"`
			viewHold
		} `json:"holds"`
		SetRun *struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Outcome string `json:"outcome"`
		} `json:"set_run"`
	}
	if done, err := getView(c, "/v1/run-sets/"+id, &out, *asJSON); done || err != nil {
		return err
	}
	fmt.Printf("%s  %s", out.ID, out.Status)
	if out.Outcome != "" {
		fmt.Printf(" (%s)", out.Outcome)
	}
	fmt.Printf("\n%d runs: %d completed, %d failed\n", out.Total, out.Completed, out.Failed)
	// The two clocks again: a budget the customer bought, and a wall-clock retention bound
	// that is not an SLA.
	fmt.Printf("execution budget %s, expires %s\n",
		humanSeconds(out.ExecutionBudgetSeconds), out.ExpiresAt)
	if len(out.Counts) > 0 {
		parts := make([]string, 0, len(out.Counts))
		for status, n := range out.Counts {
			parts = append(parts, fmt.Sprintf("%s=%d", status, n))
		}
		fmt.Println(strings.Join(parts, "  "))
	}
	// Held is not stuck: a `running` set whose queued work waits for a cold start the planner
	// is holding on purpose says why, and until when.
	for _, h := range out.Holds {
		fmt.Printf("%s: %d run(s) %s\n", h.NodeKey, h.Runs, h.String())
	}
	// The run carrying the set-scope nodes, which run once the items have finished.
	if s := out.SetRun; s != nil {
		line := "set run " + s.Status
		if s.ID != "" {
			line = "set run " + s.ID + "  " + s.Status
		}
		if s.Outcome != "" {
			line += " (" + s.Outcome + ")"
		}
		fmt.Println(line + "  — otiumctl run-set set-run " + out.ID)
	}
	return nil
}

// viewHold is why queued work has not started yet (the run views' `hold`).
type viewHold struct {
	Reason      string `json:"reason"`
	LatestStart string `json:"latest_start"`
}

func (h viewHold) String() string {
	var s string
	switch h.Reason {
	case "gathering":
		s = "held: gathering work to share one GPU boot"
	case "price":
		s = "held: waiting for a cheaper GPU"
	default:
		s = "held: " + h.Reason
	}
	if h.LatestStart != "" {
		s += ", starts by " + h.LatestStart
	}
	return s
}

// getView fetches a run view. With --json it prints the server's response as it came, so a
// field this build does not know yet is still there for a script, and reports done.
// Otherwise it decodes into out for the table.
func getView(c *apiClient, path string, out any, asJSON bool) (done bool, err error) {
	if !asJSON {
		return false, c.do(context.Background(), http.MethodGet, path, nil, out)
	}
	var raw json.RawMessage
	if err := c.do(context.Background(), http.MethodGet, path, nil, &raw); err != nil {
		return true, err
	}
	return true, printJSON(raw)
}

// runSetOutput streams a set's results as JSONL.
//
// It is the only way to read a CONTINUATION's results: a continuation is created by an API
// call rather than a file upload, so it has no output file to download.
func runSetOutput(args []string) error {
	fs := flag.NewFlagSet("run-set output", flag.ContinueOnError)
	outPath := fs.String("out", "", "write to a file instead of stdout")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl run-set output <set-id> [--out <file>]")
	}
	c, err := client()
	if err != nil {
		return err
	}

	w := os.Stdout
	if *outPath != "" {
		// gosec G304: writing to a caller-named path IS this command.
		f, err := os.Create(*outPath) //nolint:gosec
		if err != nil {
			return fmt.Errorf("create %s: %w", *outPath, err)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	return c.stream(context.Background(), "/v1/run-sets/"+id+"/output", w)
}

// runSetNodeOutput downloads ONE node's results in stock OpenAI batch-output format
// — the migration lever, as a file.
//
// It is deliberately a separate verb rather than a flag on `output`: the two files are
// different formats for different readers. `output` is our per-run line with every node on
// it; this one is the shape a customer's existing collect code already parses, and
// conflating them behind a flag would make "which format did I just download?" depend on
// an argument someone forgot.
func runSetNodeOutput(args []string) error {
	fs := flag.NewFlagSet("run-set node-output", flag.ContinueOnError)
	outPath := fs.String("out", "", "write to a file instead of stdout")
	client := apiFlags(fs)
	positional, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return errors.New("usage: otiumctl run-set node-output <set-id> <node-key> [--out <file>]")
	}
	c, err := client()
	if err != nil {
		return err
	}

	w := os.Stdout
	if *outPath != "" {
		// gosec G304: writing to a caller-named path IS this command.
		f, err := os.Create(*outPath) //nolint:gosec
		if err != nil {
			return fmt.Errorf("create %s: %w", *outPath, err)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	return c.stream(context.Background(),
		"/v1/run-sets/"+positional[0]+"/nodes/"+positional[1]+"/output.jsonl", w)
}

// runContinue resumes work that stopped part-way, carrying forward what was already paid
// for. One prefix serves both the set and the single-run form, because the response
// is the same shape and the difference is only which id the caller has to hand.
func runContinue(prefix string, args []string) error {
	fs := flag.NewFlagSet("continue", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("a run or run-set id is required")
	}
	c, err := client()
	if err != nil {
		return err
	}

	// The server's job.Continuation. These names were once set_id/inherited/rerunning, which
	// the server never sent, so every continuation printed a blank id and zero counts; the
	// test feeds this the server's own type so the two cannot drift again.
	var out struct {
		SetID     string `json:"run_set_id"`
		Existing  bool   `json:"existing"`
		Runs      int    `json:"runs"`
		Inherited int    `json:"inherited_nodes"`
		Rerunning int    `json:"rerunning_nodes"`
		Skipped   int    `json:"skipped_runs"`
		Held      int    `json:"held_runs"`
		Rerun     []struct {
			NodeKey string `json:"node_key"`
			Reason  string `json:"reason"`
			Runs    int    `json:"runs"`
		} `json:"rerun"`
	}
	var raw json.RawMessage
	if err := c.do(context.Background(), http.MethodPost, prefix+id+"/continue", nil, &raw); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			// "nothing to continue" and "still running" are answers, not malfunctions.
			return fmt.Errorf("refused: %s", apiErr.Message)
		}
		return err
	}
	if *asJSON {
		return printJSON(raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("read the continuation: %w", err)
	}
	if out.Existing {
		// Idempotency is the point: a retried call recovers the continuation it already
		// made rather than buying the graph a second time.
		fmt.Printf("already continued as %s\n", out.SetID)
	} else {
		fmt.Printf("continuing as %s\n", out.SetID)
	}
	fmt.Printf("%d run(s): %d node(s) carried forward, %d to re-run\n",
		out.Runs, out.Inherited, out.Rerunning)
	if out.Skipped > 0 {
		fmt.Printf("%d run(s) not continued: already succeeded, or continued before\n", out.Skipped)
	}
	if out.Held > 0 {
		// A continuation is a new purchase, gated like a batch line.
		fmt.Printf("%d run(s) held until your balance or monthly spend cap covers them; they start on their own\n", out.Held)
	}
	for _, r := range out.Rerun {
		fmt.Printf("  %-20s %-18s %d run(s)\n", r.NodeKey, r.Reason, r.Runs)
	}
	return nil
}

// runSetRerun reruns a finished set from one or more steps against a version of its workflow,
// reusing the set's recorded output for every other step. --dry-run prices it first.
func runSetRerun(args []string) error {
	fs := flag.NewFlagSet("run-set rerun", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	version := fs.Int("version", 0, "workflow version to rerun against (default: the latest published)")
	from := fs.String("from", "", "comma-separated steps to rerun; they and everything downstream run again")
	dryRun := fs.Bool("dry-run", false, "create nothing; show what the rerun would cover and cost")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" || *from == "" {
		return errors.New("usage: otiumctl run-set rerun <set-id> --from step[,step] [--version n] [--dry-run]")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var steps []string
	for _, s := range strings.Split(*from, ",") {
		if s = strings.TrimSpace(s); s != "" {
			steps = append(steps, s)
		}
	}
	// The server's job.RerunRequest and job.RerunResult; the test feeds this the server's own
	// types so the names cannot drift.
	req := struct {
		Version int      `json:"version,omitempty"`
		From    []string `json:"from"`
		DryRun  bool     `json:"dry_run,omitempty"`
	}{*version, steps, *dryRun}
	var raw json.RawMessage
	if err := c.do(context.Background(), http.MethodPost, "/v1/run-sets/"+id+"/rerun", req, &raw); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			// A changed reused step, an unknown version: answers, not malfunctions.
			return fmt.Errorf("refused: %s", apiErr.Message)
		}
		return err
	}
	if *asJSON {
		return printJSON(raw)
	}
	var out struct {
		SetID            string   `json:"run_set_id"`
		Existing         bool     `json:"existing"`
		DryRun           bool     `json:"dry_run"`
		Runs             int      `json:"runs"`
		Held             int      `json:"held_runs"`
		RerunSteps       []string `json:"rerun_steps"`
		CarriedSteps     []string `json:"carried_steps"`
		EstimatedCostUSD float64  `json:"estimated_cost_usd"`
		Excluded         []struct {
			Reason string `json:"reason"`
			Runs   int    `json:"runs"`
		} `json:"excluded"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("read the rerun: %w", err)
	}
	switch {
	case out.DryRun:
		fmt.Println("dry run: nothing created")
	case out.Existing:
		fmt.Printf("already rerun as %s\n", out.SetID)
	default:
		fmt.Printf("rerunning as %s\n", out.SetID)
	}
	fmt.Printf("%d run(s); rerun: %s; reused: %s\n", out.Runs,
		strings.Join(out.RerunSteps, ", "), orNone(strings.Join(out.CarriedSteps, ", ")))
	fmt.Printf("estimated cost (p95): $%.4f\n", out.EstimatedCostUSD)
	if out.Held > 0 {
		fmt.Printf("%d run(s) held until your balance or monthly spend cap covers them; they start on their own\n", out.Held)
	}
	for _, x := range out.Excluded {
		fmt.Printf("  left out: %d run(s), %s\n", x.Runs, x.Reason)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// previewOutput renders a model output for a terminal. Truncated by default: an attempt
// listing is a scan, and one 4,000-token completion would bury the five lines around it.
//
// A string is printed as itself rather than re-encoded. The API hands back JSON when the
// model produced JSON and a quoted string when it produced prose, so marshalling
// everything would render a body that is already JSON text as `"{\"nam\":\"x\"}"` —
// double-escaped, in the one view whose whole job is to show a customer what their model
// actually said.
func previewOutput(v any, full bool) string {
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprintf("%v", v)
		if b, err := marshalCompact(v); err == nil {
			s = b
		}
	}
	if full {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	const width = 160
	if len(s) > width {
		s = s[:width] + " … (--full for all of it)"
	}
	return s
}

// humanSeconds renders a duration a person reads in a status line.
func humanSeconds(n int64) string {
	switch {
	case n >= 86400:
		return fmt.Sprintf("%.1fd", float64(n)/86400)
	case n >= 3600:
		return fmt.Sprintf("%.1fh", float64(n)/3600)
	case n >= 60:
		return fmt.Sprintf("%.0fm", float64(n)/60)
	default:
		return fmt.Sprintf("%ds", n)
	}
}
