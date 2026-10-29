package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain keeps every test off the real keychain and config directory: openHosts is a store in
// a temporary directory over an in-memory keychain, and no ambient token leaks in.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "otiumctl-test")
	if err != nil {
		panic(err)
	}
	openHosts = func() (*hostStore, error) { return &hostStore{dir: dir, keyring: testKeyring}, nil }
	_ = os.Unsetenv("OTIUM_TOKEN")
	_ = os.Unsetenv("OTIUM_API_KEY")
	_ = os.Unsetenv("OTIUM_API_URL")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeKeyring is an in-memory keychain; broken makes Set fail, as on a box without one.
type fakeKeyring struct {
	m      map[string]string
	broken bool
}

var testKeyring = &fakeKeyring{m: map[string]string{}}

func (k *fakeKeyring) Get(service, user string) (string, error) {
	v, ok := k.m[service+"/"+user]
	if !ok {
		return "", errors.New("not in keychain")
	}
	return v, nil
}

func (k *fakeKeyring) Set(service, user, secret string) error {
	if k.broken {
		return errors.New("no keychain")
	}
	k.m[service+"/"+user] = secret
	return nil
}

func (k *fakeKeyring) Delete(service, user string) error {
	delete(k.m, service+"/"+user)
	return nil
}

// isolatedHosts points openHosts at a fresh store for one test.
func isolatedHosts(t *testing.T, kr keyringAPI) *hostStore {
	t.Helper()
	s := &hostStore{dir: t.TempDir(), keyring: kr}
	saved := openHosts
	openHosts = func() (*hostStore, error) { return s, nil }
	t.Cleanup(func() { openHosts = saved })
	return s
}

// liveAPI is the sign-in half of the Otium API as the CLI meets it: the device grant, token
// renewal, and the account and key routes, answering as the real API does (its OpenAPI document
// describes them), with one account (ops@acme.test). The person's approval in the portal is
// decide, called by approveOnFirstPoll; the clock moves to expire tokens.
type liveAPI struct {
	*httptest.Server
	mu       sync.Mutex
	offset   time.Duration
	decision map[string]string // user code → "approved" | "denied"
	access   map[string]time.Time
	refresh  map[string]string // refresh token → the access token it renews
	keys     []map[string]any
	ended    bool // every sign-in has ended on the server (idle past its limit)
	serial   int
}

const (
	fakeDeviceCode = "dc-1"
	fakeUserCode   = "ABCD-EFGH"
)

func (a *liveAPI) now() time.Time { return time.Now().Add(a.offset) }

// advance moves the server's clock, expiring access tokens.
func (a *liveAPI) advance(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.offset += d
}

// endSignIn ends every sign-in on the server: access tokens stop working and refreshes are
// refused.
func (a *liveAPI) endSignIn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = true
	a.access = map[string]time.Time{}
}

func (a *liveAPI) decide(userCode string, approve bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if userCode != fakeUserCode {
		return fmt.Errorf("no sign-in waits with code %s", userCode)
	}
	a.decision[userCode] = map[bool]string{true: "approved", false: "denied"}[approve]
	return nil
}

func (a *liveAPI) issue() map[string]any {
	a.serial++
	access := fmt.Sprintf("otium_user_%04d", a.serial)
	refresh := fmt.Sprintf("otium_refresh_%04d", a.serial)
	a.access[access] = a.now().Add(time.Hour)
	a.refresh[refresh] = access
	return map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": refresh}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newLiveAPI(t *testing.T) *liveAPI {
	t.Helper()
	a := &liveAPI{decision: map[string]string{}, access: map[string]time.Time{}, refresh: map[string]string{}}
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Close)
	return a
}

func (a *liveAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	route := r.Method + " " + r.URL.Path
	switch route {
	case "POST /v1/auth/device":
		writeJSON(w, http.StatusOK, map[string]any{"device_code": fakeDeviceCode, "user_code": fakeUserCode,
			"verification_uri": "https://portal.test/device", "expires_in": 900, "interval": 5})
		return
	case "POST /v1/auth/device/token":
		switch a.decision[fakeUserCode] {
		case "approved":
			delete(a.decision, fakeUserCode)
			writeJSON(w, http.StatusOK, a.issue())
		case "denied":
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "access_denied", "error_description": "The sign-in was denied."})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending", "error_description": "Waiting for approval in the portal."})
		}
		return
	case "POST /v1/auth/token/refresh":
		old, ok := a.refresh[body["refresh_token"]]
		if !ok || a.ended {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_grant", "error_description": "This sign-in has ended: run otiumctl auth login."})
			return
		}
		delete(a.refresh, body["refresh_token"])
		delete(a.access, old)
		writeJSON(w, http.StatusOK, a.issue())
		return
	}

	// Everything else needs a credential: a live user token, or an API key on a machine route.
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	isKey := strings.HasPrefix(bearer, "sk-otium-")
	knownKey := false
	for _, k := range a.keys {
		if k["secret"] == bearer {
			knownKey = true
		}
	}
	expires, isUser := a.access[bearer]
	switch {
	case isKey && !knownKey, !isKey && (!isUser || !a.now().Before(expires)):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this token was revoked, has not been used in a year, or does not exist: run otiumctl auth login"})
		return
	case isKey && r.URL.Path != "/v1/workflows":
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "an API key cannot manage the account; sign in with otiumctl auth login, or use the portal"})
		return
	}
	switch route {
	case "GET /v1/account":
		writeJSON(w, http.StatusOK, map[string]string{"email": "ops@acme.test", "tenant": "acme"})
	case "GET /v1/workflows":
		writeJSON(w, http.StatusOK, map[string]any{"workflows": []any{}})
	case "DELETE /v1/auth/token":
		delete(a.access, bearer)
		writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
	case "GET /v1/api-keys":
		out := make([]map[string]any, 0, len(a.keys))
		for _, k := range a.keys {
			out = append(out, map[string]any{"id": k["id"], "name": k["name"], "prefix": "sk-otium", "last_four": k["last_four"],
				"created_at": "2026-10-29T12:00:00Z", "revoked": false})
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": out, "count": len(out)})
	case "POST /v1/api-keys":
		secret := fmt.Sprintf("sk-otium-%064x", len(a.keys)+1)
		k := map[string]any{"id": fmt.Sprintf("key_%d", len(a.keys)+1), "name": body["name"], "secret": secret, "last_four": secret[len(secret)-4:]}
		a.keys = append(a.keys, k)
		writeJSON(w, http.StatusCreated, map[string]any{"id": k["id"], "name": k["name"], "prefix": "sk-otium",
			"last_four": k["last_four"], "created_at": "2026-10-29T12:00:00Z", "revoked": false, "secret": secret})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

var userCodePattern = regexp.MustCompile(`one-time code: ([A-Z]{4}-[A-Z]{4})`)

// approveOnFirstPoll stands in for the person: when the CLI first waits to poll, it reads the
// code the CLI printed and approves it, as they would in the portal. approve=false denies.
func approveOnFirstPoll(t *testing.T, api *liveAPI, prompts *bytes.Buffer, approve bool) {
	t.Helper()
	savedSleep, savedOut, savedOpen := sleepFor, authOut, openBrowser
	t.Cleanup(func() { sleepFor, authOut, openBrowser = savedSleep, savedOut, savedOpen })
	authOut = prompts
	openBrowser = func(context.Context, string) error { return errors.New("no browser in tests") }
	decided := false
	sleepFor = func(ctx context.Context, _ time.Duration) error {
		if !decided {
			m := userCodePattern.FindStringSubmatch(prompts.String())
			if m == nil {
				t.Fatalf("the CLI never showed a code: %q", prompts.String())
			}
			if err := api.decide(m[1], approve); err != nil {
				t.Fatalf("decide %s: %v", m[1], err)
			}
			decided = true
		}
		return nil
	}
}

// TestLoginThenMintAKey is what a customer does first: sign in from a terminal, approve it in
// the portal, and mint the key their service runs with, with no operator involved.
func TestLoginThenMintAKey(t *testing.T) {
	api := newLiveAPI(t)
	hosts := isolatedHosts(t, &fakeKeyring{m: map[string]string{}})
	var prompts bytes.Buffer
	approveOnFirstPoll(t, api, &prompts, true)

	if err := authLogin([]string{"--hostname", api.URL}); err != nil {
		t.Fatalf("login: %v\n%s", err, prompts.String())
	}
	if !strings.Contains(prompts.String(), "https://portal.test/device") {
		t.Errorf("the approval page was not shown: %q", prompts.String())
	}
	if !strings.Contains(prompts.String(), "Signed in to") || !strings.Contains(prompts.String(), "ops@acme.test") {
		t.Errorf("login did not say who it signed in as: %q", prompts.String())
	}

	// The token is in the keychain, not on disk.
	name, _, _ := hostName(api.URL)
	c, err := hosts.stored(name)
	if err != nil || c.Where != inKeyring || !isUserToken(c.Token) {
		t.Fatalf("stored = %+v, %v; want a user token in the keychain", c, err)
	}
	if raw, _ := os.ReadFile(hosts.path()); bytes.Contains(raw, []byte(c.Token)) {
		t.Fatal("the token was written to hosts.yaml although a keychain was available")
	}

	// The default server is the one signed in to, so no flags are needed from here on.
	out, err := capture(t, func() error { return apiKeysCreate([]string{"--name", "prod-firehose"}) })
	if err != nil {
		t.Fatalf("api-keys create: %v", err)
	}
	secret := regexp.MustCompile(`sk-otium-[0-9a-f]+`).FindString(out)
	if secret == "" {
		t.Fatalf("no secret printed: %q", out)
	}

	out, err = capture(t, func() error { return apiKeysList(nil) })
	if err != nil || !strings.Contains(out, "prod-firehose") || strings.Contains(out, secret) {
		t.Fatalf("list = %q, %v; want the key, without its secret", out, err)
	}

	// The minted key cannot mint another: a leaked key does not multiply.
	_, err = capture(t, func() error { return apiKeysCreate([]string{"--name", "more", "--key", secret}) })
	if err == nil || !strings.Contains(err.Error(), "otiumctl auth login") {
		t.Fatalf("create with an API key = %v, want a refusal pointing at auth login", err)
	}

	// Signing out revokes the token on the server, not just locally.
	if err := authLogout(nil); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := hosts.stored(name); !errors.Is(err, errNotSignedIn) {
		t.Fatalf("still signed in after logout: %v", err)
	}
	if _, err := whoami(context.Background(), newAPIClient(api.URL, c.Token)); err == nil {
		t.Fatal("the token still works on the server after logout")
	}
}

func TestLoginDenied(t *testing.T) {
	api := newLiveAPI(t)
	hosts := isolatedHosts(t, &fakeKeyring{m: map[string]string{}})
	var prompts bytes.Buffer
	approveOnFirstPoll(t, api, &prompts, false)

	err := authLogin([]string{"--hostname", api.URL})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("login after a denial = %v, want denied", err)
	}
	if f, _ := hosts.load(); len(f.Hosts) != 0 {
		t.Fatalf("a denied sign-in was saved: %+v", f.Hosts)
	}
}

// Without a keychain the token goes to hosts.yaml, readable only by its owner, and status says so.
func TestLoginWithTokenFallsBackToTheFile(t *testing.T) {
	api := newLiveAPI(t)
	hosts := isolatedHosts(t, &fakeKeyring{m: map[string]string{}, broken: true})

	// An API key read from stdin, as an unattended box would: it may not read the account, but
	// it works on the machine routes, so it is accepted and described as a key. It is minted
	// through the API as the person, the only way a customer can.
	var prompts bytes.Buffer
	approveOnFirstPoll(t, api, &prompts, true)
	if err := authLogin([]string{"--hostname", api.URL}); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, func() error { return apiKeysCreate([]string{"--name", "ci"}) })
	if err != nil {
		t.Fatal(err)
	}
	secret := regexp.MustCompile(`sk-otium-[0-9a-f]+`).FindString(out)

	savedIn := stdin
	t.Cleanup(func() { stdin = savedIn })
	stdin = strings.NewReader(secret + "\n")
	prompts.Reset()
	if err := authLogin([]string{"--hostname", api.URL, "--with-token"}); err != nil {
		t.Fatalf("login --with-token: %v", err)
	}
	if !strings.Contains(prompts.String(), "an API key") || !strings.Contains(prompts.String(), "hosts.yaml") {
		t.Errorf("prompts = %q, want it described as a key kept in hosts.yaml", prompts.String())
	}
	name, _, _ := hostName(api.URL)
	c, err := hosts.stored(name)
	if err != nil || c.Where != inFile || c.Token != secret {
		t.Fatalf("stored = %+v, %v; want the key in the file", c, err)
	}
	if info, err := os.Stat(hosts.path()); err == nil && info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("hosts.yaml mode = %v, want 0600", info.Mode().Perm())
	}

	// A token that does not work is refused, not saved.
	stdin = strings.NewReader("sk-otium-0000\n")
	if err := authLogin([]string{"--hostname", api.URL, "--with-token"}); err == nil {
		t.Fatal("a bad token was accepted")
	}
	if c, _ := hosts.stored(name); c.Token != secret {
		t.Fatal("a refused token replaced the working one")
	}
}

func TestResolve(t *testing.T) {
	kr := &fakeKeyring{m: map[string]string{}}
	hosts := isolatedHosts(t, kr)
	if _, err := hosts.signIn("api.example.com", "https://api.example.com", "ops@acme.test", secrets{Access: "otium_user_stored"}, false); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, server, flagToken, env, wantURL, wantToken string
		wantWhere                                        credWhere
		wantErr                                          error
	}{
		{name: "the default server's stored token", wantURL: "https://api.example.com", wantToken: "otium_user_stored", wantWhere: inKeyring},
		{name: "OTIUM_TOKEN beats the stored one", env: "sk-otium-env", wantURL: "https://api.example.com", wantToken: "sk-otium-env", wantWhere: inEnv},
		{name: "--key beats both", env: "sk-otium-env", flagToken: "sk-otium-flag", wantURL: "https://api.example.com", wantToken: "sk-otium-flag", wantWhere: inFlag},
		{name: "a server not signed in to", server: "other.example.com", wantErr: errNotSignedIn},
		{name: "a bare host means https", server: "other.example.com", env: "sk-otium-env", wantURL: "https://other.example.com", wantToken: "sk-otium-env", wantWhere: inEnv},
		{name: "a local URL keeps its scheme", server: "http://127.0.0.1:8088", flagToken: "sk-otium-flag", wantURL: "http://127.0.0.1:8088", wantToken: "sk-otium-flag", wantWhere: inFlag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTIUM_TOKEN", tt.env)
			c, err := hosts.resolve(tt.server, tt.flagToken)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.URL != tt.wantURL || c.Token != tt.wantToken || c.Where != tt.wantWhere {
				t.Fatalf("resolve = %+v, want url=%s token=%s where=%s", c, tt.wantURL, tt.wantToken, tt.wantWhere)
			}
		})
	}

	t.Run("signing out moves the default and clears the keychain", func(t *testing.T) {
		if _, err := hosts.signIn("b.example.com", "https://b.example.com", "x", secrets{Access: "otium_user_b"}, false); err != nil {
			t.Fatal(err)
		}
		if err := hosts.signOut("api.example.com"); err != nil {
			t.Fatal(err)
		}
		f, _ := hosts.load()
		if f.Default != "b.example.com" {
			t.Errorf("default = %q, want b.example.com", f.Default)
		}
		if _, err := kr.Get(keyringService("api.example.com"), "api.example.com"); err == nil {
			t.Error("the signed-out token is still in the keychain")
		}
	})

	t.Run("no server at all", func(t *testing.T) {
		empty := isolatedHosts(t, &fakeKeyring{m: map[string]string{}})
		if _, err := empty.resolve("", ""); !errors.Is(err, errNotSignedIn) {
			t.Fatalf("err = %v, want errNotSignedIn", err)
		}
	})
}

// The sign-in address comes from the server; only an http(s) URL may reach the OS opener, which
// would otherwise run a local file or another scheme a hostile server named.
func TestOpenURLRefusesAnythingButHTTP(t *testing.T) {
	for _, link := range []string{`C:\Windows\System32\calc.exe`, "file:///etc/passwd", "javascript:alert(1)", "ms-settings:", "https://", ""} {
		if err := openURL(context.Background(), link); err == nil {
			t.Errorf("openURL(%q) was allowed", link)
		}
	}
}

// The access token lasts an hour, so a sign-in must renew without the person noticing: before it
// lapses when the CLI knows the expiry, and after a 401 when the server disagrees.
func TestASignInRenewsItself(t *testing.T) {
	api := newLiveAPI(t)
	hosts := isolatedHosts(t, &fakeKeyring{m: map[string]string{}})
	var prompts bytes.Buffer
	approveOnFirstPoll(t, api, &prompts, true)
	if err := authLogin([]string{"--hostname", api.URL}); err != nil {
		t.Fatalf("login: %v", err)
	}
	name, _, _ := hostName(api.URL)
	first, err := hosts.stored(name)
	if err != nil || first.Refresh == "" || time.Until(first.ExpiresAt) < 50*time.Minute {
		t.Fatalf("stored = %+v, %v; want a refresh token and an hour", first, err)
	}

	// The server's clock moves past the hour; the CLI still believes its token is good. The 401 is
	// answered by one renewal and a retry, and the command succeeds.
	api.advance(2 * time.Hour)
	if _, err := capture(t, func() error { return apiKeysList(nil) }); err != nil {
		t.Fatalf("list after the access token lapsed server-side: %v", err)
	}
	second, _ := hosts.stored(name)
	if second.Token == first.Token || second.Refresh == first.Refresh {
		t.Fatal("the renewed pair was not saved")
	}

	// Now the CLI knows the token is about to lapse, and renews before sending anything.
	second.ExpiresAt = time.Now().Add(10 * time.Second)
	if err := hosts.renewed(name, secrets{Access: second.Token, Refresh: second.Refresh, ExpiresAt: second.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, func() error { return authToken(nil) })
	if err != nil {
		t.Fatalf("auth token: %v", err)
	}
	third, _ := hosts.stored(name)
	if third.Token == second.Token || strings.TrimSpace(out) != third.Token {
		t.Fatalf("auth token printed %q; want the renewed %q", strings.TrimSpace(out), third.Token)
	}

	// The sign-in ends on the server (idle past its limit, say). The CLI says to sign in again.
	api.endSignIn()
	_, err = capture(t, func() error { return apiKeysList(nil) })
	if !errors.Is(err, errNotSignedIn) || !strings.Contains(err.Error(), "otiumctl auth login") {
		t.Fatalf("list after the sign-in ended = %v, want a pointer to auth login", err)
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []secrets{
		{Access: "sk-otium-abc"}, // an API key stays a bare string in the keychain
		{Access: "otium_user_a", Refresh: "otium_refresh_r", ExpiresAt: at},
	}
	for _, in := range tests {
		enc := in.encode()
		if in.Refresh == "" && enc != in.Access {
			t.Errorf("an API key was encoded as %q", enc)
		}
		if got := decodeSecrets(enc); got.Access != in.Access || got.Refresh != in.Refresh || !got.ExpiresAt.Equal(in.ExpiresAt) {
			t.Errorf("decode(encode(%+v)) = %+v", in, got)
		}
	}
}
