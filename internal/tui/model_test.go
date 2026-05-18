package tui

import (
	"testing"
	"time"

	"github.com/abap34/disable-workflows/internal/ghapi"
)

func TestWorkflowFilterDefaultsToWorkflowName(t *testing.T) {
	item := filterTestItem()

	if !itemMatches(item, parseWorkflowFilter("deploy")) {
		t.Fatal("expected unqualified filter to match workflow name")
	}
	if itemMatches(item, parseWorkflowFilter("payments")) {
		t.Fatal("expected unqualified filter not to match repository name")
	}
}

func TestWorkflowFilterSupportsFieldPrefixes(t *testing.T) {
	item := filterTestItem()

	cases := []string{
		"all:payments",
		"repo:payments",
		"workflow:deploy",
		"wf:deploy",
		"path:deploy.yml",
		"state:active",
		"vis:private",
		"last:2026-01-03",
		"updated:2026-01-02",
		"repo:payments workflow:deploy state:active",
	}
	for _, query := range cases {
		if !itemMatches(item, parseWorkflowFilter(query)) {
			t.Fatalf("expected filter %q to match", query)
		}
	}

	if itemMatches(item, parseWorkflowFilter("repo:website workflow:deploy")) {
		t.Fatal("expected mismatched repo field to fail")
	}
}

func filterTestItem() ghapi.WorkflowItem {
	return ghapi.WorkflowItem{
		Repo: ghapi.Repo{
			Name:     "payments-api",
			FullName: "acme/payments-api",
			Private:  true,
			Owner: ghapi.Owner{
				Login: "acme",
			},
		},
		Workflow: ghapi.Workflow{
			Name:      "Deploy Production",
			Path:      ".github/workflows/deploy.yml",
			State:     "active",
			UpdatedAt: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
		},
		LastRunAt: time.Date(2026, 1, 3, 4, 5, 0, 0, time.UTC),
	}
}
