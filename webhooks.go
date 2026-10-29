package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

// The webhook verbs.
//
//	otiumctl webhooks list
//	otiumctl webhooks add --url <https://…> [--events a,b] [--description <s>]
//	otiumctl webhooks update <id> [--url …] [--events …] [--enable|--disable]
//	otiumctl webhooks rotate <id>
//	otiumctl webhooks rm <id>
//	otiumctl webhooks deliveries [id] [--limit n]
//
// `add` and `rotate` print the signing secret, and they are the only things that ever
// will: it is stored sealed and there is no read path back to plaintext. The output says
// so, because the person reading it is at the only moment they can act on it.

// webhookEndpoint mirrors the API's projection. Restated here rather than imported from
// pkg/customerapi for the same reason assetVersion is: that package is an HTTP server, and
// importing it would drag gin, a billing service and a Stripe client into a binary that
// makes one JSON call.
type webhookEndpoint struct {
	ID                  string   `json:"id"`
	URL                 string   `json:"url"`
	Description         string   `json:"description,omitempty"`
	Events              []string `json:"events"`
	Enabled             bool     `json:"enabled"`
	CreatedAt           string   `json:"created_at"`
	LastSuccessAt       string   `json:"last_success_at,omitempty"`
	LastFailureAt       string   `json:"last_failure_at,omitempty"`
	LastError           string   `json:"last_error,omitempty"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
}

type webhookRegisterResult struct {
	Endpoint   webhookEndpoint `json:"endpoint"`
	Secret     string          `json:"secret"`
	SecretNote string          `json:"secret_note"`
}

func webhooksCommand(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("webhooks needs a verb")
	}
	switch args[0] {
	case "list":
		return webhooksList(args[1:])
	case "add":
		return webhooksAdd(args[1:])
	case "update":
		return webhooksUpdate(args[1:])
	case "rotate":
		return webhooksRotate(args[1:])
	case "rm", "delete":
		return webhooksRemove(args[1:])
	case "deliveries":
		return webhooksDeliveries(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown webhooks verb %q", args[0])
	}
}

func webhooksList(args []string) error {
	fs := flag.NewFlagSet("webhooks list", flag.ContinueOnError)
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
		Endpoints       []webhookEndpoint `json:"endpoints"`
		AvailableEvents []struct {
			Event   string `json:"event"`
			PerRun  bool   `json:"per_run"`
			Default bool   `json:"default"`
		} `json:"available_events"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/v1/webhooks", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	if len(out.Endpoints) == 0 {
		fmt.Println("no webhook endpoints")
	} else {
		fmt.Printf("%-26s %-8s %-9s %s\n", "ID", "STATE", "FAILS", "URL")
		for _, ep := range out.Endpoints {
			state := "enabled"
			if !ep.Enabled {
				state = "disabled"
			}
			fmt.Printf("%-26s %-8s %-9d %s\n", ep.ID, state, ep.ConsecutiveFailures, ep.URL)
			fmt.Printf("%-26s events: %s\n", "", strings.Join(ep.Events, ", "))
			if ep.LastError != "" {
				fmt.Printf("%-26s last error: %s\n", "", ep.LastError)
			}
		}
	}
	// The vocabulary, with the per-run ones marked: a customer ticking a box needs to know
	// which of these turns a 10,000-item batch into 10,000 notifications.
	fmt.Println("\navailable events (* = subscribed by default, ! = fires once per item):")
	for _, ev := range out.AvailableEvents {
		mark := " "
		if ev.Default {
			mark = "*"
		}
		if ev.PerRun {
			mark = "!"
		}
		fmt.Printf("  %s %s\n", mark, ev.Event)
	}
	return nil
}

func webhooksAdd(args []string) error {
	fs := flag.NewFlagSet("webhooks add", flag.ContinueOnError)
	url := fs.String("url", "", "https endpoint to POST notifications to")
	events := fs.String("events", "", "comma-separated event list (default: the set-level events)")
	description := fs.String("description", "", "your own label for this endpoint")
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("--url is required")
	}
	c, err := client()
	if err != nil {
		return err
	}
	body := map[string]any{"url": *url, "description": *description}
	if list := splitEvents(*events); len(list) > 0 {
		body["events"] = list
	}
	var out webhookRegisterResult
	if err := c.do(context.Background(), http.MethodPost, "/v1/webhooks", body, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	printSecret(out)
	return nil
}

func webhooksUpdate(args []string) error {
	fs := flag.NewFlagSet("webhooks update", flag.ContinueOnError)
	url := fs.String("url", "", "replace the endpoint url")
	events := fs.String("events", "", "replace the subscription")
	description := fs.String("description", "", "replace the label")
	enable := fs.Bool("enable", false, "re-enable the endpoint")
	disable := fs.Bool("disable", false, "stop delivering to the endpoint")
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl webhooks update <id> [flags]")
	}
	if *enable && *disable {
		return errors.New("--enable and --disable are mutually exclusive")
	}
	c, err := client()
	if err != nil {
		return err
	}
	// Only the flags that were actually given are sent. An update that silently reset the
	// subscription every time someone disabled an endpoint would opt the customer back
	// out of the per-run events they deliberately chose.
	body := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "url":
			body["url"] = *url
		case "events":
			body["events"] = splitEvents(*events)
		case "description":
			body["description"] = *description
		case "enable":
			body["enabled"] = true
		case "disable":
			body["enabled"] = false
		}
	})
	if len(body) == 0 {
		return errors.New("nothing to update: pass at least one of --url, --events, --description, --enable, --disable")
	}
	var out webhookEndpoint
	if err := c.do(context.Background(), http.MethodPatch, "/v1/webhooks/"+id, body, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	state := "enabled"
	if !out.Enabled {
		state = "disabled"
	}
	fmt.Printf("%s %s\n  url:    %s\n  events: %s\n", out.ID, state, out.URL, strings.Join(out.Events, ", "))
	return nil
}

func webhooksRotate(args []string) error {
	fs := flag.NewFlagSet("webhooks rotate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl webhooks rotate <id>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out webhookRegisterResult
	if err := c.do(context.Background(), http.MethodPost, "/v1/webhooks/"+id+"/rotate", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	// The old secret stops verifying the moment this returns, so the message says so:
	// somebody who deploys the new one an hour later has an hour of unverifiable
	// notifications and needs to know that before they run it, not after.
	fmt.Println("rotated. The previous secret no longer verifies our signatures.")
	printSecret(out)
	return nil
}

func webhooksRemove(args []string) error {
	fs := flag.NewFlagSet("webhooks rm", flag.ContinueOnError)
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl webhooks rm <id>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	if err := c.do(context.Background(), http.MethodDelete, "/v1/webhooks/"+id, nil, nil); err != nil {
		return err
	}
	fmt.Printf("removed %s (its delivery history went with it)\n", id)
	return nil
}

func webhooksDeliveries(args []string) error {
	fs := flag.NewFlagSet("webhooks deliveries", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "how many to show")
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	path := "/v1/webhooks/deliveries"
	if id != "" {
		path = "/v1/webhooks/" + id + "/deliveries"
	}
	path += "?limit=" + fmt.Sprint(*limit)

	var out struct {
		Deliveries []struct {
			ID             string `json:"id"`
			EndpointID     string `json:"endpoint_id"`
			Event          string `json:"event"`
			Status         string `json:"status"`
			Attempts       int    `json:"attempts"`
			NextAttemptAt  string `json:"next_attempt_at,omitempty"`
			LastStatusCode int    `json:"last_status_code,omitempty"`
			LastError      string `json:"last_error,omitempty"`
			CreatedAt      string `json:"created_at"`
		} `json:"deliveries"`
	}
	if err := c.do(context.Background(), http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out.Deliveries)
	}
	if len(out.Deliveries) == 0 {
		fmt.Println("no deliveries")
		return nil
	}
	fmt.Printf("%-26s %-22s %-10s %5s  %s\n", "WHEN", "EVENT", "STATUS", "TRIES", "DETAIL")
	for _, d := range out.Deliveries {
		detail := d.LastError
		if detail == "" && d.Status == "delivered" {
			detail = fmt.Sprintf("HTTP %d", d.LastStatusCode)
		}
		if d.Status == "pending" && d.NextAttemptAt != "" {
			detail = "next attempt " + d.NextAttemptAt
		}
		fmt.Printf("%-26s %-22s %-10s %5d  %s\n",
			d.CreatedAt, d.Event, d.Status, d.Attempts, detail)
	}
	return nil
}

// printSecret prints a one-time signing secret with the warning that makes it actionable.
func printSecret(out webhookRegisterResult) {
	fmt.Printf("endpoint %s\n  url:    %s\n  events: %s\n",
		out.Endpoint.ID, out.Endpoint.URL, strings.Join(out.Endpoint.Events, ", "))
	fmt.Printf("\n  signing secret: %s\n", out.Secret)
	note := out.SecretNote
	if note == "" {
		note = "Store this now: it is shown once and cannot be retrieved."
	}
	fmt.Printf("  %s\n", note)
}

// splitEvents parses a comma-separated subscription, dropping empties so a trailing comma
// is not an "unknown event" from the server.
func splitEvents(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if ev := strings.TrimSpace(part); ev != "" {
			out = append(out, ev)
		}
	}
	return out
}
