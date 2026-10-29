package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// `otiumctl workflow apply -f <dir|file>` — infrastructure-as-code for the graph.
//
//	otiumctl workflow apply -f ./workflows/            publish what changed
//	otiumctl workflow apply -f ./workflows/ --dry-run  say what would change, write nothing
//	otiumctl workflow apply -f ./workflows/ --max-critical-path 4h
//
// There is NO STATE FILE, and that follows from versions being immutable rather than being a simplification.
// A published version is immutable and content-addressed, so "what is deployed" is a
// question the server can always answer exactly; a local state file could only ever be a
// cache of it, and a cache of an immutable fact is a thing that goes wrong for no benefit.
//
// The unit of change is a VERSION, not a workflow. Editing a document does not mutate
// anything — it mints a new version that new submissions pin, while every run already
// referencing the old one keeps referencing it. So `apply` has no update path and no
// destroy path: publish is idempotent on digest (an unchanged document is a no-op), and
// removing a document from the directory does nothing at all, because deleting a graph
// that live runs still execute is not something a directory listing should be able to ask
// for. Deprecation stays an explicit verb.
//
// --max-critical-path is the assertion a customer's CI wants to be able to make: the
// compiled plan's p95 critical path against their tier's window, checked BEFORE the change
// ships rather than discovered at submit.

// maxApplyFiles bounds a directory walk, so a `-f /` typo is an error rather than a
// traversal of the filesystem.
const maxApplyFiles = 500

// applyOutcome is what happened to one document.
type applyOutcome struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
	// Action is one of: "published", "unchanged", "would-publish", "rejected".
	Action string `json:"action"`
	Digest string `json:"digest,omitempty"`
	// Detail carries the reason for a rejection, or the plan headline for a change.
	Detail string `json:"detail,omitempty"`
}

func workflowApply(args []string) error {
	fs := flag.NewFlagSet("workflow apply", flag.ContinueOnError)
	path := fs.String("f", "", "directory or file of workflow documents")
	dryRun := fs.Bool("dry-run", false, "report what would change and write nothing")
	strict := fs.Bool("strict", false, "treat compiler warnings as failures")
	maxPath := fs.Duration("max-critical-path", 0,
		"fail if a document's p95 critical path exceeds this (e.g. 4h)")
	asJSON := fs.Bool("json", false, "print the outcomes as JSON")
	client := apiFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("usage: otiumctl workflow apply -f <dir|file> [--dry-run]")
	}
	c, err := client()
	if err != nil {
		return err
	}

	files, err := workflowFiles(*path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no .yaml or .yml documents under %s", *path)
	}

	outcomes := make([]applyOutcome, 0, len(files))
	failed := 0
	for _, file := range files {
		out := applyOne(c, file, *dryRun, *strict, *maxPath)
		if out.Action == "rejected" {
			failed++
		}
		outcomes = append(outcomes, out)
	}

	if *asJSON {
		if err := printJSON(outcomes); err != nil {
			return err
		}
	} else {
		printApply(outcomes, *dryRun)
	}
	if failed > 0 {
		// Non-zero so this drops into a CI step without parsing output — the same contract
		// `workflow validate` already has.
		return fmt.Errorf("%d of %d document(s) were rejected", failed, len(outcomes))
	}
	return nil
}

// applyOne validates a document, then publishes it unless this is a dry run.
//
// The validate (POST /v1/workflows/validate, which stores nothing) runs for every document
// before any publish, so a directory with one broken document reports WHICH one without having
// published the others, and a --dry-run writes nothing at all.
func applyOne(c *apiClient, file string, dryRun, strict bool, maxPath time.Duration) applyOutcome {
	out := applyOutcome{Path: file}
	src, err := os.ReadFile(file) //nolint:gosec // G304: reading a caller-named file IS this command
	if err != nil {
		out.Action, out.Detail = "rejected", err.Error()
		return out
	}
	res, err := validateRemote(c, src, false)
	if err != nil {
		out.Action, out.Detail = "rejected", err.Error()
		return out
	}
	out.Name, out.Digest = res.Name, res.Digest
	switch {
	case res.Diagnostics.hasErrors():
		out.Action, out.Detail = "rejected", firstError(res.Diagnostics)
		return out
	case strict && len(res.Diagnostics) > 0:
		out.Action, out.Detail = "rejected", "warnings, and --strict is set: "+res.Diagnostics[0].Message
		return out
	}
	if maxPath > 0 && res.Summary != nil {
		if p95 := time.Duration(res.Summary.P95DurationSeconds * float64(time.Second)); p95 > maxPath {
			// The assertion a customer's CI wants to make before the change ships:
			// a graph whose critical path no longer fits the window they bought is a
			// deadline miss discovered at submit, on every item, at scale.
			out.Action = "rejected"
			out.Detail = fmt.Sprintf("p95 critical path %s exceeds --max-critical-path %s",
				p95, maxPath)
			return out
		}
	}
	if res.Summary != nil {
		out.Detail = fmt.Sprintf("%d nodes, p95 %.0fs, $%.4f/item",
			res.Summary.Nodes, res.Summary.P95DurationSeconds, res.Summary.EstCostP50USD)
	}

	if dryRun {
		// A dry run says "would publish" without asking the server whether the digest is
		// already live, because that answer is not stable: another apply can land between
		// the plan and the apply. Publish is idempotent, so the honest report is "this
		// document is publishable", and the apply itself says whether it minted a version.
		out.Action = "would-publish"
		return out
	}

	var resp struct {
		OK      bool `json:"ok"`
		Created bool `json:"created"`
		Version *struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
			Digest  string `json:"digest"`
		} `json:"version"`
		Diagnostics diagnostics `json:"diagnostics"`
	}
	if err := c.do(context.Background(), http.MethodPost, "/v1/workflows", documentEnvelope{Document: string(src)}, &resp); err != nil {
		out.Action, out.Detail = "rejected", err.Error()
		return out
	}
	if resp.Version != nil {
		out.Name, out.Version, out.Digest = resp.Version.Name, resp.Version.Version, resp.Version.Digest
	}
	// "unchanged" is the normal outcome of a CI job that runs on every commit, not a
	// warning: idempotency on digest is what makes `apply` safe to run unconditionally.
	out.Action = "unchanged"
	if resp.Created {
		out.Action = "published"
	}
	return out
}

// printApply renders the report a human reads, ordered so the things that need attention
// are not buried under the things that did not change.
func printApply(outcomes []applyOutcome, dryRun bool) {
	byAction := map[string][]applyOutcome{}
	for _, o := range outcomes {
		byAction[o.Action] = append(byAction[o.Action], o)
	}
	for _, action := range []string{"rejected", "published", "would-publish", "unchanged"} {
		list := byAction[action]
		if len(list) == 0 {
			continue
		}
		for _, o := range list {
			name := o.Name
			if name == "" {
				name = filepath.Base(o.Path)
			}
			switch action {
			case "rejected":
				fmt.Printf("REJECTED  %-28s %s\n", name, o.Detail)
			case "published":
				fmt.Printf("published %-28s v%d  %s\n", name, o.Version, o.Detail)
			case "would-publish":
				fmt.Printf("would publish %-24s %s\n", name, o.Detail)
			default:
				fmt.Printf("unchanged %-28s v%d\n", name, o.Version)
			}
		}
	}
	verb := "applied"
	if dryRun {
		verb = "planned"
	}
	fmt.Printf("\n%s %d document(s): %d published, %d unchanged, %d rejected\n",
		verb, len(outcomes), len(byAction["published"])+len(byAction["would-publish"]),
		len(byAction["unchanged"]), len(byAction["rejected"]))
}

// workflowFiles resolves -f to a sorted list of documents. A single file is allowed so the
// verb works before a customer has a directory to point at.
func workflowFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var files []string
	// Not recursive. A workflow directory is a flat set of documents, and walking
	// subdirectories would sweep up a `fixtures/` or `examples/` folder and publish it.
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isWorkflowDoc(e.Name()) {
			continue
		}
		files = append(files, filepath.Join(path, e.Name()))
		if len(files) > maxApplyFiles {
			return nil, fmt.Errorf("more than %d documents under %s; point -f at a workflow directory",
				maxApplyFiles, path)
		}
	}
	sort.Strings(files)
	return files, nil
}

func isWorkflowDoc(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// firstError returns the first error diagnostic's message, which is the one a reader acts
// on; the rest are available from `workflow validate`.
func firstError(diags diagnostics) string {
	for _, d := range diags {
		if d.Severity == "error" {
			where := d.Node
			if d.Path != "" {
				where += " " + d.Path
			}
			if where = strings.TrimSpace(where); where != "" {
				return fmt.Sprintf("%s [%s]: %s", d.Code, where, d.Message)
			}
			return fmt.Sprintf("%s: %s", d.Code, d.Message)
		}
	}
	return "document was rejected"
}

// documentEnvelope is what the publish endpoint takes over JSON. The endpoint also accepts
// a raw YAML body, but the envelope is what the portal's editor posts and reusing it keeps
// one path through the server rather than two.
type documentEnvelope struct {
	Document string `json:"document"`
}
