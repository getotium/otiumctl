package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The report POST /v1/workflows/test answers with (api/openapi.yaml, WorkflowTestReport): every
// field, so --json passes it on whole.
type testReport struct {
	Passed int          `json:"passed"`
	Failed int          `json:"failed"`
	Cases  []testResult `json:"cases"`
}

type testResult struct {
	Name     string       `json:"name"`
	Pass     bool         `json:"pass"`
	Failures []string     `json:"failures,omitempty"`
	Run      testObserved `json:"run"`
}

type testObserved struct {
	Rejected *testRejection `json:"rejected,omitempty"`
	Status   string         `json:"status,omitempty"`
	Outcome  string         `json:"outcome,omitempty"`
	Nodes    []testNode     `json:"nodes,omitempty"`
	Errors   []string       `json:"errors,omitempty"`
	Items    []testItem     `json:"items,omitempty"`
}

type testItem struct {
	CustomID string     `json:"custom_id"`
	Status   string     `json:"status,omitempty"`
	Outcome  string     `json:"outcome,omitempty"`
	Nodes    []testNode `json:"nodes,omitempty"`
}

type testRejection struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type testNode struct {
	Key      string        `json:"key"`
	Status   string        `json:"status"`
	Reason   string        `json:"reason,omitempty"`
	Attempts []testAttempt `json:"attempts,omitempty"`
	Prompt   string        `json:"prompt,omitempty"`
	Output   string        `json:"output,omitempty"`
}

type testAttempt struct {
	PolicyStep int    `json:"policy_step"`
	Product    string `json:"product,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

// workflowTest runs a workflow's unit tests: the API runs the real ingress and engine over the
// suite's canned node outputs (POST /v1/workflows/test), with no GPU and nothing written
// . The snapshots are compared here, beside the test file.
//
//	otiumctl workflow test <file|dir>... [--run <substring>] [--snapshots] [--update] [--verbose] [--json]
//
// A directory is searched for *.test.yaml. A suite's `workflow:` is read relative to the test
// file, and defaults to the same name without `.test` (enrich.test.yaml tests enrich.yaml).
//
// --snapshots compares every node's rendered prompt with __snapshots__/<suite>/<case>/<node>.txt
// beside the test file, so a prompt change shows up as a diff in review; --update writes them.
//
// Exit status is 1 when any case fails, so it drops into a pre-commit hook or a CI step.
func workflowTest(args []string) error {
	fset := flag.NewFlagSet("workflow test", flag.ContinueOnError)
	only := fset.String("run", "", "run only the cases whose name contains this")
	snapshots := fset.Bool("snapshots", false, "compare rendered prompts with __snapshots__ beside the test file")
	update := fset.Bool("update", false, "write the __snapshots__ instead of comparing (implies --snapshots)")
	verbose := fset.Bool("verbose", false, "print every case's node trace, not only failing ones")
	asJSON := fset.Bool("json", false, "print the reports as JSON")
	connect := apiFlags(fset)
	paths, err := parseArgs(fset, args, 64)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	files, err := testFiles(paths)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no *.test.yaml files found")
	}
	c, err := connect()
	if err != nil {
		return err
	}

	type fileReport struct {
		File   string      `json:"file"`
		Report *testReport `json:"report"`
	}
	var reports []fileReport
	passed, failed := 0, 0
	for _, file := range files {
		rep, err := runTestFile(c, file, *only, *snapshots || *update, *update)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		passed += rep.Passed
		failed += rep.Failed
		reports = append(reports, fileReport{File: file, Report: rep})
		if !*asJSON {
			printTestReport(file, rep, *verbose)
		}
	}
	if *asJSON {
		out, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
	} else {
		fmt.Printf("\n%d passed, %d failed\n", passed, failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d case(s) failed", failed)
	}
	return nil
}

// testFiles expands files and directories into the test files to run, in a stable order.
func testFiles(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		st, err := os.Stat(p) //nolint:gosec // G703: walking the caller-named paths IS this command
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // G703: as above
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "__snapshots__" || strings.HasPrefix(d.Name(), ".") && path != p) {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".test.yaml") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// runTestFile reads one suite and its workflow, has the API run it, and applies snapshots.
func runTestFile(c *apiClient, file, only string, snapshots, update bool) (*testReport, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // G304: reading a caller-named file IS this command
	if err != nil {
		return nil, err
	}
	// Only the suite's `workflow:` is read here, to find the document; the API parses the rest.
	var head struct {
		Workflow string `yaml:"workflow"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("read the suite: %w", err)
	}
	wf := head.Workflow
	if wf == "" {
		wf = strings.TrimSuffix(filepath.Base(file), ".test.yaml") + ".yaml"
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), wf)) //nolint:gosec // G304: the workflow the caller's test file names
	if err != nil {
		return nil, fmt.Errorf("read the workflow under test: %w", err)
	}
	rep := &testReport{}
	req := map[string]string{"document": string(src), "tests": string(raw), "run": only}
	if err := c.do(context.Background(), http.MethodPost, "/v1/workflows/test", req, rep); err != nil {
		return nil, fmt.Errorf("run the suite: %w", err)
	}
	if snapshots {
		if err := applySnapshots(file, rep, update); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

var unsafeName = regexp.MustCompile(`[^a-z0-9]+`)

// slug turns a case name into a directory name.
func slug(s string) string {
	return strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// applySnapshots writes or compares each node's rendered prompt. A mismatch fails the case:
// the prompt is the bytes the customer pays for, and an unreviewed change to it is the
// regression a snapshot exists to catch.
func applySnapshots(file string, rep *testReport, update bool) error {
	root := filepath.Join(filepath.Dir(file), "__snapshots__", strings.TrimSuffix(filepath.Base(file), ".test.yaml"))
	for i := range rep.Cases {
		c := &rep.Cases[i]
		// A case with items snapshots the set run's prompts where a plain case keeps its run's,
		// and each item's under items/<custom_id>.
		runs := []promptDir{{filepath.Join(root, slug(c.Name)), c.Run.Nodes}}
		for _, it := range c.Run.Items {
			runs = append(runs, promptDir{filepath.Join(root, slug(c.Name), "items", slug(it.CustomID)), it.Nodes})
		}
		for _, n := range promptedNodes(runs) {
			dir := n.dir
			path := filepath.Join(dir, n.Key+".txt")
			shown := path
			if cwd, err := os.Getwd(); err == nil {
				if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
					shown = rel
				}
			}
			if update {
				if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G703: dir is a slug ([a-z0-9-]) under __snapshots__
					return err
				}
				if err := os.WriteFile(path, []byte(n.Prompt), 0o600); err != nil { //nolint:gosec // G703: a slug and a compiler-checked node key
					return err
				}
				continue
			}
			want, err := os.ReadFile(path) //nolint:gosec // G304: a snapshot beside the caller's test file
			switch {
			case errors.Is(err, fs.ErrNotExist):
				c.Failures = append(c.Failures, fmt.Sprintf("%s has no prompt snapshot (%s); run with --update to write it", n.Key, shown))
			case err != nil:
				return err
			case string(want) != n.Prompt:
				c.Failures = append(c.Failures, fmt.Sprintf("%s's prompt differs from its snapshot (%s): %s; run with --update to accept",
					n.Key, shown, firstDifference(string(want), n.Prompt)))
			}
		}
		if c.Pass && len(c.Failures) > 0 {
			c.Pass = false
			rep.Passed--
			rep.Failed++
		}
	}
	return nil
}

// promptDir is one run's nodes and the directory their snapshots live in.
type promptDir struct {
	dir   string
	nodes []testNode
}

// dirNode is a node with a rendered prompt, and the directory its snapshot lives in.
type dirNode struct {
	testNode
	dir string
}

// promptedNodes flattens runs into the nodes that rendered a prompt.
func promptedNodes(runs []promptDir) []dirNode {
	var out []dirNode
	for _, r := range runs {
		for _, n := range r.nodes {
			if n.Prompt != "" {
				out = append(out, dirNode{testNode: n, dir: r.dir})
			}
		}
	}
	return out
}

// firstDifference names the first line where two prompts part.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d was %q, now %q", i+1, wl, gl)
		}
	}
	return "whitespace differs"
}

// printTestReport prints one suite's results, with the node trace for a failing case (or for
// every case under --verbose): what the engine did is how the author finds why.
func printTestReport(file string, rep *testReport, verbose bool) {
	fmt.Println(file)
	if len(rep.Cases) == 0 {
		fmt.Println("  (no cases matched)")
	}
	for _, c := range rep.Cases {
		mark := "ok  "
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Printf("  %s %s\n", mark, c.Name)
		for _, f := range c.Failures {
			fmt.Printf("         - %s\n", f)
		}
		if !c.Pass || verbose {
			printTrace(c.Run)
		}
	}
}

func printTrace(o testObserved) {
	if o.Rejected != nil {
		fmt.Printf("         refused: %s: %s\n", o.Rejected.Code, o.Rejected.Message)
		return
	}
	if o.Status == "" {
		return
	}
	// A case with items: each item's run, then the set run under the set's status.
	for _, it := range o.Items {
		fmt.Printf("         item %s %s\n", it.CustomID, it.Status)
		printNodes(it.Nodes)
	}
	if len(o.Items) > 0 {
		fmt.Printf("         set %s, set run:\n", o.Status)
	} else {
		fmt.Printf("         run %s\n", o.Status)
	}
	printNodes(o.Nodes)
}

func printNodes(nodes []testNode) {
	for _, n := range nodes {
		line := fmt.Sprintf("           %-18s %s", n.Key, n.Status)
		if n.Reason != "" && n.Reason != "ok" {
			line += " (" + n.Reason + ")"
		}
		if len(n.Attempts) > 1 {
			var products []string
			for _, a := range n.Attempts {
				products = append(products, a.Product)
			}
			line += fmt.Sprintf("  %d attempts: %s", len(n.Attempts), strings.Join(products, " → "))
		}
		fmt.Println(line)
	}
}
