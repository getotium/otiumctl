package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
)

// The tool-endpoint verbs.
//
//	otiumctl tools list
//	otiumctl tools register --name <name> --url <https://…> [--description <s>]
//	otiumctl tools update <id> [--url …] [--description …]
//	otiumctl tools rotate <id>
//	otiumctl tools rm <id>
//
// `register` and `rotate` print the signing secret, once, exactly as the webhook verbs do: a
// receiver verifies every call with it, and it is stored sealed with no read path back.

// toolEndpoint mirrors the API's projection, restated for the reason webhookEndpoint is.
type toolEndpoint struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type toolRegisterResult struct {
	Endpoint   toolEndpoint `json:"endpoint"`
	Secret     string       `json:"secret"`
	SecretNote string       `json:"secret_note"`
}

func toolsCommand(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("tools needs a verb")
	}
	switch args[0] {
	case "list":
		return toolsList(args[1:])
	case "register", "add":
		return toolsRegister(args[1:])
	case "update":
		return toolsUpdate(args[1:])
	case "rotate":
		return toolsRotate(args[1:])
	case "rm", "delete":
		return toolsRemove(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown tools verb %q", args[0])
	}
}

func toolsList(args []string) error {
	fs := flag.NewFlagSet("tools list", flag.ContinueOnError)
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
		Endpoints []toolEndpoint `json:"endpoints"`
	}
	if err := c.do(context.Background(), http.MethodGet, "/v1/tools", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out.Endpoints)
	}
	if len(out.Endpoints) == 0 {
		fmt.Println("no tool endpoints (register one with `otiumctl tools register --name … --url …`)")
		return nil
	}
	fmt.Printf("%-28s %-24s %s\n", "ID", "NAME", "URL")
	for _, ep := range out.Endpoints {
		fmt.Printf("%-28s %-24s %s\n", ep.ID, ep.Name, ep.URL)
	}
	return nil
}

func toolsRegister(args []string) error {
	fs := flag.NewFlagSet("tools register", flag.ContinueOnError)
	name := fs.String("name", "", "the name a workflow's `tools: { endpoint }` uses")
	url := fs.String("url", "", "https endpoint every tool call is POSTed to")
	description := fs.String("description", "", "your own label for this endpoint")
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *url == "" {
		return errors.New("--name and --url are required")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out toolRegisterResult
	body := map[string]any{"name": *name, "url": *url, "description": *description}
	if err := c.do(context.Background(), http.MethodPost, "/v1/tools", body, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	printToolSecret(out)
	return nil
}

func toolsUpdate(args []string) error {
	fs := flag.NewFlagSet("tools update", flag.ContinueOnError)
	url := fs.String("url", "", "replace the endpoint url")
	description := fs.String("description", "", "replace the label")
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl tools update <id> [--url …] [--description …]")
	}
	c, err := client()
	if err != nil {
		return err
	}
	// Only the flags given are sent, so updating the label never blanks the URL.
	body := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "url":
			body["url"] = *url
		case "description":
			body["description"] = *description
		}
	})
	if len(body) == 0 {
		return errors.New("nothing to update: pass --url or --description")
	}
	var out toolEndpoint
	if err := c.do(context.Background(), http.MethodPatch, "/v1/tools/"+id, body, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	fmt.Printf("%s %s\n  url: %s\n", out.ID, out.Name, out.URL)
	return nil
}

func toolsRotate(args []string) error {
	fs := flag.NewFlagSet("tools rotate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl tools rotate <id>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	var out toolRegisterResult
	if err := c.do(context.Background(), http.MethodPost, "/v1/tools/"+id+"/rotate", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	// Calls signed with the old secret stop verifying at once; a receiver deploying the new one
	// later fails every call in between, and needs to know that before running this.
	fmt.Println("rotated. Calls are now signed with the new secret only; the previous one no longer verifies.")
	printToolSecret(out)
	return nil
}

func toolsRemove(args []string) error {
	fs := flag.NewFlagSet("tools rm", flag.ContinueOnError)
	client := apiFlags(fs)
	id, err := parseArg(fs, args)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("usage: otiumctl tools rm <id>")
	}
	c, err := client()
	if err != nil {
		return err
	}
	if err := c.do(context.Background(), http.MethodDelete, "/v1/tools/"+id, nil, nil); err != nil {
		return err
	}
	fmt.Printf("removed %s (workflows naming it fail their tool calls as tool_refused until it is registered again)\n", id)
	return nil
}

// printToolSecret prints a one-time signing secret with the warning that makes it actionable.
func printToolSecret(out toolRegisterResult) {
	fmt.Printf("endpoint %s (%s)\n  url: %s\n", out.Endpoint.Name, out.Endpoint.ID, out.Endpoint.URL)
	fmt.Printf("\n  signing secret: %s\n", out.Secret)
	note := out.SecretNote
	if note == "" {
		note = "Store this now: it is shown once and cannot be retrieved."
	}
	fmt.Printf("  %s\n", note)
	fmt.Printf("  Verify each call's Otium-Signature with it, as you would a webhook's.\n")
}
