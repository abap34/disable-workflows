package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestListWorkflowsUsesConditionalCache(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/repos/acme/widgets/actions/workflows" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"total_count":1,"workflows":[{"id":1,"name":"CI","path":".github/workflows/ci.yml","state":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]}`)
	}))
	defer server.Close()

	cache := NewCache(t.TempDir())
	client := NewClient("token",
		WithBaseURL(server.URL),
		WithCache(cache),
		WithMinRequestInterval(0),
		WithCacheMaxAge(0),
	)

	first, err := client.ListWorkflows(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.ListWorkflows(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || second[0].Name != "CI" {
		t.Fatalf("unexpected workflows: %#v %#v", first, second)
	}
	if requests != 2 {
		t.Fatalf("expected 2 HTTP requests, got %d", requests)
	}
	if client.CacheHits() != 1 {
		t.Fatalf("expected 1 cache hit, got %d", client.CacheHits())
	}
}

func TestListWorkflowsUsesFreshCacheWithoutRequest(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("ETag", `"fresh"`)
		fmt.Fprint(w, `{"total_count":1,"workflows":[{"id":1,"name":"CI","path":".github/workflows/ci.yml","state":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]}`)
	}))
	defer server.Close()

	client := NewClient("token",
		WithBaseURL(server.URL),
		WithCache(NewCache(t.TempDir())),
		WithMinRequestInterval(0),
		WithCacheMaxAge(time.Hour),
	)
	if _, err := client.ListWorkflows(context.Background(), "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListWorkflows(context.Background(), "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("expected one HTTP request with fresh cache, got %d", requests)
	}
	if client.CacheHits() != 1 {
		t.Fatalf("expected 1 cache hit, got %d", client.CacheHits())
	}
}

func TestDisableWorkflow(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/repos/acme/widgets/actions/workflows/42/disable" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient("token", WithBaseURL(server.URL), WithMinRequestInterval(0))
	if err := client.DisableWorkflow(context.Background(), "acme/widgets", 42); err != nil {
		t.Fatal(err)
	}
}

func TestDisableWorkflowInvalidatesCachedWorkflowPages(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var server *httptest.Server
	disabled := false
	page2Gets := 0

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widgets/actions/workflows/42/disable":
			mu.Lock()
			disabled = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/actions/workflows":
			page := r.URL.Query().Get("page")
			mu.Lock()
			isDisabled := disabled
			if page == "2" {
				page2Gets++
			}
			mu.Unlock()

			switch page {
			case "":
				w.Header().Set("ETag", fmt.Sprintf(`"page1-%t"`, isDisabled))
				w.Header().Set("Link", fmt.Sprintf("<%s/repos/acme/widgets/actions/workflows?per_page=100&page=2>; rel=\"next\"", server.URL))
				fmt.Fprint(w, `{"total_count":2,"workflows":[{"id":1,"name":"CI","path":".github/workflows/ci.yml","state":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]}`)
			case "2":
				state := "active"
				if isDisabled {
					state = "disabled_manually"
				}
				w.Header().Set("ETag", fmt.Sprintf(`"page2-%t"`, isDisabled))
				fmt.Fprintf(w, `{"total_count":2,"workflows":[{"id":42,"name":"Release","path":".github/workflows/release.yml","state":%q,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]}`, state)
			default:
				t.Fatalf("unexpected page: %s", page)
			}
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient("token",
		WithBaseURL(server.URL),
		WithCache(NewCache(t.TempDir())),
		WithMinRequestInterval(0),
		WithCacheMaxAge(time.Hour),
	)
	first, err := client.ListWorkflows(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got := workflowState(first, 42); got != "active" {
		t.Fatalf("unexpected initial workflow state: %s", got)
	}
	if err := client.DisableWorkflow(context.Background(), "acme/widgets", 42); err != nil {
		t.Fatal(err)
	}
	second, err := client.ListWorkflows(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got := workflowState(second, 42); got != "disabled_manually" {
		t.Fatalf("expected refreshed workflow state after disable, got %s", got)
	}
	mu.Lock()
	gotPage2Gets := page2Gets
	mu.Unlock()
	if gotPage2Gets != 2 {
		t.Fatalf("expected page 2 to be refetched after disable, got %d requests", gotPage2Gets)
	}
}

func TestAuthenticatedUser(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/user" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"login":"octocat","type":"User"}`)
	}))
	defer server.Close()

	client := NewClient("token", WithBaseURL(server.URL), WithMinRequestInterval(0))
	user, err := client.AuthenticatedUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.Login != "octocat" || user.Type != "User" {
		t.Fatalf("unexpected user: %#v", user)
	}
}

func TestListAccountWorkflowsReportsProgress(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"octocat","type":"User"}`)
		case "/user/repos":
			fmt.Fprint(w, `[{"id":1,"name":"one","full_name":"octocat/one","private":false},{"id":2,"name":"two","full_name":"octocat/two","private":true}]`)
		case "/repos/octocat/one/actions/workflows":
			fmt.Fprint(w, `{"total_count":1,"workflows":[{"id":11,"name":"CI","path":".github/workflows/ci.yml","state":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]}`)
		case "/repos/octocat/one/actions/runs":
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{"id":101,"workflow_id":11,"status":"completed","created_at":"2026-01-03T00:00:00Z","updated_at":"2026-01-03T00:05:00Z","run_started_at":"2026-01-03T00:01:00Z"}]}`)
		case "/repos/octocat/two/actions/workflows":
			fmt.Fprint(w, `{"total_count":0,"workflows":[]}`)
		case "/repos/octocat/two/actions/runs":
			fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient("token", WithBaseURL(server.URL), WithMinRequestInterval(0))
	var mu sync.Mutex
	var events []ProgressEvent
	result, err := client.ListAccountWorkflows(context.Background(), ListOptions{
		Owner:       "octocat",
		Concurrency: 1,
		LastRunMode: LastRunRepo,
		Progress: func(event ProgressEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || len(result.Repos) != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if got := result.Items[0].LastRunAt.Format(time.RFC3339); got != "2026-01-03T00:01:00Z" {
		t.Fatalf("unexpected last run: %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 3 {
		t.Fatalf("expected progress events, got %#v", events)
	}
	last := events[len(events)-1]
	if last.ReposDone != 2 || last.ReposTotal != 2 || last.Workflows != 1 {
		t.Fatalf("unexpected final progress event: %#v", last)
	}
}

func TestNormalizeLastRunModeDefaultsToWorkflow(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "workflow", "exact", "unknown"} {
		if got := normalizeLastRunMode(input); got != LastRunWorkflow {
			t.Fatalf("normalizeLastRunMode(%q) = %q, want %q", input, got, LastRunWorkflow)
		}
	}
	for _, input := range []string{"repo", "fast", "approx", "approximate"} {
		if got := normalizeLastRunMode(input); got != LastRunRepo {
			t.Fatalf("normalizeLastRunMode(%q) = %q, want %q", input, got, LastRunRepo)
		}
	}
	for _, input := range []string{"off", "none", "false"} {
		if got := normalizeLastRunMode(input); got != LastRunOff {
			t.Fatalf("normalizeLastRunMode(%q) = %q, want %q", input, got, LastRunOff)
		}
	}
}

func TestCachePutGet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cache := NewCache(dir)
	entry := cacheEntry{
		ETag:       `"abc"`,
		StatusCode: http.StatusOK,
		Header: httpHeader{
			"Link": []string{"<next>; rel=\"next\""},
		},
		Body:      []byte(`{"ok":true}`),
		FetchedAt: time.Now(),
	}
	if err := cache.Put("GET https://example.test", entry); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get("GET https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag != entry.ETag || string(got.Body) != string(entry.Body) {
		t.Fatalf("unexpected cache entry: %#v", got)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one cache file, got %d", len(files))
	}
}

func workflowState(workflows []Workflow, id int64) string {
	for _, workflow := range workflows {
		if workflow.ID == id {
			return workflow.State
		}
	}
	return ""
}
