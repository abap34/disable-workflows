package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/abap34/disable-workflows/internal/ghapi"
)

type Config struct {
	Owner           string
	RepoFilter      string
	IncludeArchived bool
	MaxRepos        int
	Concurrency     int
	LastRunMode     string
	TokenSource     string
}

type state int

const (
	stateLoading state = iota
	stateReady
	stateConfirm
	stateDisabling
	stateError
)

type sortKey int

const (
	sortRepo sortKey = iota
	sortWorkflow
	sortState
	sortVisibility
	sortLastRun
	sortUpdated
	sortPath
)

type Model struct {
	ctx    context.Context
	client *ghapi.Client
	cfg    Config

	state state
	err   error

	width  int
	height int
	offset int
	cursor int

	items      []ghapi.WorkflowItem
	repos      []ghapi.Repo
	repoErrors []ghapi.RepoError
	rate       ghapi.RateInfo
	requests   int
	cacheHits  int

	selected       map[string]bool
	filter         string
	filtering      bool
	sortKey        sortKey
	sortAsc        bool
	confirmInput   string
	status         string
	disableQueue   []ghapi.WorkflowItem
	disableIndex   int
	disableResults []disableResult
	loadCh         <-chan tea.Msg
	loadCancel     context.CancelFunc
	loadProgress   ghapi.ProgressEvent
}

type loadStartedMsg struct {
	ch     <-chan tea.Msg
	cancel context.CancelFunc
}

type loadMsg struct {
	result *ghapi.ListResult
	err    error
}

type loadProgressMsg struct {
	event ghapi.ProgressEvent
}

type loadStreamClosedMsg struct{}

type disableMsg struct {
	result disableResult
}

type disableResult struct {
	item ghapi.WorkflowItem
	err  error
}

func NewModel(ctx context.Context, client *ghapi.Client, cfg Config) Model {
	return Model{
		ctx:      ctx,
		client:   client,
		cfg:      cfg,
		state:    stateLoading,
		selected: make(map[string]bool),
		sortKey:  sortRepo,
		sortAsc:  true,
	}
}

func (m Model) Init() tea.Cmd {
	return m.startLoadCmd()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.clampCursor()
		return m, nil
	case loadStartedMsg:
		m.loadCh = msg.ch
		m.loadCancel = msg.cancel
		return m, waitLoadMsg(msg.ch)
	case loadProgressMsg:
		m.loadProgress = msg.event
		m.rate = msg.event.Rate
		m.requests = msg.event.Requests
		m.cacheHits = msg.event.CacheHits
		if m.loadCh != nil {
			return m, waitLoadMsg(m.loadCh)
		}
		return m, nil
	case loadStreamClosedMsg:
		if m.state == stateLoading {
			m.state = stateError
			m.err = context.Canceled
		}
		m.loadCh = nil
		m.loadCancel = nil
		return m, nil
	case loadMsg:
		m.loadCh = nil
		m.loadCancel = nil
		if msg.err != nil {
			m.state = stateError
			m.err = msg.err
			return m, nil
		}
		m.state = stateReady
		m.items = msg.result.Items
		m.repos = msg.result.Repos
		m.repoErrors = msg.result.RepoErrors
		m.rate = msg.result.Rate
		m.requests = msg.result.Requests
		m.cacheHits = msg.result.CacheHits
		m.selected = keepSelections(m.selected, m.items)
		m.status = fmt.Sprintf("Loaded %d workflows from %d repositories", len(m.items), len(m.repos))
		if len(m.repoErrors) > 0 {
			m.status += fmt.Sprintf("; %d repositories had errors", len(m.repoErrors))
		}
		m.clampCursor()
		return m, nil
	case disableMsg:
		m.disableResults = append(m.disableResults, msg.result)
		m.rate = m.client.Rate()
		m.requests = m.client.Requests()
		m.cacheHits = m.client.CacheHits()
		if msg.result.err == nil {
			delete(m.selected, msg.result.item.Key())
			m.markDisabled(msg.result.item.Key())
		}
		m.disableIndex++
		if m.disableIndex < len(m.disableQueue) {
			return m, m.disableOneCmd(m.disableQueue[m.disableIndex])
		}
		m.state = stateReady
		m.status = summarizeDisableResults(m.disableResults)
		m.disableQueue = nil
		m.disableIndex = 0
		m.confirmInput = ""
		m.clampCursor()
		return m, nil
	case tea.KeyMsg:
		switch m.state {
		case stateLoading, stateDisabling:
			if msg.String() == "ctrl+c" {
				if m.loadCancel != nil {
					m.loadCancel()
				}
				return m, tea.Quit
			}
			return m, nil
		case stateError:
			if msg.String() == "q" || msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			if msg.String() == "r" {
				m.state = stateLoading
				m.err = nil
				m.loadProgress = ghapi.ProgressEvent{}
				return m, m.startLoadCmd()
			}
			return m, nil
		case stateConfirm:
			return m.updateConfirm(msg)
		default:
			return m.updateReady(msg)
		}
	}
	return m, nil
}

func (m Model) updateReady(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch msg.Type {
		case tea.KeyEsc, tea.KeyEnter:
			m.filtering = false
		case tea.KeyBackspace:
			if len(m.filter) > 0 {
				m.filter = dropLastRune(m.filter)
				m.cursor = 0
				m.offset = 0
			}
		case tea.KeyRunes:
			m.filter += string(msg.Runes)
			m.cursor = 0
			m.offset = 0
		case tea.KeyCtrlC:
			return m, tea.Quit
		}
		m.clampCursor()
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup":
		m.cursor -= m.tableHeight()
	case "pgdown":
		m.cursor += m.tableHeight()
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = len(m.visibleIndexes()) - 1
	case " ", "enter":
		m.toggleCursor()
	case "a":
		for _, idx := range m.visibleIndexes() {
			if m.items[idx].CanDisable() {
				m.selected[m.items[idx].Key()] = true
			}
		}
	case "u":
		for key := range m.selected {
			delete(m.selected, key)
		}
	case "/":
		m.filtering = true
	case "]":
		m.nextSortKey()
	case "[":
		m.prevSortKey()
	case "o":
		m.sortAsc = !m.sortAsc
	case "1":
		m.setSortKey(sortState)
	case "2":
		m.setSortKey(sortVisibility)
	case "3":
		m.setSortKey(sortRepo)
	case "4":
		m.setSortKey(sortWorkflow)
	case "5":
		m.setSortKey(sortLastRun)
	case "6":
		m.setSortKey(sortUpdated)
	case "7":
		m.setSortKey(sortPath)
	case "esc":
		if m.filter != "" {
			m.filter = ""
			m.cursor = 0
			m.offset = 0
		}
	case "d":
		queue := m.selectedItems()
		if len(queue) == 0 {
			m.status = "No active workflows selected"
			return m, nil
		}
		m.state = stateConfirm
		m.confirmInput = ""
	case "r":
		if m.loadCancel != nil {
			m.loadCancel()
		}
		m.state = stateLoading
		m.status = ""
		m.loadProgress = ghapi.ProgressEvent{}
		return m, m.startLoadCmd()
	}
	m.clampCursor()
	return m, nil
}

func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.state = stateReady
		m.confirmInput = ""
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEnter:
		if m.confirmInput == "disable" {
			m.disableQueue = m.selectedItems()
			m.disableIndex = 0
			m.disableResults = nil
			m.state = stateDisabling
			if len(m.disableQueue) == 0 {
				m.state = stateReady
				m.status = "No active workflows selected"
				return m, nil
			}
			return m, m.disableOneCmd(m.disableQueue[0])
		}
	case tea.KeyBackspace:
		if len(m.confirmInput) > 0 {
			m.confirmInput = dropLastRune(m.confirmInput)
		}
	case tea.KeyRunes:
		m.confirmInput += string(msg.Runes)
	}
	return m, nil
}

func (m Model) View() string {
	if m.width == 0 {
		return "Starting...\n"
	}
	switch m.state {
	case stateLoading:
		return m.loadingView()
	case stateError:
		return m.frame(errorStyle.Render("Error: "+m.err.Error()) + "\n\nr refresh  q quit\n")
	case stateConfirm:
		return m.confirmView()
	case stateDisabling:
		return m.disablingView()
	default:
		return m.readyView()
	}
}

func (m Model) loadingView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Loading GitHub repositories and workflows"))
	b.WriteString("\n\n")
	b.WriteString(m.configSummary())
	b.WriteString("\n\n")
	b.WriteString(m.loadingProgressView())
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("ctrl+c quit"))
	return m.frame(b.String())
}

func (m Model) loadingProgressView() string {
	progress := m.loadProgress
	phase := progress.Phase
	if phase == "" {
		phase = "starting"
	}
	var b strings.Builder
	b.WriteString(activeStyle.Render(phase))
	b.WriteString("\n")
	if progress.ReposTotal > 0 {
		b.WriteString(progressBar(progress.ReposDone, progress.ReposTotal, m.width-8))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("%d / %d repositories scanned  workflows: %d  errors: %d",
			progress.ReposDone,
			progress.ReposTotal,
			progress.Workflows,
			progress.RepoErrors,
		))
		b.WriteString("\n")
		if progress.LastRunsTotal > 0 {
			b.WriteString(fmt.Sprintf("last-run lookups: %d / %d\n", progress.LastRunsDone, progress.LastRunsTotal))
		}
		if progress.CurrentRepo != "" {
			b.WriteString(dimStyle.Render("current: " + progress.CurrentRepo))
			b.WriteString("\n")
		}
	} else {
		b.WriteString(dimStyle.Render("Fetching repository list..."))
		b.WriteString("\n")
	}
	if progress.Rate.Limit > 0 {
		reset := "-"
		if !progress.Rate.Reset.IsZero() {
			reset = progress.Rate.Reset.Local().Format("15:04:05")
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("rate: %d/%d reset %s  requests: %d cache: %d",
			progress.Rate.Remaining,
			progress.Rate.Limit,
			reset,
			progress.Requests,
			progress.CacheHits,
		)))
	}
	return b.String()
}

func (m Model) readyView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("GitHub Actions Workflow Disabler"))
	b.WriteString("\n")
	b.WriteString(m.summaryLine())
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("sort: " + m.sortLabel()))
	b.WriteString("\n")
	if m.filtering {
		b.WriteString(activeStyle.Render("filter: " + m.filter))
	} else if m.filter != "" {
		b.WriteString(activeStyle.Render("filter: " + m.filter))
	} else {
		b.WriteString(dimStyle.Render("filter: / to search"))
	}
	b.WriteString("\n\n")
	b.WriteString(m.tableView())
	b.WriteString("\n")
	if m.status != "" {
		b.WriteString(dimStyle.Render(m.status))
		b.WriteString("\n")
	}
	if len(m.repoErrors) > 0 {
		b.WriteString(errorStyle.Render(m.repoErrorSummary()))
		b.WriteString("\n")
	}
	b.WriteString(helpStyle.Render("up/down move  space select  a select-visible  u clear  / filter  1-7 sort  [/] sort  o order  d disable  r refresh  q quit"))
	return m.frame(b.String())
}

func (m Model) confirmView() string {
	queue := m.selectedItems()
	var b strings.Builder
	b.WriteString(titleStyle.Render("Confirm Disable"))
	b.WriteString("\n")
	b.WriteString(errorStyle.Render(fmt.Sprintf("This will disable %d selected workflows.", len(queue))))
	b.WriteString("\n\n")
	for i, item := range queue {
		if i >= 10 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("... and %d more", len(queue)-i)))
			b.WriteString("\n")
			break
		}
		b.WriteString(fmt.Sprintf("- %s / %s (%s)\n", item.Repo.FullName, item.Workflow.Name, item.Workflow.Path))
	}
	b.WriteString("\nType ")
	b.WriteString(activeStyle.Render("disable"))
	b.WriteString(" and press enter, or esc to cancel.\n")
	b.WriteString("> ")
	b.WriteString(m.confirmInput)
	return m.frame(b.String())
}

func (m Model) disablingView() string {
	total := len(m.disableQueue)
	done := len(m.disableResults)
	current := ""
	if m.disableIndex < total {
		item := m.disableQueue[m.disableIndex]
		current = fmt.Sprintf("\nCurrent: %s / %s", item.Repo.FullName, item.Workflow.Name)
	}
	return m.frame(fmt.Sprintf("%s\n\n%d / %d complete%s\n\n%s",
		titleStyle.Render("Disabling workflows"),
		done,
		total,
		current,
		helpStyle.Render("ctrl+c quit"),
	))
}

func (m Model) tableView() string {
	visible := m.visibleIndexes()
	if len(visible) == 0 {
		return dimStyle.Render("No workflows match the current filter.") + "\n"
	}
	height := m.tableHeight()
	if height < 1 {
		height = 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+height {
		m.offset = m.cursor - height + 1
	}
	end := m.offset + height
	if end > len(visible) {
		end = len(visible)
	}

	widths := m.columnWidths()
	var b strings.Builder
	b.WriteString(m.headerRow(widths))
	b.WriteString("\n")
	for rowPos, idx := range visible[m.offset:end] {
		item := m.items[idx]
		cursor := m.offset+rowPos == m.cursor
		line := m.renderRow(item, widths, cursor)
		b.WriteString(line)
		b.WriteString("\n")
	}
	if end < len(visible) {
		b.WriteString(dimStyle.Render(fmt.Sprintf("... %d more", len(visible)-end)))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderRow(item ghapi.WorkflowItem, widths colWidths, cursor bool) string {
	selected := "[ ]"
	if m.selected[item.Key()] {
		selected = "[x]"
	}
	if !item.CanDisable() {
		selected = " - "
	}
	visibility := visibilityLabel(item.Repo.Private)
	updated := "-"
	if !item.Workflow.UpdatedAt.IsZero() {
		updated = item.Workflow.UpdatedAt.Local().Format("2006-01-02")
	}
	lastRun := "-"
	if !item.LastRunAt.IsZero() {
		if widths.lastRun <= 10 {
			lastRun = item.LastRunAt.Local().Format("2006-01-02")
		} else {
			lastRun = item.LastRunAt.Local().Format("2006-01-02 15:04")
		}
	}
	values := []string{
		fit(selected, widths.selectCol),
		fit(item.Workflow.State, widths.state),
		fit(visibility, widths.visibility),
		fit(item.Repo.FullName, widths.repo),
		fit(item.Workflow.Name, widths.workflow),
	}
	if widths.lastRun > 0 {
		values = append(values, fit(lastRun, widths.lastRun))
	}
	values = append(values, fit(updated, widths.updated))
	if widths.path > 0 {
		values = append(values, fit(item.Workflow.Path, widths.path))
	}
	line := strings.Join(values, " ")
	switch {
	case cursor:
		return cursorStyle.Render(line)
	case !item.CanDisable():
		return dimStyle.Render(line)
	case item.Workflow.State == "active":
		return line
	default:
		return dimStyle.Render(line)
	}
}

func (m Model) summaryLine() string {
	active, disabled := 0, 0
	for _, item := range m.items {
		if item.CanDisable() {
			active++
		} else {
			disabled++
		}
	}
	selected := len(m.selectedItems())
	rate := "rate: unknown"
	if m.rate.Limit > 0 {
		reset := "-"
		if !m.rate.Reset.IsZero() {
			reset = m.rate.Reset.Local().Format("15:04:05")
		}
		rate = fmt.Sprintf("rate: %d/%d reset %s", m.rate.Remaining, m.rate.Limit, reset)
	}
	return fmt.Sprintf("owner: %s  repos: %d  workflows: %d  active: %d  other: %d  selected: %d  %s  requests: %d cache: %d",
		m.cfg.Owner,
		len(m.repos),
		len(m.items),
		active,
		disabled,
		selected,
		rate,
		m.requests,
		m.cacheHits,
	)
}

func (m Model) configSummary() string {
	parts := []string{
		"owner: " + m.cfg.Owner,
		"token: " + m.cfg.TokenSource,
		fmt.Sprintf("concurrency: %d", m.cfg.Concurrency),
		"last run: " + m.cfg.LastRunMode,
	}
	if m.cfg.RepoFilter != "" {
		parts = append(parts, "repo filter: "+m.cfg.RepoFilter)
	}
	if m.cfg.MaxRepos > 0 {
		parts = append(parts, fmt.Sprintf("max repos: %d", m.cfg.MaxRepos))
	}
	return strings.Join(parts, "\n")
}

func (m Model) repoErrorSummary() string {
	if len(m.repoErrors) == 0 {
		return ""
	}
	first := m.repoErrors[0]
	return fmt.Sprintf("repo errors: %d; first: %s: %s", len(m.repoErrors), first.Repo.FullName, first.Error)
}

func (m Model) frame(s string) string {
	return lipgloss.NewStyle().Width(m.width).Height(m.height).Render(s)
}

func (m *Model) clampCursor() {
	visible := m.visibleIndexes()
	if len(visible) == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(visible) {
		m.cursor = len(visible) - 1
	}
	height := m.tableHeight()
	if height < 1 {
		height = 1
	}
	if m.offset > m.cursor {
		m.offset = m.cursor
	}
	if m.offset < 0 {
		m.offset = 0
	}
	if m.cursor >= m.offset+height {
		m.offset = m.cursor - height + 1
	}
}

func (m Model) tableHeight() int {
	height := m.height - 10
	if m.status != "" {
		height--
	}
	if len(m.repoErrors) > 0 {
		height--
	}
	if height < 3 {
		return 3
	}
	return height
}

func (m Model) visibleIndexes() []int {
	filter := strings.ToLower(strings.TrimSpace(m.filter))
	indexes := make([]int, 0, len(m.items))
	for i, item := range m.items {
		if filter == "" || itemMatches(item, filter) {
			indexes = append(indexes, i)
		}
	}
	sort.SliceStable(indexes, func(i, j int) bool {
		cmp := compareItems(m.items[indexes[i]], m.items[indexes[j]], m.sortKey)
		if cmp == 0 {
			cmp = compareItems(m.items[indexes[i]], m.items[indexes[j]], sortRepo)
			if cmp == 0 {
				cmp = compareItems(m.items[indexes[i]], m.items[indexes[j]], sortWorkflow)
			}
		}
		if m.sortAsc {
			return cmp < 0
		}
		return cmp > 0
	})
	return indexes
}

func compareItems(a, b ghapi.WorkflowItem, key sortKey) int {
	switch key {
	case sortWorkflow:
		return strings.Compare(strings.ToLower(a.Workflow.Name), strings.ToLower(b.Workflow.Name))
	case sortState:
		return strings.Compare(a.Workflow.State, b.Workflow.State)
	case sortVisibility:
		return strings.Compare(visibilityLabel(a.Repo.Private), visibilityLabel(b.Repo.Private))
	case sortLastRun:
		return compareTime(a.LastRunAt, b.LastRunAt)
	case sortUpdated:
		return compareTime(a.Workflow.UpdatedAt, b.Workflow.UpdatedAt)
	case sortPath:
		return strings.Compare(a.Workflow.Path, b.Workflow.Path)
	default:
		return strings.Compare(a.Repo.FullName, b.Repo.FullName)
	}
}

func compareTime(a, b time.Time) int {
	switch {
	case a.IsZero() && b.IsZero():
		return 0
	case a.IsZero():
		return -1
	case b.IsZero():
		return 1
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	default:
		return 0
	}
}

func itemMatches(item ghapi.WorkflowItem, filter string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		item.Repo.FullName,
		item.Workflow.Name,
		item.Workflow.Path,
		item.Workflow.State,
	}, " "))
	return strings.Contains(haystack, filter)
}

func (m Model) selectedItems() []ghapi.WorkflowItem {
	items := make([]ghapi.WorkflowItem, 0, len(m.selected))
	for _, item := range m.items {
		if m.selected[item.Key()] && item.CanDisable() {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Repo.FullName != items[j].Repo.FullName {
			return items[i].Repo.FullName < items[j].Repo.FullName
		}
		return items[i].Workflow.Name < items[j].Workflow.Name
	})
	return items
}

func (m *Model) toggleCursor() {
	visible := m.visibleIndexes()
	if len(visible) == 0 || m.cursor >= len(visible) {
		return
	}
	item := m.items[visible[m.cursor]]
	if !item.CanDisable() {
		m.status = "Only active workflows in non-archived repositories can be selected"
		return
	}
	key := item.Key()
	if m.selected[key] {
		delete(m.selected, key)
	} else {
		m.selected[key] = true
	}
}

func (m *Model) nextSortKey() {
	m.sortKey = (m.sortKey + 1) % sortKeyCount()
	m.cursor = 0
	m.offset = 0
}

func (m *Model) setSortKey(key sortKey) {
	if m.sortKey == key {
		m.sortAsc = !m.sortAsc
	} else {
		m.sortKey = key
	}
	m.cursor = 0
	m.offset = 0
}

func (m *Model) prevSortKey() {
	if m.sortKey == 0 {
		m.sortKey = sortKeyCount() - 1
	} else {
		m.sortKey--
	}
	m.cursor = 0
	m.offset = 0
}

func (m Model) sortLabel() string {
	dir := "asc"
	if !m.sortAsc {
		dir = "desc"
	}
	return sortKeyLabel(m.sortKey) + " " + dir
}

func sortKeyCount() sortKey {
	return sortPath + 1
}

func sortKeyLabel(key sortKey) string {
	switch key {
	case sortWorkflow:
		return "workflow"
	case sortState:
		return "state"
	case sortVisibility:
		return "visibility"
	case sortLastRun:
		return "last run"
	case sortUpdated:
		return "updated"
	case sortPath:
		return "path"
	default:
		return "repository"
	}
}

func (m *Model) markDisabled(key string) {
	for i := range m.items {
		if m.items[i].Key() == key {
			m.items[i].Workflow.State = "disabled_manually"
			return
		}
	}
}

func keepSelections(selected map[string]bool, items []ghapi.WorkflowItem) map[string]bool {
	valid := make(map[string]bool, len(items))
	for _, item := range items {
		if selected[item.Key()] && item.CanDisable() {
			valid[item.Key()] = true
		}
	}
	return valid
}

func summarizeDisableResults(results []disableResult) string {
	ok, failed := 0, 0
	for _, result := range results {
		if result.err != nil {
			failed++
		} else {
			ok++
		}
	}
	if failed == 0 {
		return fmt.Sprintf("Disabled %d workflows", ok)
	}
	return fmt.Sprintf("Disabled %d workflows; %d failed", ok, failed)
}

func (m Model) startLoadCmd() tea.Cmd {
	cfg := m.cfg
	client := m.client
	parentCtx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(parentCtx)
		ch := make(chan tea.Msg, 64)
		go func() {
			defer close(ch)
			result, err := client.ListAccountWorkflows(ctx, ghapi.ListOptions{
				Owner:           cfg.Owner,
				RepoFilter:      cfg.RepoFilter,
				IncludeArchived: cfg.IncludeArchived,
				MaxRepos:        cfg.MaxRepos,
				Concurrency:     cfg.Concurrency,
				LastRunMode:     cfg.LastRunMode,
				Progress: func(event ghapi.ProgressEvent) {
					select {
					case ch <- loadProgressMsg{event: event}:
					default:
					}
				},
			})
			select {
			case ch <- loadMsg{result: result, err: err}:
			case <-ctx.Done():
			}
		}()
		return loadStartedMsg{ch: ch, cancel: cancel}
	}
}

func waitLoadMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return loadStreamClosedMsg{}
		}
		return msg
	}
}

func (m Model) disableOneCmd(item ghapi.WorkflowItem) tea.Cmd {
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		err := client.DisableWorkflow(ctx, item.Repo.FullName, item.Workflow.ID)
		return disableMsg{result: disableResult{item: item, err: err}}
	}
}

type colWidths struct {
	selectCol  int
	state      int
	visibility int
	repo       int
	workflow   int
	lastRun    int
	updated    int
	path       int
}

func (m Model) columnWidths() colWidths {
	total := m.width - 1
	if total < 34 {
		total = 34
	}
	widths := colWidths{
		selectCol:  3,
		state:      17,
		visibility: 3,
		lastRun:    16,
		updated:    10,
	}
	if total < 100 {
		widths.state = 12
		widths.lastRun = 10
	}
	if total < 70 {
		widths.state = 8
		widths.lastRun = 0
	}
	spaces := 5
	if widths.lastRun > 0 {
		spaces++
	}
	fixed := widths.selectCol + widths.state + widths.visibility + widths.lastRun + widths.updated + spaces
	flexible := total - fixed
	if flexible < 0 {
		flexible = 0
	}
	if total >= 100 {
		widths.repo = flexible * 28 / 100
		widths.workflow = flexible * 32 / 100
		widths.path = flexible - widths.repo - widths.workflow
	} else {
		widths.repo = flexible * 45 / 100
		widths.workflow = flexible - widths.repo
		widths.path = 0
	}
	return widths
}

func (m Model) headerRow(widths colWidths) string {
	values := []string{
		fit("Sel", widths.selectCol),
		fit(m.sortHeader("State", sortState, widths.state), widths.state),
		fit(m.sortHeader("Vis", sortVisibility, widths.visibility), widths.visibility),
		fit(m.sortHeader("Repository", sortRepo, widths.repo), widths.repo),
		fit(m.sortHeader("Workflow", sortWorkflow, widths.workflow), widths.workflow),
	}
	if widths.lastRun > 0 {
		values = append(values, fit(m.sortHeader("Last Run", sortLastRun, widths.lastRun), widths.lastRun))
	}
	values = append(values, fit(m.sortHeader("Updated", sortUpdated, widths.updated), widths.updated))
	if widths.path > 0 {
		values = append(values, fit(m.sortHeader("Path", sortPath, widths.path), widths.path))
	}
	return headerStyle.Render(strings.Join(values, " "))
}

func visibilityLabel(private bool) string {
	if private {
		return "prv"
	}
	return "pub"
}

func (m Model) sortHeader(label string, key sortKey, width int) string {
	if m.sortKey != key || width <= 1 {
		return label
	}
	marker := "^"
	if !m.sortAsc {
		marker = "v"
	}
	return label + marker
}

func fit(s string, width int) string {
	runes := []rune(s)
	if width <= 0 {
		return ""
	}
	if len(runes) > width {
		if width <= 3 {
			return string(runes[:width])
		}
		return string(runes[:width-3]) + "..."
	}
	return s + strings.Repeat(" ", width-len(runes))
}

func dropLastRune(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	return string(runes[:len(runes)-1])
}

func progressBar(done, total, maxWidth int) string {
	if total <= 0 {
		return ""
	}
	width := maxWidth
	if width > 48 {
		width = 48
	}
	if width < 12 {
		width = 12
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)
