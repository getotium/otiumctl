// Command otiumctl is Otium's command-line client: a customer's way to the customer API from a
// terminal, a script or CI. Sign in with `otiumctl auth login`, which opens
// the portal to approve the sign-in and keeps a token in the system keychain; or give an API key
// in OTIUM_TOKEN for unattended use.
//
// Every command calls the documented Otium API with that credential, and nothing else:
// `workflow validate` and `workflow test` ask the API to compile and test a workflow, and store
// nothing.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
)

// version is the release, set at build time (-ldflags "-X main.version=v1.2.3"). A `go install`
// build has none, and reports the module version Go recorded instead.
var version = ""

// versionString is what `otiumctl version` prints.
func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "otiumctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// The grouped verbs dispatch their own subcommand and print their own usage.
	if len(args) == 0 {
		usage()
		return errors.New("unknown command")
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("otiumctl", versionString())
		return nil
	case "auth":
		return authCommand(args[1:])
	case "api-keys":
		return apiKeysCommand(args[1:])
	case "assets", "run", "run-set", "webhooks", "tools":
	default:
		if len(args) < 2 {
			usage()
			return errors.New("unknown command")
		}
	}
	switch {
	case args[0] == "workflow" && args[1] == "validate":
		return workflowValidate(args[2:])
	case args[0] == "workflow" && args[1] == "apply":
		return workflowApply(args[2:])
	case args[0] == "workflow" && args[1] == "test":
		return workflowTest(args[2:])
	case args[0] == "assets":
		return assetsCommand(args[1:])
	case args[0] == "run":
		return runCommand(args[1:])
	case args[0] == "run-set":
		return runSetCommand(args[1:])
	case args[0] == "webhooks":
		return webhooksCommand(args[1:])
	case args[0] == "tools":
		return toolsCommand(args[1:])
	default:
		usage()
		return errors.New("unknown command")
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  otiumctl auth login --hostname <api host> [--with-token] [--no-browser]")
	fmt.Fprintln(os.Stderr, "    (approve the sign-in in the portal; the token is kept in the system keychain)")
	fmt.Fprintln(os.Stderr, "  otiumctl auth status | logout | token [--hostname <api host>]")
	fmt.Fprintln(os.Stderr, "  otiumctl version")
	fmt.Fprintln(os.Stderr, "  otiumctl api-keys list")
	fmt.Fprintln(os.Stderr, "  otiumctl api-keys create --name <name> [--expires-days <n>]")
	fmt.Fprintln(os.Stderr, "    (prints the secret ONCE; needs you signed in — an API key cannot mint keys)")
	fmt.Fprintln(os.Stderr, "  otiumctl api-keys revoke <id>")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  checks that store nothing:")
	fmt.Fprintln(os.Stderr, "  otiumctl workflow validate <file|-> [--plan|--mermaid] [--strict] [--quiet]")
	fmt.Fprintln(os.Stderr, "    (compiles a workflow graph as a publish would;")
	fmt.Fprintln(os.Stderr, "     exits non-zero when the document would be rejected at publish)")
	fmt.Fprintln(os.Stderr, "  otiumctl workflow test [file|dir]... [--run <substring>] [--snapshots] [--update]")
	fmt.Fprintln(os.Stderr, "                         [--verbose] [--json]")
	fmt.Fprintln(os.Stderr, "    (runs *.test.yaml unit tests: the real engine over canned node outputs,")
	fmt.Fprintln(os.Stderr, "     with no GPU and nothing stored; exits non-zero when a case fails)")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  the verbs below use your sign-in; in CI set OTIUM_API_URL and OTIUM_TOKEN (an API key),")
	fmt.Fprintln(os.Stderr, "  or pass --hostname and --key")
	fmt.Fprintln(os.Stderr, "  otiumctl assets publish --bundle <name> [--comment <s>] <file|->")
	fmt.Fprintln(os.Stderr, "    (JSON or YAML: a string is one shared value, a map is one value per key)")
	fmt.Fprintln(os.Stderr, "  otiumctl assets list")
	fmt.Fprintln(os.Stderr, "  otiumctl assets versions <bundle>")
	fmt.Fprintln(os.Stderr, "  otiumctl assets get <bundle> [version]")
	fmt.Fprintln(os.Stderr, "  otiumctl assets deprecate|purge <bundle> <version>")
	fmt.Fprintln(os.Stderr, "  otiumctl run get <run-id>")
	fmt.Fprintln(os.Stderr, "  otiumctl run attempts <run-id> <node-key> [--full]")
	fmt.Fprintln(os.Stderr, "  otiumctl run continue <run-id>")
	fmt.Fprintln(os.Stderr, "  otiumctl run-set get|continue <set-id>")
	fmt.Fprintln(os.Stderr, "  otiumctl run-set rerun <set-id> --from step[,step] [--version n] [--dry-run]")
	fmt.Fprintln(os.Stderr, "  otiumctl run-set set-run <set-id>")
	fmt.Fprintln(os.Stderr, "    (the run of the set-scope nodes, which run once the items have finished)")
	fmt.Fprintln(os.Stderr, "  otiumctl run-set output <set-id> [--out <file>]")
	fmt.Fprintln(os.Stderr, "    (the only way to read a continuation's results: it has no batch)")
	fmt.Fprintln(os.Stderr, "  otiumctl run-set node-output <set-id> <node-key> [--out <file>]")
	fmt.Fprintln(os.Stderr, "    (one node's results in stock OpenAI batch-output format — the")
	fmt.Fprintln(os.Stderr, "     migration path: existing collect code reads it unmodified)")
	fmt.Fprintln(os.Stderr, "  otiumctl webhooks list")
	fmt.Fprintln(os.Stderr, "  otiumctl webhooks add --url <https://…> [--events a,b] [--description <s>]")
	fmt.Fprintln(os.Stderr, "  otiumctl webhooks update <id> [--url …] [--events …] [--enable|--disable]")
	fmt.Fprintln(os.Stderr, "  otiumctl webhooks rotate|rm <id>")
	fmt.Fprintln(os.Stderr, "    (add and rotate print the signing secret ONCE; it is stored sealed)")
	fmt.Fprintln(os.Stderr, "  otiumctl webhooks deliveries [id] [--limit n]")
	fmt.Fprintln(os.Stderr, "    (did we send it, and what did your endpoint say)")
	fmt.Fprintln(os.Stderr, "  otiumctl tools list")
	fmt.Fprintln(os.Stderr, "  otiumctl tools register --name <name> --url <https://…> [--description <s>]")
	fmt.Fprintln(os.Stderr, "  otiumctl tools update <id> [--url …] [--description …]")
	fmt.Fprintln(os.Stderr, "  otiumctl tools rotate|rm <id>")
	fmt.Fprintln(os.Stderr, "    (the endpoint a workflow's tool nodes call; register and rotate print")
	fmt.Fprintln(os.Stderr, "     its signing secret ONCE)")
	fmt.Fprintln(os.Stderr, "  otiumctl workflow apply -f <dir|file> [--dry-run] [--strict]")
	fmt.Fprintln(os.Stderr, "                          [--max-critical-path 4h]")
	fmt.Fprintln(os.Stderr, "    (infrastructure-as-code for the graph: idempotent on digest,")
	fmt.Fprintln(os.Stderr, "     no state file, and --max-critical-path is the CI assertion")
	fmt.Fprintln(os.Stderr, "     that a change still fits your deadline window)")
}
