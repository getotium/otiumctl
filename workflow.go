package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxDocumentBytes is the largest document the API takes (1 MiB, api/openapi.yaml).
const maxDocumentBytes = 1 << 20

// workflowValidate compiles a document and prints its findings, plan summary or diagram. The
// API compiles it (POST /v1/workflows/validate) and stores nothing, so the answer is the one a
// publish would get, for the account this CLI is signed in to: the same check here, in CI, in
// the publish endpoint and in the browser demo.
//
//	otiumctl workflow validate <file>            findings + summary
//	otiumctl workflow validate <file> --plan     the compiled plan, as JSON
//	otiumctl workflow validate <file> --mermaid  the graph, as a mermaid flowchart
//
// Exit status is 1 when the document would be rejected at publish, so it drops straight
// into a pre-commit hook or a CI step without parsing output.
func workflowValidate(args []string) error {
	fs := flag.NewFlagSet("workflow validate", flag.ContinueOnError)
	asPlan := fs.Bool("plan", false, "print the compiled plan as JSON")
	asMermaid := fs.Bool("mermaid", false, "print the graph as a mermaid flowchart")
	quiet := fs.Bool("quiet", false, "print nothing; use the exit status")
	strict := fs.Bool("strict", false, "treat warnings as failures")
	connect := apiFlags(fs)
	path, err := parseArg(fs, args)
	if err != nil {
		return err
	}

	src, err := readWorkflowSource(path)
	if err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	res, err := validateRemote(c, src, *asPlan)
	if err != nil {
		return err
	}

	switch {
	case *asPlan:
		if len(res.Plan) == 0 {
			return errors.New("document did not compile, so there is no plan")
		}
		var plan any
		if err := json.Unmarshal(res.Plan, &plan); err != nil {
			return fmt.Errorf("read the plan: %w", err)
		}
		out, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
	case *asMermaid:
		if res.Mermaid == "" {
			return errors.New("document did not compile, so there is no diagram")
		}
		fmt.Print(res.Mermaid)
	case !*quiet:
		printFindings(res)
	}

	if res.Diagnostics.hasErrors() {
		return errors.New("document has errors")
	}
	if *strict && len(res.Diagnostics) > 0 {
		return errors.New("document has warnings and --strict is set")
	}
	return nil
}

// parseArgs parses flags and positional arguments that may be interleaved, filling in at
// most max positionals and returning them in order.
//
// Go's flag package stops at the first non-flag argument, so `validate file.yaml --strict`
// would silently ignore --strict — and a silently ignored --strict in a CI step is a gate
// that passes everything. The obvious fix, scanning for arguments that do not start with a
// dash, is WRONG in a way that is worse: it eats a flag's VALUE. `--api https://…` looks
// exactly like a positional to such a scanner, which is invisible when the positional is
// required (the URL lands in a slot that was already filled) and actively misleading when
// it is optional.
//
// So the parse alternates: let the flag package consume what it can, take the one bare
// argument it stopped on, and continue from there. It knows which flags take values; a
// dash-check never can.
//
// A "-" argument means stdin and is a positional, not a flag. An extra positional beyond
// max is an error rather than a silent drop, for the same reason as the ignored flag.
func parseArgs(fs *flag.FlagSet, args []string, max int) ([]string, error) {
	positional := make([]string, 0, max)
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		if len(positional) >= max {
			return nil, fmt.Errorf("unexpected argument %q", rest[0])
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseArg is parseArgs for the common case of one optional positional.
func parseArg(fs *flag.FlagSet, args []string) (string, error) {
	positional, err := parseArgs(fs, args, 1)
	if err != nil {
		return "", err
	}
	if len(positional) == 0 {
		return "", nil
	}
	return positional[0], nil
}

// readWorkflowSource reads a document from a path, or from stdin when the path is "-" or
// absent — so the CLI composes with a pipe as readily as with a file.
func readWorkflowSource(path string) ([]byte, error) {
	if path == "" || path == "-" {
		src, err := io.ReadAll(io.LimitReader(os.Stdin, maxDocumentBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return src, nil
	}
	// gosec G304: reading a caller-named file IS this command. There is no privilege
	// boundary to cross — otiumctl runs as the operator, with the operator's own file access.
	src, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return src, nil
}

// printFindings writes the report a human reads. It names the node and the binding path,
// never just "invalid workflow": every finding here is one that otherwise costs GPU-hours,
// so it has to be actionable without opening the compiler.
func printFindings(res compileResult) {
	for _, d := range res.Diagnostics {
		where := d.Node
		if d.Path != "" {
			where += " " + d.Path
		}
		if where != "" {
			where = " [" + strings.TrimSpace(where) + "]"
		}
		fmt.Fprintf(os.Stderr, "%s: %s%s: %s\n", d.Severity, d.Code, where, d.Message)
	}

	if res.Summary == nil {
		return
	}
	s := res.Summary
	fmt.Printf("%d nodes (%d external, %d tool), critical path %d deep: %s\n",
		s.Nodes, s.ExternalNodes, s.ToolNodes, s.Depth, strings.Join(s.CriticalPath, " → "))
	fmt.Printf("est. duration p50 %.0fs / p95 %.0fs, of which execution p95 %.0fs "+
		"(parks excluded: what the tier's budget is checked against)\n",
		s.P50DurationSeconds, s.P95DurationSeconds, s.P95ExecutionSeconds)
	fmt.Printf("est. cost $%.6f per item, $%.2f per 10,000\n", s.EstCostP50USD, s.CostPer10kUSD)
	if s.Set != nil {
		fmt.Printf("  %s; %d set-scope nodes after every item: %s\n",
			s.Set.line(), s.Set.Nodes, strings.Join(s.Set.CriticalPath, " → "))
	}
	// Never print a bare dollar figure. The rates are illustrative until the quoting
	// engine is wired, and the token counts are the author's only if they declared
	// them — a number carrying neither caveat reads as a quote.
	if s.EstimateBasis == "declared" {
		fmt.Println("  (tokens as declared on each node; rates illustrative, not a quote)")
	} else {
		// Off in either direction, not just low: input misses the per-item data, assets and
		// upstream output, but output is a flat guess that overstates a terse node. The first
		// customer's graph came out 14% HIGH on this basis against its declared tokens.
		fmt.Printf("  (%d of %d nodes declare no estimate, so their tokens were guessed: input from "+
			"the template alone, output a flat 256. Can be off either way; rates illustrative, not a quote)\n",
			s.InferenceNodes-s.DeclaredNodes, s.InferenceNodes)
	}
	fmt.Printf("%d affinity groups, %d barriers, %d cache groups\n",
		s.AffinityGroups, s.Barriers, s.CacheGroups)
	if s.Errors > 0 {
		fmt.Printf("%d error(s) — this would be rejected at publish\n", s.Errors)
	}
}
