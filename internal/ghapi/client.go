package ghapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.github.com"

const (
	LastRunOff      = "off"
	LastRunRepo     = "repo"
	LastRunWorkflow = "workflow"
)

type Client struct {
	token       string
	baseURL     string
	httpClient  *http.Client
	cache       *Cache
	minInterval time.Duration
	maxRateWait time.Duration
	cacheMaxAge time.Duration

	mu          sync.Mutex
	lastRequest time.Time
	rate        RateInfo
	requests    int
	cacheHits   int
	authUser    *Owner
}

type Option func(*Client)

func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		token:       token,
		baseURL:     defaultBaseURL,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		minInterval: 250 * time.Millisecond,
		maxRateWait: 2 * time.Minute,
		cacheMaxAge: 5 * time.Minute,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func WithCache(cache *Cache) Option {
	return func(c *Client) {
		c.cache = cache
	}
}

func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

func WithMinRequestInterval(interval time.Duration) Option {
	return func(c *Client) {
		if interval >= 0 {
			c.minInterval = interval
		}
	}
}

func WithMaxRateLimitWait(wait time.Duration) Option {
	return func(c *Client) {
		if wait >= 0 {
			c.maxRateWait = wait
		}
	}
}

func WithCacheMaxAge(maxAge time.Duration) Option {
	return func(c *Client) {
		if maxAge >= 0 {
			c.cacheMaxAge = maxAge
		}
	}
}

type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.StatusCode)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.StatusCode, e.Message)
}

type rateLimitError struct {
	wait time.Duration
	msg  string
}

func (e *rateLimitError) Error() string {
	return e.msg
}

func (c *Client) ListAccountWorkflows(ctx context.Context, opts ListOptions) (*ListResult, error) {
	owner := strings.TrimSpace(opts.Owner)
	if owner == "" {
		return nil, errors.New("owner is required")
	}
	lastRunMode := normalizeLastRunMode(opts.LastRunMode)
	report := func(event ProgressEvent) {
		if opts.Progress == nil {
			return
		}
		event.Rate = c.Rate()
		event.Requests = c.Requests()
		event.CacheHits = c.CacheHits()
		opts.Progress(event)
	}
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 6 {
		concurrency = 6
	}

	report(ProgressEvent{Phase: "listing repositories"})
	repos, err := c.listReposForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	repos = filterRepos(repos, opts)
	report(ProgressEvent{
		Phase:         "scanning workflows",
		ReposTotal:    len(repos),
		LastRunsTotal: lastRunTotal(lastRunMode, len(repos), 0),
	})

	result := &ListResult{
		Owner: owner,
		Repos: repos,
	}

	jobs := make(chan Repo)
	var wg sync.WaitGroup
	var mu sync.Mutex
	reposDone := 0
	lastRunsDone := 0
	lastRunsTotal := lastRunTotal(lastRunMode, len(repos), 0)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repo := range jobs {
				mu.Lock()
				startEvent := ProgressEvent{
					Phase:         "scanning workflows",
					ReposTotal:    len(repos),
					ReposDone:     reposDone,
					Workflows:     len(result.Items),
					LastRunsDone:  lastRunsDone,
					LastRunsTotal: lastRunsTotal,
					RepoErrors:    len(result.RepoErrors),
					CurrentRepo:   repo.FullName,
				}
				mu.Unlock()
				report(startEvent)
				workflows, err := c.ListWorkflows(ctx, repo.FullName)
				items := make([]WorkflowItem, 0, len(workflows))
				repoErrors := make([]RepoError, 0, 1)
				if err == nil {
					items, repoErrors = c.workflowItemsWithLastRuns(ctx, repo, workflows, lastRunMode)
				}
				mu.Lock()
				if err != nil {
					result.RepoErrors = append(result.RepoErrors, RepoError{
						Repo:  repo,
						Error: err.Error(),
					})
				} else {
					result.Items = append(result.Items, items...)
					result.RepoErrors = append(result.RepoErrors, repoErrors...)
				}
				reposDone++
				switch lastRunMode {
				case LastRunRepo:
					lastRunsDone++
				case LastRunWorkflow:
					lastRunsDone += len(workflows)
					lastRunsTotal += len(workflows)
				}
				event := ProgressEvent{
					Phase:         "scanning workflows",
					ReposTotal:    len(repos),
					ReposDone:     reposDone,
					Workflows:     len(result.Items),
					LastRunsDone:  lastRunsDone,
					LastRunsTotal: lastRunsTotal,
					RepoErrors:    len(result.RepoErrors),
					CurrentRepo:   repo.FullName,
				}
				mu.Unlock()
				report(event)
			}
		}()
	}
	for _, repo := range repos {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		case jobs <- repo:
		}
	}
	close(jobs)
	wg.Wait()

	sort.Slice(result.Items, func(i, j int) bool {
		a, b := result.Items[i], result.Items[j]
		if a.Repo.FullName != b.Repo.FullName {
			return a.Repo.FullName < b.Repo.FullName
		}
		if a.Workflow.Name != b.Workflow.Name {
			return strings.ToLower(a.Workflow.Name) < strings.ToLower(b.Workflow.Name)
		}
		return a.Workflow.ID < b.Workflow.ID
	})
	sort.Slice(result.RepoErrors, func(i, j int) bool {
		return result.RepoErrors[i].Repo.FullName < result.RepoErrors[j].Repo.FullName
	})

	result.Rate = c.Rate()
	result.Requests = c.Requests()
	result.CacheHits = c.CacheHits()
	return result, nil
}

func (c *Client) workflowItemsWithLastRuns(ctx context.Context, repo Repo, workflows []Workflow, mode string) ([]WorkflowItem, []RepoError) {
	items := make([]WorkflowItem, 0, len(workflows))
	lastRuns := make(map[int64]time.Time)
	var repoErrors []RepoError

	switch mode {
	case LastRunRepo:
		runs, err := c.ListRecentWorkflowRuns(ctx, repo.FullName, 100)
		if err != nil {
			repoErrors = append(repoErrors, RepoError{Repo: repo, Error: "last runs: " + err.Error()})
		} else {
			for _, run := range runs {
				if run.WorkflowID == 0 {
					continue
				}
				at := run.LastRunAt()
				if at.IsZero() {
					continue
				}
				if prev, ok := lastRuns[run.WorkflowID]; !ok || at.After(prev) {
					lastRuns[run.WorkflowID] = at
				}
			}
		}
	case LastRunWorkflow:
		for _, workflow := range workflows {
			run, ok, err := c.LatestWorkflowRun(ctx, repo.FullName, workflow.ID)
			if err != nil {
				repoErrors = append(repoErrors, RepoError{Repo: repo, Error: "last run " + workflow.Name + ": " + err.Error()})
				continue
			}
			if ok {
				lastRuns[workflow.ID] = run.LastRunAt()
			}
		}
	}

	for _, workflow := range workflows {
		items = append(items, WorkflowItem{
			Repo:      repo,
			Workflow:  workflow,
			LastRunAt: lastRuns[workflow.ID],
		})
	}
	return items, repoErrors
}

func (c *Client) ListWorkflows(ctx context.Context, fullName string) ([]Workflow, error) {
	repoPath, err := escapedRepoPath(fullName)
	if err != nil {
		return nil, err
	}
	var out []Workflow
	path := "/repos/" + repoPath + "/actions/workflows"
	query := url.Values{"per_page": {"100"}}
	for {
		var page struct {
			TotalCount int        `json:"total_count"`
			Workflows  []Workflow `json:"workflows"`
		}
		next, err := c.getJSONPage(ctx, path, query, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Workflows...)
		if next == "" {
			break
		}
		path, query = splitAPIPathAndQuery(next)
	}
	return out, nil
}

func (c *Client) ListRecentWorkflowRuns(ctx context.Context, fullName string, perPage int) ([]WorkflowRun, error) {
	repoPath, err := escapedRepoPath(fullName)
	if err != nil {
		return nil, err
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	var page struct {
		TotalCount   int           `json:"total_count"`
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	path := "/repos/" + repoPath + "/actions/runs"
	query := url.Values{
		"per_page": {strconv.Itoa(perPage)},
	}
	_, err = c.getJSONPage(ctx, path, query, &page)
	if err != nil {
		return nil, err
	}
	return page.WorkflowRuns, nil
}

func (c *Client) LatestWorkflowRun(ctx context.Context, fullName string, workflowID int64) (WorkflowRun, bool, error) {
	repoPath, err := escapedRepoPath(fullName)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	var page struct {
		TotalCount   int           `json:"total_count"`
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	path := "/repos/" + repoPath + "/actions/workflows/" + formatWorkflowID(workflowID) + "/runs"
	query := url.Values{"per_page": {"1"}}
	_, err = c.getJSONPage(ctx, path, query, &page)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	if len(page.WorkflowRuns) == 0 {
		return WorkflowRun{}, false, nil
	}
	return page.WorkflowRuns[0], true, nil
}

func (c *Client) DisableWorkflow(ctx context.Context, fullName string, workflowID int64) error {
	repoPath, err := escapedRepoPath(fullName)
	if err != nil {
		return err
	}
	path := "/repos/" + repoPath + "/actions/workflows/" + formatWorkflowID(workflowID) + "/disable"
	if _, _, err = c.do(ctx, http.MethodPut, path, nil, nil, false); err != nil {
		return err
	}
	c.invalidateCachedPages("/repos/"+repoPath+"/actions/workflows", url.Values{"per_page": {"100"}})
	return nil
}

func (c *Client) Rate() RateInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rate
}

func (c *Client) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *Client) CacheHits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cacheHits
}

func (c *Client) listReposForOwner(ctx context.Context, owner string) ([]Repo, error) {
	authUser, _ := c.authenticatedUser(ctx)
	if strings.EqualFold(authUser.Login, owner) {
		return c.listRepos(ctx, "/user/repos", url.Values{
			"affiliation": {"owner"},
			"visibility":  {"all"},
			"sort":        {"updated"},
			"direction":   {"desc"},
			"per_page":    {"100"},
		})
	}

	account, err := c.account(ctx, owner)
	if err != nil {
		return nil, err
	}
	if account.Type == "Organization" {
		return c.listRepos(ctx, "/orgs/"+url.PathEscape(owner)+"/repos", url.Values{
			"type":      {"all"},
			"sort":      {"updated"},
			"direction": {"desc"},
			"per_page":  {"100"},
		})
	}
	return c.listRepos(ctx, "/users/"+url.PathEscape(owner)+"/repos", url.Values{
		"type":      {"owner"},
		"sort":      {"updated"},
		"direction": {"desc"},
		"per_page":  {"100"},
	})
}

func (c *Client) AuthenticatedUser(ctx context.Context) (Owner, error) {
	return c.authenticatedUser(ctx)
}

func (c *Client) authenticatedUser(ctx context.Context) (Owner, error) {
	c.mu.Lock()
	if c.authUser != nil {
		user := *c.authUser
		c.mu.Unlock()
		return user, nil
	}
	c.mu.Unlock()

	var out Owner
	_, err := c.getJSON(ctx, "/user", nil, &out)
	if err != nil {
		return out, err
	}
	c.mu.Lock()
	c.authUser = &out
	c.mu.Unlock()
	return out, err
}

func (c *Client) account(ctx context.Context, owner string) (Owner, error) {
	var out Owner
	_, err := c.getJSON(ctx, "/users/"+url.PathEscape(owner), nil, &out)
	return out, err
}

func (c *Client) listRepos(ctx context.Context, path string, query url.Values) ([]Repo, error) {
	var out []Repo
	for {
		var page []Repo
		next, err := c.getJSONPage(ctx, path, query, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		path, query = splitAPIPathAndQuery(next)
	}
	return out, nil
}

func filterRepos(repos []Repo, opts ListOptions) []Repo {
	filter := strings.ToLower(strings.TrimSpace(opts.RepoFilter))
	out := make([]Repo, 0, len(repos))
	for _, repo := range repos {
		if repo.Disabled {
			continue
		}
		if !opts.IncludeArchived && repo.Archived {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(repo.FullName), filter) {
			continue
		}
		out = append(out, repo)
		if opts.MaxRepos > 0 && len(out) >= opts.MaxRepos {
			break
		}
	}
	return out
}

func normalizeLastRunMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", LastRunWorkflow, "exact":
		return LastRunWorkflow
	case LastRunOff, "none", "false":
		return LastRunOff
	case LastRunRepo, "fast", "approx", "approximate":
		return LastRunRepo
	default:
		return LastRunWorkflow
	}
}

func lastRunTotal(mode string, repos, workflows int) int {
	switch mode {
	case LastRunRepo:
		return repos
	case LastRunWorkflow:
		return workflows
	default:
		return 0
	}
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) (string, error) {
	return c.getJSONPage(ctx, path, query, out)
}

func (c *Client) getJSONPage(ctx context.Context, path string, query url.Values, out any) (string, error) {
	body, header, err := c.do(ctx, http.MethodGet, path, query, nil, true)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", err
	}
	return parseNextLink(header.Get("Link")), nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, allowCache bool) ([]byte, http.Header, error) {
	fullURL, err := c.fullURL(path, query)
	if err != nil {
		return nil, nil, err
	}
	var cached *cacheEntry
	cacheKey := c.cacheKey(method, fullURL)
	if allowCache && method == http.MethodGet && c.cache != nil {
		if entry, err := c.cache.Get(cacheKey); err == nil {
			cached = entry
			if c.cacheMaxAge > 0 && time.Since(entry.FetchedAt) <= c.cacheMaxAge {
				c.recordCacheHit()
				return entry.Body, http.Header(entry.Header), nil
			}
		}
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.waitForPace(ctx); err != nil {
			return nil, nil, err
		}
		var reader io.Reader
		if len(body) > 0 {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "disable-workflows-tui")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if cached != nil && cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		respBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		c.recordResponse(resp)

		if resp.StatusCode == http.StatusNotModified && cached != nil {
			c.recordCacheHit()
			return cached.Body, http.Header(cached.Header), nil
		}
		if wait, ok := c.retryWait(resp); ok {
			lastErr = &rateLimitError{wait: wait, msg: rateLimitMessage(resp, wait)}
			if wait <= c.maxRateWait {
				if err := sleepContext(ctx, wait); err != nil {
					return nil, nil, err
				}
				continue
			}
			return nil, resp.Header, lastErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, resp.Header, newAPIError(method, fullURL, resp.StatusCode, respBody)
		}
		if allowCache && method == http.MethodGet && c.cache != nil {
			if etag := resp.Header.Get("ETag"); etag != "" {
				_ = c.cache.Put(cacheKey, cacheEntry{
					ETag:       etag,
					StatusCode: resp.StatusCode,
					Header:     httpHeader(resp.Header.Clone()),
					Body:       respBody,
					FetchedAt:  time.Now(),
				})
			}
		}
		return respBody, resp.Header, nil
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, errors.New("request failed")
}

func (c *Client) invalidateCachedPages(path string, query url.Values) {
	if c.cache == nil {
		return
	}
	for {
		fullURL, err := c.fullURL(path, query)
		if err != nil {
			return
		}
		key := c.cacheKey(http.MethodGet, fullURL)
		entry, _ := c.cache.Get(key)
		_ = c.cache.Delete(key)
		if entry == nil {
			return
		}
		next := parseNextLink(http.Header(entry.Header).Get("Link"))
		if next == "" {
			return
		}
		path, query = splitAPIPathAndQuery(next)
	}
}

func (c *Client) cacheKey(method, fullURL string) string {
	return method + " " + tokenFingerprint(c.token) + " " + fullURL
}

func (c *Client) fullURL(path string, query url.Values) (string, error) {
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		u, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		if query != nil {
			u.RawQuery = query.Encode()
		}
		return u.String(), nil
	}
	u, err := url.Parse(c.baseURL + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return "", err
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func (c *Client) waitForPace(ctx context.Context) error {
	if c.minInterval <= 0 {
		return nil
	}
	c.mu.Lock()
	now := time.Now()
	next := c.lastRequest.Add(c.minInterval)
	if !next.After(now) {
		c.lastRequest = now
		c.mu.Unlock()
		return nil
	}
	c.lastRequest = next
	c.mu.Unlock()
	return sleepContext(ctx, time.Until(next))
}

func (c *Client) recordResponse(resp *http.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.rate = RateInfo{
		Limit:     parseIntHeader(resp.Header.Get("X-RateLimit-Limit")),
		Remaining: parseIntHeader(resp.Header.Get("X-RateLimit-Remaining")),
		Used:      parseIntHeader(resp.Header.Get("X-RateLimit-Used")),
		Resource:  resp.Header.Get("X-RateLimit-Resource"),
	}
	if reset := parseIntHeader(resp.Header.Get("X-RateLimit-Reset")); reset > 0 {
		c.rate.Reset = time.Unix(int64(reset), 0)
	}
}

func (c *Client) recordCacheHit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cacheHits++
}

func (c *Client) retryWait(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		seconds, err := strconv.Atoi(retryAfter)
		if err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		reset := parseIntHeader(resp.Header.Get("X-RateLimit-Reset"))
		if reset > 0 {
			wait := time.Until(time.Unix(int64(reset), 0)) + time.Second
			if wait < time.Second {
				wait = time.Second
			}
			return wait, true
		}
	}
	return 0, false
}

func newAPIError(method, requestURL string, status int, body []byte) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	return &APIError{
		Method:     method,
		URL:        requestURL,
		StatusCode: status,
		Message:    payload.Message,
	}
}

func rateLimitMessage(resp *http.Response, wait time.Duration) string {
	reset := resp.Header.Get("X-RateLimit-Reset")
	if reset != "" {
		if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
			return fmt.Sprintf("GitHub rate limit reached; reset at %s; wait %s", time.Unix(ts, 0).Format(time.RFC3339), wait.Round(time.Second))
		}
	}
	return fmt.Sprintf("GitHub asked us to slow down; retry after %s", wait.Round(time.Second))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseNextLink(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	parts := strings.Split(linkHeader, ",")
	for _, part := range parts {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}
		isNext := false
		for _, section := range sections[1:] {
			if strings.TrimSpace(section) == `rel="next"` {
				isNext = true
				break
			}
		}
		if isNext {
			rawURL := strings.TrimSpace(sections[0])
			rawURL = strings.TrimPrefix(rawURL, "<")
			rawURL = strings.TrimSuffix(rawURL, ">")
			return rawURL
		}
	}
	return ""
}

func splitAPIPathAndQuery(next string) (string, url.Values) {
	u, err := url.Parse(next)
	if err != nil {
		return next, nil
	}
	return u.Path, u.Query()
}

func escapedRepoPath(fullName string) (string, error) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid repository name %q", fullName)
	}
	return url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

func formatWorkflowID(id int64) string {
	return strconv.FormatInt(id, 10)
}

func parseIntHeader(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func tokenFingerprint(token string) string {
	if token == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
