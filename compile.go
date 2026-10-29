package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// The CLI validates and tests workflows through the API (POST /v1/workflows/validate and
// /v1/workflows/test) rather than with a compiler of its own: the server's answer is the one a
// publish gets, for the account the CLI is signed in to, and otiumctl stays a plain client of
// the documented API, which is what lets it be open source. These types mirror the API's JSON
// (api/openapi.yaml) and hold only what the CLI reads.

// diagnostic is one compiler finding.
type diagnostic struct {
	Severity string `json:"severity"` // "error" or "warn"
	Code     string `json:"code"`
	Message  string `json:"message"`
	Node     string `json:"node,omitempty"`
	Path     string `json:"path,omitempty"`
}

type diagnostics []diagnostic

// hasErrors reports whether any finding blocks a publish.
func (ds diagnostics) hasErrors() bool {
	for _, d := range ds {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}

// setSummary is the set scope's part of a plan summary.
type setSummary struct {
	Nodes                int      `json:"nodes"`
	CriticalPath         []string `json:"critical_path"`
	EstCostP95USD        float64  `json:"est_cost_p95_usd"`
	EstCostPerItemP95USD float64  `json:"est_cost_per_item_p95_usd"`
}

// line is the set scope's cost line, worded as the portal and the site word it.
func (s *setSummary) line() string {
	return fmt.Sprintf("plus, once per submission, $%.4f + $%.6f per item (p95)", s.EstCostP95USD, s.EstCostPerItemP95USD)
}

// planSummary is a compiled document's headline.
type planSummary struct {
	Nodes               int         `json:"nodes"`
	ExternalNodes       int         `json:"external_nodes"`
	ToolNodes           int         `json:"tool_nodes"`
	Depth               int         `json:"depth"`
	CriticalPath        []string    `json:"critical_path"`
	P50DurationSeconds  float64     `json:"p50_duration_seconds"`
	P95DurationSeconds  float64     `json:"p95_duration_seconds"`
	P95ExecutionSeconds float64     `json:"p95_execution_seconds"`
	EstCostP50USD       float64     `json:"est_cost_p50_usd"`
	CostPer10kUSD       float64     `json:"cost_per_10k_usd"`
	AffinityGroups      int         `json:"affinity_groups"`
	Barriers            int         `json:"barriers"`
	CacheGroups         int         `json:"cache_groups"`
	Errors              int         `json:"errors"`
	EstimateBasis       string      `json:"estimate_basis"`
	DeclaredNodes       int         `json:"declared_nodes"`
	InferenceNodes      int         `json:"inference_nodes"`
	Set                 *setSummary `json:"set,omitempty"`
}

// compileResult is POST /v1/workflows/validate's answer.
type compileResult struct {
	OK          bool            `json:"ok"`
	Name        string          `json:"name,omitempty"`
	Digest      string          `json:"digest,omitempty"`
	Diagnostics diagnostics     `json:"diagnostics"`
	Summary     *planSummary    `json:"summary,omitempty"`
	Mermaid     string          `json:"mermaid,omitempty"`
	Plan        json.RawMessage `json:"plan,omitempty"`
}

// validateRemote asks the API to compile a document, storing nothing. withPlan asks for the
// compiled plan too.
func validateRemote(c *apiClient, src []byte, withPlan bool) (compileResult, error) {
	path := "/v1/workflows/validate"
	if withPlan {
		path += "?plan=true"
	}
	var res compileResult
	if err := c.do(context.Background(), http.MethodPost, path, documentEnvelope{Document: string(src)}, &res); err != nil {
		return compileResult{}, fmt.Errorf("validate: %w", err)
	}
	return res, nil
}
