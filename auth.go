package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// `otiumctl auth` signs in to the customer API the way `gh auth` signs in to GitHub
//: the OAuth device grant, approved by the person in the portal, yields a
// user token that acts as them; it is kept in the OS keychain.

// The seams tests replace: where prompts go, how the browser opens, how the CLI waits between
// polls, and what it reads a pasted token from.
var (
	authOut     io.Writer = os.Stderr
	openBrowser           = openURL
	sleepFor              = sleepCtx
	stdin       io.Reader = os.Stdin
)

func authCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: otiumctl auth login | status | logout | token")
	}
	switch args[0] {
	case "login":
		return authLogin(args[1:])
	case "status":
		return authStatus(args[1:])
	case "logout":
		return authLogout(args[1:])
	case "token":
		return authToken(args[1:])
	default:
		return fmt.Errorf("unknown auth command %q: login, status, logout or token", args[0])
	}
}

func authLogin(args []string) error {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	hostname := fs.String("hostname", "", "the Otium API to sign in to (default: OTIUM_API_URL, the one signed in to, or "+defaultHost+")")
	withToken := fs.Bool("with-token", false, "read a token or API key from standard input instead of signing in in the browser")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in address instead of opening a browser")
	insecure := fs.Bool("insecure-storage", false, "keep the token in hosts.yaml instead of the system keychain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openHosts()
	if err != nil {
		return err
	}
	server := firstNonEmpty(*hostname, os.Getenv("OTIUM_API_URL"))
	if server == "" {
		f, err := store.load()
		if err != nil {
			return err
		}
		server = f.Default
	}
	if server == "" {
		server = defaultHost
	}
	name, base, err := hostName(server)
	if err != nil {
		return err
	}
	ctx := context.Background()

	var sec secrets
	if *withToken {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if sec.Access = strings.TrimSpace(line); sec.Access == "" {
			return errors.New("no token on standard input")
		}
	} else if sec, err = deviceSignIn(ctx, base, !*noBrowser); err != nil {
		return err
	}

	user, err := whoami(ctx, newAPIClient(base, sec.Access))
	if err != nil {
		return fmt.Errorf("the token does not work on %s: %w", name, err)
	}
	where, err := store.signIn(name, base, user, sec, *insecure)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(authOut, "✓ Signed in to %s as %s\n", name, user)
	if where == inFile {
		_, _ = fmt.Fprintf(authOut, "! The token is in %s (no system keychain was available)\n", store.path())
	}
	return nil
}

// deviceSignIn runs the OAuth device grant (RFC 8628): it asks for a code, shows it, and polls
// until the person approves it in the portal. It returns the short-lived access token and the
// refresh token that renews it.
func deviceSignIn(ctx context.Context, base string, browser bool) (secrets, error) {
	c := newAPIClient(base, "")
	device, _ := os.Hostname()
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/device",
		map[string]string{"device_name": "otiumctl on " + firstNonEmpty(device, "this computer")}, &start); err != nil {
		return secrets{}, fmt.Errorf("start sign-in: %w", err)
	}
	// The code is never put in the link: typing it is what proves the sign-in is yours.
	_, _ = fmt.Fprintf(authOut, "! First copy your one-time code: %s\n", start.UserCode)
	if browser && openBrowser(ctx, start.VerificationURI) == nil {
		_, _ = fmt.Fprintf(authOut, "Opened %s in your browser; enter the code there.\n", start.VerificationURI)
	} else {
		_, _ = fmt.Fprintf(authOut, "Open %s in a browser and enter the code.\n", start.VerificationURI)
	}

	interval := time.Duration(max(start.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for {
		if err := sleepFor(ctx, interval); err != nil {
			return secrets{}, err
		}
		var tok struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		err := c.do(ctx, http.MethodPost, "/v1/auth/device/token", map[string]string{"device_code": start.DeviceCode}, &tok)
		if err == nil {
			return secrets{
				Access: tok.AccessToken, Refresh: tok.RefreshToken,
				ExpiresAt: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
			}, nil
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			return secrets{}, fmt.Errorf("waiting for approval: %w", err)
		}
		switch ae.Message {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second // RFC 8628 §3.5
		case "access_denied":
			return secrets{}, errors.New("the sign-in was denied in the portal")
		case "expired_token":
			return secrets{}, errors.New("the code expired before it was approved: run otiumctl auth login again")
		default:
			return secrets{}, fmt.Errorf("sign-in failed: %s", ae.Message)
		}
		if !time.Now().Before(deadline) {
			return secrets{}, errors.New("the code expired before it was approved: run otiumctl auth login again")
		}
	}
}

// whoami describes a credential: the account's email for a user token. An API key may not read
// the account (keys are confined to the machine routes), so a key that is refused there but
// accepted on a machine route is reported as one.
func whoami(ctx context.Context, c *apiClient) (string, error) {
	var acct struct {
		Email  string `json:"email"`
		Tenant string `json:"tenant"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/account", nil, &acct)
	if err == nil {
		return firstNonEmpty(acct.Email, acct.Tenant), nil
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		return "", err
	}
	// Any answer but 401 from a machine route means the key authenticated: a 503 there is a
	// feature the deployment has not wired, not a bad credential.
	err = c.do(ctx, http.MethodGet, "/v1/workflows", nil, nil)
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		return "", err
	}
	var netErr *apiError
	if err != nil && !errors.As(err, &netErr) {
		return "", err
	}
	return "an API key", nil
}

func authStatus(args []string) error {
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	hostname := fs.String("hostname", "", "show only this server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openHosts()
	if err != nil {
		return err
	}
	f, err := store.load()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(f.Hosts))
	for n := range f.Hosts {
		names = append(names, n)
	}
	if *hostname != "" {
		n, _, err := hostName(*hostname)
		if err != nil {
			return err
		}
		names = []string{n}
	}
	sort.Strings(names)
	if t := envToken(); t != "" {
		fmt.Printf("OTIUM_TOKEN is set: commands use it instead of a stored sign-in\n")
	}
	if len(names) == 0 {
		return fmt.Errorf("%w to any server: run otiumctl auth login --hostname <api host>", errNotSignedIn)
	}
	ctx := context.Background()
	var failed bool
	for _, n := range names {
		marker := ""
		if n == f.Default {
			marker = " (default)"
		}
		fmt.Printf("%s%s\n", n, marker)
		c, err := store.stored(n)
		if err != nil {
			fmt.Printf("  X %v\n", err)
			failed = true
			continue
		}
		if _, err := whoami(ctx, clientFor(store, c)); err != nil {
			fmt.Printf("  X signed in as %s, but the token does not work: %v\n    run otiumctl auth login --hostname %s\n", c.User, err, n)
			failed = true
			continue
		}
		fmt.Printf("  ✓ signed in as %s (token in the %s)\n", c.User, c.Where)
	}
	if failed {
		return errors.New("a sign-in needs attention")
	}
	return nil
}

func authLogout(args []string) error {
	fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
	hostname := fs.String("hostname", "", "the server to sign out of (default: the default one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openHosts()
	if err != nil {
		return err
	}
	server := *hostname
	if server == "" {
		f, err := store.load()
		if err != nil {
			return err
		}
		server = f.Default
	}
	if server == "" {
		return fmt.Errorf("%w to any server", errNotSignedIn)
	}
	name, _, err := hostName(server)
	if err != nil {
		return err
	}
	// Revoke the token on the server first, so signing out of a laptop really ends it there. An
	// API key is not revoked: it may be running something; it is revoked on the API keys page.
	if c, err := store.stored(name); err == nil && isUserToken(c.Token) {
		if err := clientFor(store, c).do(context.Background(), http.MethodDelete, "/v1/auth/token", nil, nil); err != nil {
			_, _ = fmt.Fprintf(authOut, "! Could not revoke the token on %s (%v); revoke it in the portal under Settings → Devices.\n", name, err)
		}
	}
	if err := store.signOut(name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(authOut, "✓ Signed out of %s\n", name)
	return nil
}

func authToken(args []string) error {
	fs := flag.NewFlagSet("auth token", flag.ContinueOnError)
	hostname := fs.String("hostname", "", "the server (default: the default one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openHosts()
	if err != nil {
		return err
	}
	c, err := store.resolve(*hostname, "")
	if err != nil {
		return err
	}
	// A stored sign-in's access token lasts an hour: print a current one, renewing it if need be.
	client := clientFor(store, c)
	if err := client.fresh(context.Background()); err != nil {
		return err
	}
	fmt.Println(client.key)
	return nil
}

// isUserToken reports whether a credential is a person's token rather than an API key.
func isUserToken(token string) bool { return strings.HasPrefix(token, "otium_user_") }

// openURL opens a link in the default browser. The link comes from the server, and every OS
// opener also runs local files and other schemes, so anything but an absolute http(s) URL is
// refused and only printed: a hostile server must not be able to launch a program here.
func openURL(ctx context.Context, link string) error {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("not opening %q: not an http(s) URL", link)
	}
	link = u.String()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", link) //nolint:gosec // an http(s) URL, checked above
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", link) //nolint:gosec // an http(s) URL, checked above
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", link) //nolint:gosec // an http(s) URL, checked above
	}
	return cmd.Start()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
