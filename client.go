package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// otiumctl is a client of the customer API and nothing else: every command
// goes through the same /v1 a portal or a Terraform provider would, with a credential. A person
// signs in with `otiumctl auth login` (a user token, kept in the OS keychain); a CI job supplies
// an API key in OTIUM_TOKEN. No command opens a database.

// apiTimeout bounds one call. An asset publish carries the largest body this API takes, so
// it is generous rather than snappy: a CI step that fails on a slow upload has to be
// re-run, and re-running a publish is only safe because publish is idempotent.
const apiTimeout = 2 * time.Minute

// userAgent names the CLI to the API.
const userAgent = "otiumctl"

// apiClient is a small JSON client for the customer API.
type apiClient struct {
	base string
	key  string
	http *http.Client
	// expiresAt and renew are set for a person's sign-in: the access token lasts an hour, and the
	// client renews it with the refresh token before it lapses, or once after a 401, so a command
	// never fails because an hour went by.
	expiresAt time.Time
	renew     func(ctx context.Context, failed string) (secrets, error)
}

// renewSkew renews an access token this long before it lapses, so it does not expire in flight.
const renewSkew = time.Minute

// newAPIClient is a client for base authenticated by token ("" sends no credential, for the
// sign-in routes).
func newAPIClient(base, token string) *apiClient {
	return &apiClient{base: strings.TrimSuffix(base, "/"), key: token, http: &http.Client{Timeout: apiTimeout}}
}

// apiFlags registers the connection flags shared by every API command. The server is
// --hostname (or OTIUM_API_URL, or the one signed in to); the credential is the token stored
// for it, unless OTIUM_TOKEN or --key says otherwise. --api is the older name for --hostname.
func apiFlags(fs *flag.FlagSet) func() (*apiClient, error) {
	hostname := fs.String("hostname", "", "the Otium API, e.g. api.getotium.ai (default: the one signed in to, or OTIUM_API_URL)")
	api := fs.String("api", "", "the Otium API base URL (older name for --hostname)")
	key := fs.String("key", "", "an API key or token to use instead of the stored sign-in (or set OTIUM_TOKEN)")
	return func() (*apiClient, error) {
		store, err := openHosts()
		if err != nil {
			return nil, err
		}
		c, err := store.resolve(firstNonEmpty(*hostname, *api), *key)
		if err != nil {
			return nil, err
		}
		return clientFor(store, c), nil
	}
}

// clientFor is a client for a resolved credential, renewing it when it is a stored sign-in.
func clientFor(store *hostStore, c credential) *apiClient {
	client := newAPIClient(c.URL, c.Token)
	if c.Refresh != "" && (c.Where == inKeyring || c.Where == inFile) {
		client.expiresAt = c.ExpiresAt
		client.renew = renewer(store, c.Name, c.URL)
	}
	return client
}

// renewer renews a stored sign-in. It re-reads the store first: another otiumctl process may have
// renewed it already, and its refresh token is then the only current one.
func renewer(store *hostStore, name, base string) func(context.Context, string) (secrets, error) {
	return func(ctx context.Context, failed string) (secrets, error) {
		cur, err := store.stored(name)
		if err != nil {
			return secrets{}, err
		}
		if cur.Token != failed && time.Until(cur.ExpiresAt) > renewSkew {
			return secrets{Access: cur.Token, Refresh: cur.Refresh, ExpiresAt: cur.ExpiresAt}, nil
		}
		var resp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		err = newAPIClient(base, "").do(ctx, http.MethodPost, "/v1/auth/token/refresh",
			map[string]string{"refresh_token": cur.Refresh}, &resp)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return secrets{}, fmt.Errorf("%w: your sign-in to %s has ended; run otiumctl auth login --hostname %s",
				errNotSignedIn, name, name)
		}
		if err != nil {
			return secrets{}, fmt.Errorf("renew sign-in: %w", err)
		}
		sec := secrets{
			Access: resp.AccessToken, Refresh: resp.RefreshToken,
			ExpiresAt: time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		}
		if err := store.renewed(name, sec); err != nil {
			return secrets{}, fmt.Errorf("save renewed sign-in: %w", err)
		}
		return sec, nil
	}
}

// fresh renews the access token first when it is about to lapse.
func (c *apiClient) fresh(ctx context.Context) error {
	if c.renew == nil || c.expiresAt.IsZero() || time.Until(c.expiresAt) > renewSkew {
		return nil
	}
	return c.renewNow(ctx)
}

func (c *apiClient) renewNow(ctx context.Context) error {
	sec, err := c.renew(ctx, c.key)
	if err != nil {
		return err
	}
	c.key, c.expiresAt = sec.Access, sec.ExpiresAt
	return nil
}

// send performs a request built by build, renewing a sign-in before it lapses and once more if
// the API still says 401 (clock skew, or a sign-in renewed elsewhere). build is called again for
// the retry, so a body is never reused half-read.
func (c *apiClient) send(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	if err := c.fresh(ctx); err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		c.authorize(req)
		resp, err := c.http.Do(req) //nolint:gosec // the user's own choice of server; see do
		if err != nil || resp.StatusCode != http.StatusUnauthorized || c.renew == nil || attempt > 0 {
			return resp, err
		}
		_ = resp.Body.Close()
		if err := c.renewNow(ctx); err != nil {
			return nil, err
		}
	}
}

// apiError is a non-2xx response, carrying the status so a caller can tell a refusal from
// a fault without matching on message text.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("API returned %d", e.Status)
	}
	return e.Message
}

// do performs one request. A nil body sends none; a nil out discards the response.
func (c *apiClient) do(ctx context.Context, method, path string, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	resp, err := c.send(ctx, func() (*http.Request, error) {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(encoded)
		}
		// gosec G704: the base URL comes from --hostname, OTIUM_API_URL or the sign-in, which is
		// the user's own choice of which server to talk to. SSRF is a SERVER fetching a URL an
		// untrusted party supplied; here the process and the URL's author are the same person
		// with the same network access. This is what `curl` does.
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader) //nolint:gosec
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	})
	if err != nil {
		if errors.Is(err, errNotSignedIn) {
			return err
		}
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the whole body before branching: an error body is small, and a success body has
	// to be decoded anyway. Streaming would only matter for the output route, which has its
	// own path.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Status: resp.StatusCode, Message: errorMessage(payload, resp.StatusCode)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// authorize sets the credential, when there is one, and names the client.
func (c *apiClient) authorize(req *http.Request) {
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	req.Header.Set("User-Agent", userAgent)
}

// stream writes a response body straight to w, for the routes that return JSONL rather
// than a document.
func (c *apiClient) stream(ctx context.Context, path string, w io.Writer) error {
	resp, err := c.send(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil) //nolint:gosec // the user's chosen host
	})
	if err != nil {
		if errors.Is(err, errNotSignedIn) {
			return err
		}
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return &apiError{Status: resp.StatusCode, Message: errorMessage(payload, resp.StatusCode)}
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("stream %s: %w", path, err)
	}
	return nil
}

// errorMessage pulls the API's own message out of an error body.
//
// The API answers `{"error": "…"}` and those messages are written to be actionable — they
// name the missing asset, or say which live runs block a purge. Replacing one with a
// generic "request failed" would throw away the only part of the response worth reading.
func errorMessage(payload []byte, status int) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Error != "" {
		return envelope.Error
	}
	if trimmed := strings.TrimSpace(string(payload)); trimmed != "" && len(trimmed) < 512 {
		return trimmed
	}
	return fmt.Sprintf("API returned %d", status)
}

// marshalCompact renders a value on one line, for a preview.
func marshalCompact(v any) (string, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// printJSON writes a value as indented JSON, for --json output and for the verbs whose
// answer IS a document.
func printJSON(v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
