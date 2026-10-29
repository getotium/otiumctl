package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// The asset-bundle verbs.
//
// The split here is that the WORKFLOW is an infrastructure-as-code resource and the
// asset bundle is a CI JOB — a field catalog lives in a production table and changes when
// the business changes, so managing it as IaC means a state file that is permanently and
// correctly drifted. These verbs are the pipeline step that split implies: render the
// taxonomy from the source of truth, pipe it in, and let idempotency decide whether that
// is a new version.
//
//	otiumctl assets publish --bundle <name> [--comment <s>] <file|->
//	otiumctl assets list
//	otiumctl assets versions <bundle>
//	otiumctl assets get <bundle> [version]
//	otiumctl assets deprecate <bundle> <version>
//	otiumctl assets purge <bundle> <version>

// maxAssetFile bounds what the CLI will read before the server gets a chance to refuse it.
const maxAssetFile = 64 << 20

// assetVersion mirrors the API's version projection. It is restated here rather than
// imported from pkg/customerapi because that package is an HTTP server: importing it into
// a CLI would drag gin, a billing service and a Stripe client into a binary that makes one
// JSON call.
type assetVersion struct {
	ID          string `json:"id"`
	Bundle      string `json:"bundle"`
	Version     int    `json:"version"`
	Status      string `json:"status"`
	Digest      string `json:"digest"`
	Bytes       int64  `json:"bytes"`
	Assets      int    `json:"assets"`
	Keys        int    `json:"keys"`
	Comment     string `json:"comment,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	PurgedAt    string `json:"purged_at,omitempty"`
}

func assetsCommand(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("assets needs a verb")
	}
	switch args[0] {
	case "publish":
		return assetsPublish(args[1:])
	case "list":
		return assetsList(args[1:])
	case "versions":
		return assetsVersions(args[1:])
	case "get":
		return assetsGet(args[1:])
	case "deprecate", "purge":
		return assetsLifecycle(args[0], args[1:])
	default:
		usage()
		return fmt.Errorf("unknown assets verb %q", args[0])
	}
}

// assetsPublish uploads a bundle. The file is JSON or YAML in the same value-or-map form
// the API takes: a string is one shared value, an object is one value per key.
func assetsPublish(args []string) error {
	fs := flag.NewFlagSet("assets publish", flag.ContinueOnError)
	bundle := fs.String("bundle", "", "bundle name")
	comment := fs.String("comment", "", "note recorded on the version (e.g. a build id)")
	asJSON := fs.Bool("json", false, "print the version as JSON")
	client := apiFlags(fs)
	path, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if *bundle == "" {
		return errors.New("--bundle is required")
	}
	c, err := client()
	if err != nil {
		return err
	}

	assets, err := readAssetFile(path)
	if err != nil {
		return err
	}

	var out struct {
		Created bool         `json:"created"`
		Version assetVersion `json:"version"`
	}
	body := map[string]any{"bundle": *bundle, "comment": *comment, "assets": assets}
	if err := c.do(context.Background(), http.MethodPost, "/v1/assets", body, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	// "unchanged" is the normal outcome for a scheduled job, not a warning: the whole point
	// of idempotency is that a nightly publish over an unchanged table costs a version
	// number of nothing. Exit 0 either way, and say which happened.
	if !out.Created {
		fmt.Printf("unchanged: %s@%d (%d assets, %d keys, %s)\n",
			out.Version.Bundle, out.Version.Version, out.Version.Assets, out.Version.Keys,
			humanBytes(out.Version.Bytes))
		return nil
	}
	fmt.Printf("published %s@%d (%d assets, %d keys, %s)\n",
		out.Version.Bundle, out.Version.Version, out.Version.Assets, out.Version.Keys,
		humanBytes(out.Version.Bytes))
	return nil
}

func assetsList(args []string) error {
	fs := flag.NewFlagSet("assets list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out struct {
		Bundles []struct {
			Bundle        string `json:"bundle"`
			LatestVersion int    `json:"latest_version"`
			Status        string `json:"status"`
			Versions      int    `json:"versions"`
			Bytes         int64  `json:"bytes"`
			UpdatedAt     string `json:"updated_at"`
		} `json:"bundles"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/v1/assets", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out.Bundles)
	}
	if len(out.Bundles) == 0 {
		fmt.Println("no asset bundles")
		return nil
	}
	fmt.Printf("%-28s %8s %12s %9s  %s\n", "BUNDLE", "LATEST", "STATUS", "SIZE", "UPDATED")
	for _, b := range out.Bundles {
		fmt.Printf("%-28s %8d %12s %9s  %s\n",
			b.Bundle, b.LatestVersion, b.Status, humanBytes(b.Bytes), b.UpdatedAt)
	}
	return nil
}

func assetsVersions(args []string) error {
	fs := flag.NewFlagSet("assets versions", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	bundle, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if bundle == "" {
		return errors.New("usage: otiumctl assets versions <bundle>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out struct {
		Versions []assetVersion `json:"versions"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/v1/assets/"+bundle, nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out.Versions)
	}
	fmt.Printf("%7s %12s %9s %7s %6s  %s\n", "VERSION", "STATUS", "SIZE", "ASSETS", "KEYS", "PUBLISHED")
	for _, v := range out.Versions {
		when := v.PublishedAt
		if v.PurgedAt != "" {
			when = "purged " + v.PurgedAt
		}
		fmt.Printf("%7d %12s %9s %7d %6d  %s\n",
			v.Version, v.Status, humanBytes(v.Bytes), v.Assets, v.Keys, when)
	}
	return nil
}

// assetsGet prints one version's content, which is the shape it was published in — so it
// round-trips straight back into `assets publish`.
func assetsGet(args []string) error {
	fs := flag.NewFlagSet("assets get", flag.ContinueOnError)
	client := apiFlags(fs)
	bundle, version, err := parseBundleRef(fs, args)
	if err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	path := "/v1/assets/" + bundle + "/" + strconv.Itoa(version)
	if version == 0 {
		// The API's detail route needs an explicit version, so resolve "latest" through the
		// versions listing rather than inventing a second server-side meaning for 0.
		var list struct {
			Versions []assetVersion `json:"versions"`
		}
		if err := c.do(context.Background(), http.MethodGet, "/v1/assets/"+bundle, nil, &list); err != nil {
			return err
		}
		latest := 0
		for _, v := range list.Versions {
			if v.Status == "published" && v.Version > latest {
				latest = v.Version
			}
		}
		if latest == 0 {
			return fmt.Errorf("bundle %q has no published version", bundle)
		}
		path = "/v1/assets/" + bundle + "/" + strconv.Itoa(latest)
	}
	var out struct {
		Assets map[string]json.RawMessage `json:"assets"`
	}
	if err := c.do(context.Background(), http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	return printJSON(out.Assets)
}

func assetsLifecycle(verb string, args []string) error {
	fs := flag.NewFlagSet("assets "+verb, flag.ContinueOnError)
	client := apiFlags(fs)
	bundle, version, err := parseBundleRef(fs, args)
	if err != nil {
		return err
	}
	if version <= 0 {
		return fmt.Errorf("usage: otiumctl assets %s <bundle> <version>", verb)
	}
	c, err := client()
	if err != nil {
		return err
	}

	var out assetVersion
	path := "/v1/assets/" + bundle + "/" + strconv.Itoa(version)
	method, suffix := http.MethodDelete, ""
	if verb == "purge" {
		method, suffix = http.MethodPost, "/purge"
	}
	if err := c.do(context.Background(), method, path+suffix, nil, &out); err != nil {
		// A refused purge is the expected answer while a run is still rendering against
		// the data, not a malfunction. Say so plainly and keep the server's reason, which
		// names how many runs are in the way.
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return fmt.Errorf("refused: %s", apiErr.Message)
		}
		return err
	}
	fmt.Printf("%s@%d is now %s\n", out.Bundle, out.Version, out.Status)
	return nil
}

// readAssetFile reads a bundle from a path or stdin, accepting JSON or YAML.
//
// YAML because a hand-maintained taxonomy is far more pleasant to write and review in it,
// and JSON because a CI step rendering one out of a database will emit that. YAML is a
// superset of JSON, so one parser covers both and there is no format flag to get wrong.
func readAssetFile(path string) (map[string]any, error) {
	var (
		src []byte
		err error
	)
	if path == "" || path == "-" {
		src, err = io.ReadAll(io.LimitReader(os.Stdin, maxAssetFile+1))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	} else {
		// gosec G304: reading a caller-named file IS this command. otiumctl runs as the
		// operator, with the operator's own file access, so there is no boundary to cross.
		src, err = os.ReadFile(path) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	if len(src) == 0 {
		return nil, errors.New("no asset data: pass a file or pipe one in")
	}
	return parseAssetDocument(src)
}

// parseAssetDocument turns a YAML or JSON bundle into the wire shape.
//
// It reads the document as a NODE TREE rather than unmarshalling into Go values, for one
// reason: an asset is text, and YAML would otherwise decide that `version: 1.10` is a
// float. Round-tripping that through a number loses the author's own bytes — `1.10`
// becomes `1.1` — in a value whose entire job is to be interpolated verbatim into a
// prompt. The node keeps the source text, so what a customer wrote is what gets published.
//
// It also checks the shape here, not only server-side: a CI job that uploads megabytes
// before learning it wrote a list instead of a map has burned the slow half of its loop
// for a mistake visible in the first line of the file. The errors carry line numbers for
// the same reason.
func parseAssetDocument(src []byte) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parse assets: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("no assets in the file: expected a map of name → value")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: expected a map of name → value at the top level", root.Line)
	}
	if len(root.Content) == 0 {
		return nil, errors.New("no assets in the file: expected a map of name → value")
	}

	out := make(map[string]any, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		name, value := root.Content[i], root.Content[i+1]
		if name.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: asset names must be plain strings", name.Line)
		}
		switch value.Kind {
		case yaml.ScalarNode:
			out[name.Value] = value.Value
		case yaml.MappingNode:
			byKey := make(map[string]string, len(value.Content)/2)
			for j := 0; j+1 < len(value.Content); j += 2 {
				k, v := value.Content[j], value.Content[j+1]
				if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode {
					// One level of indexing, by a declared key — no nesting, no joins.
					// That is the rule for asset values, and refusing here says so where the author
					// can see it rather than as a type error from the server.
					return nil, fmt.Errorf("line %d: asset %q takes one level of key → value; "+
						"pre-render anything deeper at publish time", v.Line, name.Value)
				}
				byKey[k.Value] = v.Value
			}
			out[name.Value] = byKey
		default:
			return nil, fmt.Errorf("line %d: asset %q must be a string (one shared value) "+
				"or a map of key → value", value.Line, name.Value)
		}
	}
	return out, nil
}

// parseBundleRef parses `<bundle> [version]` alongside the flag set's own flags.
func parseBundleRef(fs *flag.FlagSet, args []string) (bundle string, version int, err error) {
	positional, err := parseArgs(fs, args, 2)
	if err != nil {
		return "", 0, err
	}
	if len(positional) == 0 {
		return "", 0, errors.New("a bundle name is required")
	}
	bundle = positional[0]
	if len(positional) > 1 {
		v, convErr := strconv.Atoi(positional[1])
		if convErr != nil || v <= 0 {
			return "", 0, fmt.Errorf("version %q must be a positive integer", positional[1])
		}
		version = v
	}
	return bundle, version, nil
}

// humanBytes renders a size for a table without pulling in a dependency.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(int64(1)<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(int64(1)<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
