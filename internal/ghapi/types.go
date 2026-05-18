package ghapi

import "time"

type Owner struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type Repo struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	FullName      string     `json:"full_name"`
	Private       bool       `json:"private"`
	Archived      bool       `json:"archived"`
	Disabled      bool       `json:"disabled"`
	DefaultBranch string     `json:"default_branch"`
	PushedAt      *time.Time `json:"pushed_at"`
	Owner         Owner      `json:"owner"`
}

type Workflow struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	URL       string    `json:"url"`
	HTMLURL   string    `json:"html_url"`
	BadgeURL  string    `json:"badge_url"`
}

type WorkflowRun struct {
	ID           int64      `json:"id"`
	WorkflowID   int64      `json:"workflow_id"`
	Status       string     `json:"status"`
	Conclusion   *string    `json:"conclusion"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
	HTMLURL      string     `json:"html_url"`
}

func (r WorkflowRun) LastRunAt() time.Time {
	if r.RunStartedAt != nil && !r.RunStartedAt.IsZero() {
		return *r.RunStartedAt
	}
	if !r.CreatedAt.IsZero() {
		return r.CreatedAt
	}
	return r.UpdatedAt
}

type WorkflowItem struct {
	Repo      Repo
	Workflow  Workflow
	LastRunAt time.Time
}

func (i WorkflowItem) Key() string {
	return i.Repo.FullName + "#" + formatWorkflowID(i.Workflow.ID)
}

func (i WorkflowItem) CanDisable() bool {
	return i.Workflow.State == "active" && !i.Repo.Archived && !i.Repo.Disabled
}

type RepoError struct {
	Repo  Repo
	Error string
}

type RateInfo struct {
	Limit     int
	Remaining int
	Reset     time.Time
	Used      int
	Resource  string
}

type ListOptions struct {
	Owner           string
	RepoFilter      string
	IncludeArchived bool
	MaxRepos        int
	Concurrency     int
	LastRunMode     string
	Progress        func(ProgressEvent)
}

type ListResult struct {
	Owner      string
	Repos      []Repo
	Items      []WorkflowItem
	RepoErrors []RepoError
	Rate       RateInfo
	Requests   int
	CacheHits  int
}

type ProgressEvent struct {
	Phase         string
	ReposTotal    int
	ReposDone     int
	Workflows     int
	LastRunsDone  int
	LastRunsTotal int
	RepoErrors    int
	CurrentRepo   string
	Rate          RateInfo
	Requests      int
	CacheHits     int
}
