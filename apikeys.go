package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"
)

// `otiumctl api-keys` manages the tenant's API keys over /v1/api-keys — what a customer's
// service and CI authenticate with. Minting one needs a person signed in (`otiumctl auth
// login`): an API key may not mint keys, so a leaked one cannot multiply.

func apiKeysCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: otiumctl api-keys list | create --name <name> [--expires-days n] | revoke <id>")
	}
	switch args[0] {
	case "list", "ls":
		return apiKeysList(args[1:])
	case "create":
		return apiKeysCreate(args[1:])
	case "revoke", "rm":
		return apiKeysRevoke(args[1:])
	default:
		return fmt.Errorf("unknown api-keys command %q: list, create or revoke", args[0])
	}
}

// apiKeyView is a key as the API lists it; the secret is never in a listing.
type apiKeyView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	LastFour   string    `json:"last_four"`
	CreatedBy  string    `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	Revoked    bool      `json:"revoked"`
}

func apiKeysList(args []string) error {
	fs := flag.NewFlagSet("api-keys list", flag.ContinueOnError)
	connect := apiFlags(fs)
	asJSON := fs.Bool("json", false, "print the response as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect()
	if err != nil {
		return err
	}
	var resp struct {
		Keys []apiKeyView `json:"keys"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/v1/api-keys", nil, &resp); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	if len(resp.Keys) == 0 {
		fmt.Println("no API keys: create one with otiumctl api-keys create --name <name>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tKEY\tCREATED\tLAST USED\tEXPIRES\tSTATUS")
	for _, k := range resp.Keys {
		status := "active"
		switch {
		case k.Revoked:
			status = "revoked"
		case !k.ExpiresAt.IsZero() && !time.Now().Before(k.ExpiresAt):
			status = "expired"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s-…%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Prefix, k.LastFour,
			day(k.CreatedAt), orDash(day(k.LastUsedAt)), orDash(day(k.ExpiresAt)), status)
	}
	return w.Flush()
}

func apiKeysCreate(args []string) error {
	fs := flag.NewFlagSet("api-keys create", flag.ContinueOnError)
	connect := apiFlags(fs)
	name := fs.String("name", "", "what the key is for, e.g. prod-firehose or ci (required)")
	expiresDays := fs.Int("expires-days", 90, "days until the key expires (0: never expires)")
	asJSON := fs.Bool("json", false, "print the response as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	if *expiresDays < 0 {
		return errors.New("--expires-days must be 0 or more")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	var created struct {
		apiKeyView
		Secret string `json:"secret"`
	}
	body := map[string]any{"name": *name, "expires_days": *expiresDays}
	if err := c.do(context.Background(), http.MethodPost, "/v1/api-keys", body, &created); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusForbidden && !isUserToken(c.key) {
			return fmt.Errorf("%w\n  an API key cannot create keys: sign in as yourself with otiumctl auth login", err)
		}
		return err
	}
	if *asJSON {
		return printJSON(created)
	}
	fmt.Printf("Created API key %s (%s)\n", created.ID, created.Name)
	if !created.ExpiresAt.IsZero() {
		fmt.Printf("Expires %s\n", created.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Println()
	fmt.Println(created.Secret)
	fmt.Println()
	_, _ = fmt.Fprintln(os.Stderr, "! Store this now: the secret is shown once and cannot be retrieved.")
	return nil
}

func apiKeysRevoke(args []string) error {
	fs := flag.NewFlagSet("api-keys revoke", flag.ContinueOnError)
	connect := apiFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: otiumctl api-keys revoke <id>")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := c.do(context.Background(), http.MethodDelete, "/v1/api-keys/"+url.PathEscape(id), nil, nil); err != nil {
		return err
	}
	fmt.Printf("Revoked API key %s\n", id)
	return nil
}

func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
