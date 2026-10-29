package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The two-argument case of the same problem, on the verbs where getting it wrong is worst:
// `assets purge tax 3 --api …` leaving --api unparsed would silently talk to localhost, and
// `assets purge --api <url> tax 3` reading the URL as the bundle name would send a destroy
// to whatever bundle that resolved to.
func TestParseArgsHandlesTwoPositionalsAmongTheFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantAPI string
		wantErr bool
	}{
		{name: "flags after both positionals", args: []string{"tax", "3", "--api", "x"},
			want: []string{"tax", "3"}, wantAPI: "x"},
		{name: "flags between positionals", args: []string{"tax", "--json", "3"},
			want: []string{"tax", "3"}},
		{name: "flags before both", args: []string{"--api", "x", "tax", "3"},
			want: []string{"tax", "3"}, wantAPI: "x"},
		{name: "fewer positionals than asked for", args: []string{"tax", "--json"},
			want: []string{"tax"}},
		{name: "none at all", want: []string{}},
		{name: "more positionals than asked for", args: []string{"a", "b", "c"}, wantErr: true},
		// The one a dash-scanner gets wrong: the API url is a flag VALUE, not a bundle.
		{name: "a flag value is never a positional", args: []string{"--api", "https://api.example.com"},
			want: []string{}, wantAPI: "https://api.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			api := fs.String("api", "", "")
			_ = fs.Bool("json", false, "")
			got, err := parseArgs(fs, tc.args, 2)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseArgs(%v, 2) = %v, want an error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%v, 2): %v", tc.args, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseArgs(%v, 2) = %v, want %v", tc.args, got, tc.want)
			}
			if *api != tc.wantAPI {
				t.Errorf("--api = %q, want %q", *api, tc.wantAPI)
			}
		})
	}
}

// The file is the interface a CI job writes against, so its acceptance rules matter more
// than most: a bad one is discovered after the render step, which is the slow part.
func TestReadAssetFileTakesJSONAndYAMLAndRefusesTheRest(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, name, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		return path
	}

	t.Run("yaml and json agree", func(t *testing.T) {
		t.Parallel()
		// YAML is a superset of JSON, so one parser covers both and there is no format
		// flag for a caller to get wrong.
		yamlPath := write(t, "a.yaml", "version: 1.10 LTS\nfield_names:\n  title: name\n")
		jsonPath := write(t, "a.json", `{"version":"1.10 LTS","field_names":{"title":"name"}}`)

		fromYAML, err := readAssetFile(yamlPath)
		if err != nil {
			t.Fatalf("yaml: %v", err)
		}
		fromJSON, err := readAssetFile(jsonPath)
		if err != nil {
			t.Fatalf("json: %v", err)
		}
		if !reflect.DeepEqual(fromYAML, fromJSON) {
			t.Errorf("yaml %#v != json %#v", fromYAML, fromJSON)
		}
		if fromYAML["version"] != "1.10 LTS" {
			t.Errorf("global asset = %#v", fromYAML["version"])
		}
	})

	// An asset is TEXT. YAML would otherwise decide `version: 1.10` is a float, and
	// round-tripping through a number loses the author's own bytes in a value whose entire
	// job is to be interpolated verbatim into a prompt.
	t.Run("a scalar keeps the bytes the author wrote", func(t *testing.T) {
		t.Parallel()
		got, err := readAssetFile(write(t, "a.yaml",
			"trailing_zero: 1.10\nbig: 1e3\nleading_zero: 007\nyes_like: yes\n"))
		if err != nil {
			t.Fatalf("readAssetFile: %v", err)
		}
		for name, want := range map[string]string{
			"trailing_zero": "1.10", // a float round trip would make this "1.1"
			"big":           "1e3",  // and this "1000"
			"leading_zero":  "007",  // and this "7"
			"yes_like":      "yes",  // and YAML 1.1 would make this "true"
		} {
			if got[name] != want {
				t.Errorf("%s = %#v, want %q verbatim", name, got[name], want)
			}
		}
	})

	t.Run("a shape the server would reject is caught first", func(t *testing.T) {
		t.Parallel()
		// Locally, because a CI job that uploads megabytes before learning it wrote a list
		// instead of a map has burned the slow half of the loop for a mistake visible in
		// the first line of the file.
		for _, tc := range []struct{ name, body, want string }{
			{"a list, not a map", "- one\n- two\n", "expected a map"},
			{"a list where a value belongs", "versions:\n  - a\n  - b\n", "must be a string"},
			{"nesting past one level", "fields:\n  title:\n    a: b\n", "one level of key"},
			{"nothing at all", "", "no asset data"},
			{"an empty map", "{}\n", "no assets in the file"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := readAssetFile(write(t, "a.yaml", tc.body))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("readAssetFile = %v, want an error mentioning %q", err, tc.want)
				}
			})
		}
	})
}

// fakeAPI is a customer API stub. It records what the CLI sent, so the tests assert on the
// request as much as on the printed output.
type fakeAPI struct {
	*httptest.Server
	method string
	path   string
	auth   string
	body   []byte
	status int
	reply  string

	// The read-only workflow routes answer on their own, and are not the request a test asserts
	// on (method/path/body stay the last OTHER request: "nothing was published" still means it).
	validateReply string // POST /v1/workflows/validate; "": compiles cleanly, named as the document
	validations   int
	validatePlan  bool   // the last validate asked for the plan
	testReply     string // POST /v1/workflows/test
	testBody      []byte
}

// cleanCompile is a validate's answer for a document that compiles with nothing to say.
const cleanCompile = `{"ok":true,"name":"enrich","digest":"d1","diagnostics":[],
	"summary":{"nodes":1,"depth":1,"critical_path":["only"],"p50_duration_seconds":20,"p95_duration_seconds":30,
	"p95_execution_seconds":30,"est_cost_p50_usd":0.0004,"cost_per_10k_usd":4,"estimate_basis":"inferred","inference_nodes":1}}`

func newFakeAPI(t *testing.T, reply string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{status: http.StatusOK, reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/workflows/validate":
			f.validations++
			f.validatePlan = r.URL.Query().Get("plan") == "true"
			if f.validateReply != "" {
				_, _ = io.WriteString(w, f.validateReply)
				return
			}
			var env documentEnvelope
			_ = json.NewDecoder(r.Body).Decode(&env)
			name := "enrich"
			for _, line := range strings.Split(env.Document, "\n") {
				if v, ok := strings.CutPrefix(line, "name: "); ok {
					name = strings.TrimSpace(v)
					break
				}
			}
			_, _ = io.WriteString(w, strings.Replace(cleanCompile, `"name":"enrich"`, `"name":"`+name+`"`, 1))
			return
		case "/v1/workflows/test":
			f.testBody, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, f.testReply)
			return
		}
		f.method, f.path, f.auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		f.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

// capture runs fn with stdout redirected, returning what it printed.
//
// It swaps a PROCESS global, so every test that uses it runs serially. That is a real
// constraint rather than an oversight: parallel captures interleave, and a test that
// occasionally reads another test's output fails in a way nobody can reproduce.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = saved
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func TestAssetsPublishSendsTheBundleAndReportsWhetherItMintedAVersion(t *testing.T) {
	api := newFakeAPI(t, `{"created":true,"version":{"bundle":"taxonomies","version":3,"assets":2,"keys":1,"bytes":2048}}`)
	path := filepath.Join(t.TempDir(), "a.yaml")
	if err := os.WriteFile(path, []byte("version: 1.10\nfield_names:\n  title: name\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := capture(t, func() error {
		return assetsPublish([]string{path, "--bundle", "taxonomies", "--comment", "build-42",
			"--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/assets" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	if api.auth != "Bearer sk-test" {
		t.Errorf("auth = %q", api.auth)
	}
	var sent struct {
		Bundle  string         `json:"bundle"`
		Comment string         `json:"comment"`
		Assets  map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("decode request: %v (%s)", err, api.body)
	}
	if sent.Bundle != "taxonomies" || sent.Comment != "build-42" {
		t.Errorf("sent = %+v", sent)
	}
	// The value-or-map form survives the CLI unchanged: a string stays a string, a map
	// stays a map, and the server decides what that means.
	if sent.Assets["version"] != "1.10" {
		t.Errorf("global asset = %#v", sent.Assets["version"])
	}
	if _, ok := sent.Assets["field_names"].(map[string]any); !ok {
		t.Errorf("keyed asset = %#v", sent.Assets["field_names"])
	}
	if !strings.Contains(out, "published taxonomies@3") {
		t.Errorf("output = %q", out)
	}

	// "unchanged" is the normal outcome for a scheduled job and must exit 0: the whole
	// point of idempotency is that a nightly publish over an unmoved table is free.
	api.reply = `{"created":false,"version":{"bundle":"taxonomies","version":3,"assets":2,"keys":1,"bytes":2048}}`
	out, err = capture(t, func() error {
		return assetsPublish([]string{path, "--bundle", "taxonomies", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("republish: %v", err)
	}
	if !strings.Contains(out, "unchanged: taxonomies@3") {
		t.Errorf("output = %q", out)
	}
}

// A refused purge is the expected answer while a run is still rendering against the data.
// The server's message names how many runs are in the way, and replacing it with a generic
// failure would throw away the only part worth reading.
func TestAPurgeRefusalKeepsTheServersReason(t *testing.T) {
	api := newFakeAPI(t, `{"error":"assetstore: a live run pins this version: 2 live run set(s) still render against it"}`)
	api.status = http.StatusConflict

	_, err := capture(t, func() error {
		return assetsLifecycle("purge", []string{"taxonomies", "3", "--api", api.URL, "--key", "sk-test"})
	})
	if err == nil {
		t.Fatal("purge succeeded against a 409")
	}
	if !strings.Contains(err.Error(), "refused:") || !strings.Contains(err.Error(), "2 live run set(s)") {
		t.Errorf("error = %q, want the server's reason", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/assets/taxonomies/3/purge" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
}

func TestDeprecateUsesDeleteAndRequiresAVersion(t *testing.T) {
	api := newFakeAPI(t, `{"bundle":"taxonomies","version":3,"status":"deprecated"}`)

	out, err := capture(t, func() error {
		return assetsLifecycle("deprecate", []string{"taxonomies", "3", "--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if api.method != http.MethodDelete || api.path != "/v1/assets/taxonomies/3" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	if !strings.Contains(out, "taxonomies@3 is now deprecated") {
		t.Errorf("output = %q", out)
	}

	// A lifecycle verb without a version is refused rather than defaulted to "latest".
	// Guessing which version to deprecate is not a convenience.
	if _, err := capture(t, func() error {
		return assetsLifecycle("purge", []string{"taxonomies", "--api", api.URL, "--key", "sk-test"})
	}); err == nil {
		t.Error("purge without a version was accepted")
	}
}

func TestAMissingKeyIsRefusedBeforeAnyRequest(t *testing.T) {
	api := newFakeAPI(t, `{}`)
	t.Setenv("OTIUM_API_KEY", "")
	t.Setenv("OTIUM_TOKEN", "")

	if _, err := capture(t, func() error {
		return assetsList([]string{"--api", api.URL})
	}); !errors.Is(err, errNotSignedIn) || !strings.Contains(err.Error(), "otiumctl auth login") {
		t.Errorf("list without a key = %v", err)
	}
	if api.method != "" {
		t.Errorf("an unauthenticated request was sent: %s %s", api.method, api.path)
	}
}

func TestErrorMessageKeepsWhatTheAPISaid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
		status  int
		want    string
	}{
		{"the API envelope", `{"error":"asset bundle \"tax\" does not supply field_catalog"}`, 400,
			`asset bundle "tax" does not supply field_catalog`},
		{"a bare body", "upstream timeout", 502, "upstream timeout"},
		{"nothing useful", "", 500, "API returned 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := errorMessage([]byte(tc.payload), tc.status); got != tc.want {
				t.Errorf("errorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}
