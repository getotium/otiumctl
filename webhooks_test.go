package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The signing secret is shown once and never again, so the moment it is printed is the
// only moment the customer can act on it — which makes the warning part of the feature,
// not decoration.
func TestAddPrintsTheSecretWithTheWarningThatMakesItActionable(t *testing.T) {
	api := newFakeAPI(t, `{
		"endpoint":{"id":"wh_1","url":"https://hooks.example.com/otium",
		            "events":["run_set.completed","run_set.node_parked"],"enabled":true},
		"secret":"whsec_abc123",
		"secret_note":"Store this now: the signing secret is shown once and cannot be retrieved."}`)
	api.status = http.StatusCreated

	out, err := capture(t, func() error {
		return webhooksAdd([]string{"--url", "https://hooks.example.com/otium",
			"--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/webhooks" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	if !strings.Contains(out, "whsec_abc123") {
		t.Errorf("the secret was not printed: %q", out)
	}
	if !strings.Contains(out, "shown once") {
		t.Errorf("no one-time warning: %q", out)
	}
	if !strings.Contains(out, "run_set.node_parked") {
		t.Errorf("the subscription is not shown: %q", out)
	}
}

// An unnamed subscription must not be sent as an empty list: the server's "no events
// named" default is the set-level pair, and an explicit empty array would be a different
// request that happens to look the same in a shell.
func TestAddOmitsTheEventsFieldWhenNoneWereNamed(t *testing.T) {
	api := newFakeAPI(t, `{"endpoint":{"id":"wh_1"},"secret":"whsec_x"}`)
	api.status = http.StatusCreated

	if _, err := capture(t, func() error {
		return webhooksAdd([]string{"--url", "https://hooks.example.com/x", "--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if _, present := sent["events"]; present {
		t.Errorf("an unnamed subscription was sent as %v", sent["events"])
	}

	// And when they ARE named, they arrive as a list with the whitespace and trailing
	// comma a shell invocation collects along the way.
	api.body = nil
	if _, err := capture(t, func() error {
		return webhooksAdd([]string{"--url", "https://hooks.example.com/x",
			"--events", "run.completed, run.failed,", "--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	sent = nil
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	events, ok := sent["events"].([]any)
	if !ok || len(events) != 2 {
		t.Fatalf("events = %#v, want two clean entries", sent["events"])
	}
	if events[0] != "run.completed" || events[1] != "run.failed" {
		t.Errorf("events = %v", events)
	}
}

// A partial update sends only the flags that were given. An update that also reset the
// subscription every time someone disabled an endpoint would silently opt the customer
// back out of the per-run events they deliberately chose.
func TestUpdateSendsOnlyTheFlagsThatWereGiven(t *testing.T) {
	api := newFakeAPI(t, `{"id":"wh_1","url":"https://hooks.example.com/x","events":["run.completed"],"enabled":false}`)

	if _, err := capture(t, func() error {
		return webhooksUpdate([]string{"wh_1", "--disable", "--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if api.method != http.MethodPatch || api.path != "/v1/webhooks/wh_1" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(sent) != 1 || sent["enabled"] != false {
		t.Errorf("--disable sent %v, want only {\"enabled\":false}", sent)
	}

	// Nothing at all is refused locally rather than sent as an empty PATCH the server
	// would answer 200 to, which reads as success.
	if _, err := capture(t, func() error {
		return webhooksUpdate([]string{"wh_1", "--api", api.URL, "--key", "sk"})
	}); err == nil {
		t.Error("an update with no flags was sent")
	}
	// And the two switches cannot both be given.
	if _, err := capture(t, func() error {
		return webhooksUpdate([]string{"wh_1", "--enable", "--disable", "--api", api.URL, "--key", "sk"})
	}); err == nil {
		t.Error("--enable --disable together was accepted")
	}
}

// Rotation invalidates the old secret immediately, so somebody who deploys the new one an
// hour later has an hour of unverifiable notifications. They need that before they run it.
func TestRotateSaysTheOldSecretIsAlreadyDead(t *testing.T) {
	api := newFakeAPI(t, `{"endpoint":{"id":"wh_1","url":"https://h.example.com/x"},"secret":"whsec_new"}`)

	out, err := capture(t, func() error {
		return webhooksRotate([]string{"wh_1", "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/webhooks/wh_1/rotate" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	if !strings.Contains(out, "whsec_new") {
		t.Errorf("the new secret was not printed: %q", out)
	}
	if !strings.Contains(out, "no longer verifies") {
		t.Errorf("the output does not say the old secret is dead: %q", out)
	}
	// Even with no note from the server, the one-time warning still appears — it is the
	// CLI's own claim about how this works, not something it relays.
	if !strings.Contains(out, "shown once") {
		t.Errorf("no one-time warning: %q", out)
	}
}

// The listing prints the vocabulary with the per-run events MARKED, because that is the
// difference between one notification and ten thousand and it is invisible from the name.
func TestListMarksTheEventsThatFireOncePerItem(t *testing.T) {
	api := newFakeAPI(t, `{
		"endpoints":[{"id":"wh_1","url":"https://h.example.com/x","enabled":false,
		              "events":["run_set.completed"],"consecutive_failures":4,
		              "last_error":"endpoint returned 502"}],
		"available_events":[
			{"event":"run_set.completed","per_run":false,"default":true},
			{"event":"node.completed","per_run":true,"default":false}]}`)

	out, err := capture(t, func() error {
		return webhooksList([]string{"--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if api.path != "/v1/webhooks" {
		t.Errorf("path = %s", api.path)
	}
	if !strings.Contains(out, "* run_set.completed") {
		t.Errorf("the default events are not marked: %q", out)
	}
	if !strings.Contains(out, "! node.completed") {
		t.Errorf("the per-run events are not marked: %q", out)
	}
	// A failing endpoint says so in the listing: "why am I not getting webhooks?" should
	// be answerable here rather than from a delivery search.
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "502") {
		t.Errorf("the failure state is not visible: %q", out)
	}
}

// "Did you send it?" — including for the dead-lettered ones, which are the only deliveries
// anyone ever looks up.
func TestDeliveriesShowsWhatWasSentAndWhatCameBack(t *testing.T) {
	api := newFakeAPI(t, `{"deliveries":[
		{"id":"whd_1","endpoint_id":"wh_1","event":"run_set.node_parked","status":"dead",
		 "attempts":6,"last_status_code":502,"last_error":"endpoint returned 502",
		 "created_at":"2026-09-01T10:00:00Z"},
		{"id":"whd_2","endpoint_id":"wh_1","event":"run_set.completed","status":"pending",
		 "attempts":1,"next_attempt_at":"2026-09-01T10:02:00Z","created_at":"2026-09-01T10:01:00Z"}]}`)

	out, err := capture(t, func() error {
		return webhooksDeliveries([]string{"wh_1", "--api", api.URL, "--key", "sk"})
	})
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if api.path != "/v1/webhooks/wh_1/deliveries" {
		t.Errorf("path = %s", api.path)
	}
	if !strings.Contains(out, "run_set.node_parked") || !strings.Contains(out, "endpoint returned 502") {
		t.Errorf("the dead delivery is not readable: %q", out)
	}
	// A pending delivery shows WHEN it will be tried again, which is the difference
	// between "we gave up" and "wait two minutes".
	if !strings.Contains(out, "next attempt 2026-09-01T10:02:00Z") {
		t.Errorf("a pending delivery does not say when it retries: %q", out)
	}

	// Without an id it is the whole account, which is the dashboard view.
	if _, err := capture(t, func() error {
		return webhooksDeliveries([]string{"--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if !strings.HasPrefix(api.path, "/v1/webhooks/deliveries") {
		t.Errorf("account-wide path = %s", api.path)
	}
}

func TestWebhookVerbsRequireTheirIdentifiers(t *testing.T) {
	api := newFakeAPI(t, `{}`)
	for _, fn := range []func() error{
		func() error { return webhooksAdd([]string{"--api", api.URL, "--key", "sk"}) },
		func() error { return webhooksUpdate([]string{"--enable", "--api", api.URL, "--key", "sk"}) },
		func() error { return webhooksRotate([]string{"--api", api.URL, "--key", "sk"}) },
		func() error { return webhooksRemove([]string{"--api", api.URL, "--key", "sk"}) },
	} {
		if _, err := capture(t, fn); err == nil {
			t.Error("a verb ran without its required identifier")
		}
	}
}

// A refusal from the server has to read as a refusal, not as a crash: an unknown event or
// a blocked URL is the caller's mistake and the message names it.
func TestAWebhookRefusalIsReportedAsARefusal(t *testing.T) {
	api := newFakeAPI(t, `{"error":"webhook: address is not permitted: 169.254.169.254"}`)
	api.status = http.StatusBadRequest

	_, err := capture(t, func() error {
		return webhooksAdd([]string{"--url", "https://169.254.169.254/x", "--api", api.URL, "--key", "sk"})
	})
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("error = %q", err)
	}
}

// A tool endpoint is registered by name — the name a workflow references — and its secret is
// printed once with the instruction to verify calls with it.
func TestToolsRegisterSendsTheNameAndPrintsTheSecretOnce(t *testing.T) {
	api := newFakeAPI(t, `{
		"endpoint":{"id":"tl_1","name":"catalog-api","url":"https://tools.example.com/hook"},
		"secret":"tlsec_abc123",
		"secret_note":"Store this now: the signing secret is shown once and cannot be retrieved."}`)
	api.status = http.StatusCreated

	out, err := capture(t, func() error {
		return toolsRegister([]string{"--name", "catalog-api", "--url", "https://tools.example.com/hook",
			"--api", api.URL, "--key", "sk-test"})
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if api.method != http.MethodPost || api.path != "/v1/tools" {
		t.Errorf("request = %s %s", api.method, api.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(api.body, &sent); err != nil || sent["name"] != "catalog-api" {
		t.Errorf("sent %v (%v), want the name", sent, err)
	}
	for _, want := range []string{"tlsec_abc123", "shown once", "Otium-Signature"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if err := toolsRegister([]string{"--url", "https://x", "--api", api.URL, "--key", "k"}); err == nil {
		t.Error("register without --name succeeded")
	}
}

// An update sends only what was asked, so relabelling never blanks the URL.
func TestToolsUpdateSendsOnlyTheGivenFields(t *testing.T) {
	api := newFakeAPI(t, `{"id":"tl_1","name":"catalog-api","url":"https://tools.example.com/hook"}`)
	if _, err := capture(t, func() error {
		return toolsUpdate([]string{"tl_1", "--description", "new label", "--api", api.URL, "--key", "sk"})
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(api.body, &sent); err != nil {
		t.Fatal(err)
	}
	if _, has := sent["url"]; has || sent["description"] != "new label" || api.method != http.MethodPatch {
		t.Errorf("sent %s %v", api.method, sent)
	}
}
