package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go's flag package stops at the first non-flag argument, so `validate file.yaml --strict`
// would silently ignore --strict — and a silently ignored --strict in a CI step is a gate
// that passes everything. parseArg alternates between the flag package and the positional
// so both orders work.
//
// The last two cases are the ones a dash-scanning splitter gets WRONG, and they are why
// this parse uses the flag set rather than looking for a leading "-": a flag's VALUE looks
// exactly like a positional, so `--api https://…` would be read as the file.
func TestParseArgTakesTheFileFromEitherSideOfTheFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantPath  string
		wantPlan  bool
		wantQuiet bool
		wantErr   bool
	}{
		{name: "flags after the file", args: []string{"a.yaml", "--plan", "--quiet"},
			wantPath: "a.yaml", wantPlan: true, wantQuiet: true},
		{name: "flags before the file", args: []string{"--plan", "a.yaml"},
			wantPath: "a.yaml", wantPlan: true},
		{name: "flags on both sides", args: []string{"--plan", "a.yaml", "--quiet"},
			wantPath: "a.yaml", wantPlan: true, wantQuiet: true},
		{name: "no file means stdin", args: []string{"--plan"}, wantPlan: true},
		{name: "explicit stdin is a path, not a flag", args: []string{"-", "--plan"},
			wantPath: "-", wantPlan: true},
		{name: "nothing at all"},
		// A second bare argument is an ERROR, not something silently dropped: `validate
		// a.yaml b.yaml` compiling only the first file is worse than refusing.
		{name: "a second bare argument is refused", args: []string{"a.yaml", "b.yaml"}, wantErr: true},
		// The two that matter. A value-taking flag's argument must never become the file.
		{name: "a flag value is not the file", args: []string{"--label", "https://api.example.com"}},
		{name: "a flag value before the file", args: []string{"--label", "x", "a.yaml"},
			wantPath: "a.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			plan := fs.Bool("plan", false, "")
			quiet := fs.Bool("quiet", false, "")
			_ = fs.String("label", "", "a flag that takes a value")
			path, err := parseArg(fs, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseArg(%v) = %q, want an error", tc.args, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArg(%v): %v", tc.args, err)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if *plan != tc.wantPlan || *quiet != tc.wantQuiet {
				t.Errorf("flags: plan=%v quiet=%v, want %v/%v", *plan, *quiet, tc.wantPlan, tc.wantQuiet)
			}
		})
	}
}

// TestWorkflowValidateExitStatus covers the property a CI step depends on: a document the API
// would reject at publish exits non-zero, without anyone parsing output. The API compiles; the
// fake answers as it would.
func TestWorkflowValidateExitStatus(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "enrich.yaml")
	if err := os.WriteFile(doc, []byte(applyDoc("enrich")), 0o600); err != nil {
		t.Fatal(err)
	}
	const warned = `{"ok":true,"diagnostics":[{"severity":"warn","code":"after_without_data","message":"serializes two nodes","node":"b"}]}`
	const broken = `{"ok":false,"diagnostics":[{"severity":"error","code":"unknown_node","message":"nodes.missing does not exist","node":"a"}]}`
	tests := []struct {
		name    string
		reply   string
		args    []string
		wantErr bool
	}{
		{"a clean document succeeds", cleanCompile, nil, false},
		{"a warning passes", warned, nil, false},
		{"--strict fails on a warning", warned, []string{"--strict"}, true},
		{"an error fails", broken, nil, true},
	}
	for _, tc := range tests {
		api := newFakeAPI(t, `{}`)
		api.validateReply = tc.reply
		args := append([]string{doc, "--quiet", "--api", api.URL, "--key", "sk"}, tc.args...)
		if err := workflowValidate(args); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.wantErr)
		}
		if api.validations != 1 || api.method != "" {
			t.Errorf("%s: %d validations, and a %s %s; want one validate and nothing else", tc.name, api.validations, api.method, api.path)
		}
	}
	if err := workflowValidate([]string{"./does-not-exist.yaml", "--quiet", "--key", "sk", "--api", "http://127.0.0.1:1"}); err == nil {
		t.Error("a missing file must be an error, not an empty document")
	}
}

// --plan asks the API for the compiled plan and prints it.
func TestWorkflowValidatePrintsThePlan(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "enrich.yaml")
	if err := os.WriteFile(doc, []byte(applyDoc("enrich")), 0o600); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI(t, `{}`)
	api.validateReply = `{"ok":true,"diagnostics":[],"plan":{"name":"enrich","nodes":[{"key":"only"}]}}`
	out, err := capture(t, func() error {
		return workflowValidate([]string{doc, "--plan", "--api", api.URL, "--key", "sk"})
	})
	if err != nil || !api.validatePlan || !strings.Contains(out, `"key": "only"`) {
		t.Errorf("--plan = %v, asked for the plan %v, printed %q", err, api.validatePlan, out)
	}
}
