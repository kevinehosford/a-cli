package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	axiom "github.com/axiomhq/axiom-go/axiom"
	axiomQuery "github.com/axiomhq/axiom-go/axiom/query"
	"github.com/charmbracelet/bubbles/spinner"
	table "github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/timer"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	asciigraph "github.com/guptarohit/asciigraph"
	slices "golang.org/x/exp/slices"
)

const (
	TYPING = iota
	QUERYING
	REFRESHING
)

var COLORS = []asciigraph.AnsiColor{
	asciigraph.Blue,
	asciigraph.Magenta,
	asciigraph.Cyan,
	asciigraph.Green,
	asciigraph.Yellow,
	asciigraph.Red,
	asciigraph.AliceBlue,
	asciigraph.Cornsilk,
	asciigraph.Crimson,
	asciigraph.DarkViolet,
	asciigraph.DeepPink,
	asciigraph.Gold,
	asciigraph.Indigo,
	asciigraph.Lavender,
	asciigraph.LightCoral,
	asciigraph.LightSalmon,
}

var PULSE_STEP_COLORS = []string{
	"#432155",
	"#4e2667",
	"#5f2d84",
	"#7938b2",
	"#8e4ec6",
	"#9d5bd2",
	"#8e4ec6",
	"#7938b2",
	"#5f2d84",
	"#4e2667",
}

var tableStyle = lipgloss.NewStyle().Padding(1)

type errMsg error

type Model struct {
	ready                      bool
	textarea                   textarea.Model
	spinner                    spinner.Model
	state                      int
	client                     *axiom.Client
	query                      *Query
	msg                        string
	matchesTable               *table.Model
	matchesTableHighlightedIdx int
	graphs                     *[]GraphData
	queryMeta                  *QueryMeta
	otherMsg                   string
	totalsTable                *table.Model
	highlightedGroup           string
	refreshTimeout             int
	pulseStep                  int
}

type Query struct {
	apl    string
	result *axiomQuery.Result
	err    error
}

type Op struct {
	name string
}

type QueryMeta struct {
	orderedGroupKeys []string
	opsCount         int
	intervals        int
	groups           []string
	ops              []Op
	groupColors      map[string]asciigraph.AnsiColor
}

type GraphData struct {
	title  string
	data   [][]float64
	colors []asciigraph.AnsiColor
}

// weird general message
type Msg struct {
	update func(m *Model)
}

type ResultMsg struct {
	apl    string
	result *axiomQuery.Result
	err    error
}

type RefreshMsg timer.TickMsg
type ReRunMsg struct{}
type PulseMsg struct{}

func initSpinner() spinner.Model {
	spin := spinner.New()
	spin.Spinner = spinner.Dot
	return spin
}

func initialModel() Model {
	ti := textarea.New()
	ti.SetWidth(100)

	ti.Placeholder = "Enter an APL query..."
	ti.Focus()

	client, err := axiom.NewClient(
	// axiom.SetPersonalTokenConfig("AXIOM_TOKEN", "AXIOM_ORG_ID"),
	// axiom.SetURL("AXIOM_URL"),
	)

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	return Model{
		textarea: ti,
		spinner:  initSpinner(),
		state:    TYPING,
		client:   client,
		query: &Query{
			apl: "",
		},
		pulseStep: 9,
	}
}

func (m *Model) RunQuery(apl string) tea.Cmd {
	m.setMsg("Running query...")
	m.setState(QUERYING)

	return tea.Batch(spinner.Tick, func() tea.Msg {

		ctx := context.Background()
		res, err := m.client.Query(ctx, apl)

		return ResultMsg{
			apl:    apl,
			result: res,
			err:    err,
		}
	})
}

func (m *Model) HighlightRow(row table.Row) tea.Cmd {
	return func() tea.Msg {
		return Msg{
			update: func(m *Model) {
				// m.otherMsg = fmt.Sprintf("row: %v", row[0])

				// have to reconstruct the group key omg
				group := map[string]any{}

				for i, key := range m.queryMeta.orderedGroupKeys {
					group[key] = row[i]
				}

				groupKey := getGroupKey(m.queryMeta.orderedGroupKeys, group)

				m.highlightedGroup = groupKey

				m.UpdateGraphs(m.query.result)
				// m.UpdateTotals(m.query.result)
			},
		}
	}
}

func (m *Model) UpdateQuery(msg ResultMsg) {
	// set the query data
	m.query = &Query{
		apl:    msg.apl,
		result: msg.result,
		err:    msg.err,
	}
}

func (m *Model) UpdateQueryMeta(result *axiomQuery.Result) {
	if result == nil || len(result.Buckets.Series) == 0 {
		m.queryMeta = nil

		return
	}

	var opsCount = 0
	var orderedGroupKeys []string = []string{}
	var intervals = len(result.Buckets.Series)
	var groups = []string{}

	// get the length of the series
	// iterate over result.Buckets.Series
	for _, interval := range result.Buckets.Series {
		for _, group := range interval.Groups {
			if len(orderedGroupKeys) == 0 && len(group.Group) > 0 {
				// iterate over group.Group keys
				for key := range group.Group {
					orderedGroupKeys = append(orderedGroupKeys, key)
				}

				// sort orderedGroupKeys
				sort.Strings(orderedGroupKeys)
			}

			if (len(group.Aggregations)) > opsCount {
				opsCount = len(group.Aggregations)
			}

			key := getGroupKey(orderedGroupKeys, group.Group)

			// if key is not in groups append it
			if !stringInSlice(key, groups) {
				groups = append(groups, key)
			}
		}
	}

	var ops = []Op{}

	for _, total := range result.Buckets.Totals {
		for _, aggregation := range total.Aggregations {
			idx := slices.IndexFunc(ops, func(op Op) bool {
				return op.name == aggregation.Alias
			})

			if idx == -1 {
				ops = append(ops, Op{
					name: aggregation.Alias,
				})
			}
		}
	}

	sort.Strings(groups)

	groupColors := map[string]asciigraph.AnsiColor{}

	for _, group := range groups {
		hashed := hash(group)
		colorIdx := hashed % len(COLORS)

		groupColors[group] = COLORS[colorIdx]
	}

	m.queryMeta = &QueryMeta{
		orderedGroupKeys: orderedGroupKeys,
		opsCount:         opsCount,
		intervals:        intervals,
		groups:           groups,
		ops:              ops,
		groupColors:      groupColors,
	}
}

func (m *Model) UpdateMatchesTable(result *axiomQuery.Result) {
	m.matchesTableHighlightedIdx = -1

	if result == nil || len(result.Matches) == 0 {
		m.matchesTable = nil
	} else {

		columns := []table.Column{
			{
				Title: "_time",
				Width: 20,
			},
		}

		var data = result.Matches[0].Data

		// iterate over all the keys in data
		for k := range data {
			columns = append(columns, table.Column{
				Title: k,
				Width: 10,
			})
		}

		rows := []table.Row{}

		// iterate over all of result.Matches
		for _, match := range result.Matches {
			row := table.Row{}

			row = append(row, match.Time.String())

			// iterate over all the columns
			for _, column := range columns[1:] {
				var value = match.Data[column.Title]

				switch value.(type) {
				case string:
					row = append(row, value.(string))
				case int:
					row = append(row, fmt.Sprintf("%v", value.(int)))
				case float64:
					row = append(row, fmt.Sprintf("%v", value.(float64)))
				default:
					row = append(row, fmt.Sprintf("%v", value))
				}
			}

			// append a table.Row to rows
			rows = append(rows, row)
		}

		t := table.New(
			table.WithColumns(columns),
			table.WithRows(rows),
			table.WithFocused(true),
			table.WithHeight(20),
		)

		s := table.DefaultStyles()
		s.Selected = lipgloss.NewStyle()
		t.SetStyles(s)

		m.matchesTable = &t
	}
}

func (m *Model) UpdateMatchesHighlight(inc int) {
	nextIdx := m.matchesTableHighlightedIdx + inc

	if nextIdx < 0 {
		nextIdx = 0
	} else if nextIdx >= len(m.matchesTable.Rows()) {
		nextIdx = len(m.matchesTable.Rows()) - 1
	}

	m.matchesTableHighlightedIdx = nextIdx
}

func (m *Model) UpdateGraphs(result *axiomQuery.Result) {
	if result == nil || len(result.Buckets.Series) == 0 {
		m.graphs = nil
		return
	}

	data := make([][]float64, 4)

	for i := 0; i < 4; i++ {
		for x := -20; x <= 20; x++ {
			v := math.NaN()
			if r := 20 - i; x >= -r && x <= r {
				v = math.Sqrt(math.Pow(float64(r), 2)-math.Pow(float64(x), 2)) / 2
			}

			data[i] = append(data[i], v)
		}
	}

	graphs := m.makeGraphs() // One for each aggregation

	// for each Interval in result.Buckets.Series
	for intervalIdx, interval := range result.Buckets.Series {
		// for each EntryGroup in Interval.Groups
		for _, group := range interval.Groups {
			groupKey := getGroupKey(m.queryMeta.orderedGroupKeys, group.Group)

			// m.otherMsg = fmt.Sprintf("groupKey: %v groups: %v", groupKey, m.queryMeta.groups)

			// get the index of groupKey in m.queryMeta.groupKeys
			graphsDataIdx := sort.SearchStrings(m.queryMeta.groups, groupKey)

			// for each Aggregation in EntryGroup.Aggregations
			for graphIdx, aggregation := range group.Aggregations {
				intervalValue := math.NaN()
				// check if aggregation.Value is a float64
				// if not set it to NaN
				if _, ok := aggregation.Value.(float64); ok {
					intervalValue = aggregation.Value.(float64)
				}

				graph := graphs[graphIdx]
				data := graph.data[graphsDataIdx]
				data[intervalIdx] = intervalValue
			}
		}
	}

	m.graphs = &graphs
}

func (m *Model) UpdateTotals(result *axiomQuery.Result) {
	if result == nil || len(result.Buckets.Totals) == 0 {
		m.totalsTable = nil
		return
	}

	columns := []table.Column{}

	for _, orderedKey := range m.queryMeta.orderedGroupKeys {
		columns = append(columns, table.Column{
			Title: orderedKey,
			Width: 20,
		})
	}

	for _, op := range m.queryMeta.ops {
		columns = append(columns, table.Column{
			Title: op.name,
			Width: 20,
		})
	}

	rows := []table.Row{}

	for _, total := range result.Buckets.Totals {
		row := table.Row{}
		for _, orderedKey := range m.queryMeta.orderedGroupKeys {
			row = append(row, fmt.Sprintf("%v", total.Group[orderedKey]))
		}

		for _, aggregation := range total.Aggregations {
			row = append(row, fmt.Sprintf("%v", aggregation.Value))
		}

		rows = append(rows, row)
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
	)

	s := table.DefaultStyles()
	s.Selected = lipgloss.NewStyle()
	t.SetStyles(s)

	t.Blur() // Blur because first key press should focus / highlight

	m.totalsTable = &t
}

func (m *Model) SetRefreshing() tea.Cmd {
	m.refreshTimeout = 5 // seconds
	m.setState(REFRESHING)

	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return RefreshMsg{}
	})
}

func (m *Model) UpdateRefreshing() tea.Cmd {
	m.refreshTimeout -= 1

	return tea.Tick(time.Second, func(t time.Time) tea.Msg {

		if (m.refreshTimeout) <= 1 {
			return ReRunMsg{}
		} else {
			return RefreshMsg{}
		}
	})
}

func (m *Model) UpdatePulse() tea.Cmd {
	if m.pulseStep <= 0 {
		m.pulseStep = 9
	} else {
		m.pulseStep -= 1
	}

	return tea.Tick(
		// Send a pulse update every 150ms
		time.Millisecond*150, func(t time.Time) tea.Msg {

			return PulseMsg{}
		})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		cmds []tea.Cmd
		cmd  tea.Cmd
	)

	switch msg := msg.(type) {
	case tea.KeyMsg:

		if !m.ready {
			m.ready = true

			return m, tea.Batch(cmds...)
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		default:
			switch m.state {
			case TYPING:
				switch msg.String() {
				case "enter":
					query := strings.TrimSpace(m.textarea.Value())

					// debug
					// if query == "" {
					// 	query = "[\"axiom-traces-dev\"] | where _time > ago(5m) | summarize avg(duration), count(), dcount(trace_id) by bin_auto(_time), ['service.name']"
					// }

					if query != "" {
						cmds = append(cmds, m.RunQuery(query))
					}
				default:
					m.textarea, cmd = m.textarea.Update(msg)
					cmds = append(cmds, cmd)
				}
			case REFRESHING:
				switch msg.String() {
				case "esc":
					if !m.textarea.Focused() {
						m.textarea.Focus()
					}
					cmds = append(cmds, textarea.Blink)

					m.setState(TYPING)

				default:
					if m.totalsTable != nil {
						if !m.totalsTable.Focused() {
							m.totalsTable.Focus()
							cmds = append(cmds, m.HighlightRow(m.totalsTable.SelectedRow()))
						} else {
							totalsTable, cmd := m.totalsTable.Update(msg)
							m.totalsTable = &totalsTable
							cmds = append(cmds, cmd)
							cmds = append(cmds, m.HighlightRow(m.totalsTable.SelectedRow()))
						}
					} else if m.matchesTable != nil {
						matchesTable, cmd := m.matchesTable.Update(msg)
						m.matchesTable = &matchesTable
						cmds = append(cmds, cmd)

						switch msg.String() {
						case "down":
							m.UpdateMatchesHighlight(1)
						case "up":
							m.UpdateMatchesHighlight(-1)
						}
					}

				}
			}

		}
	case ResultMsg:
		m.textarea.Blur()
		m.highlightedGroup = ""
		m.UpdateQuery(msg)
		m.UpdateQueryMeta(msg.result)
		m.UpdateMatchesTable(msg.result)
		m.UpdateTotals(msg.result)
		m.UpdateGraphs(msg.result)
		cmd = m.SetRefreshing()
		cmds = append(cmds, cmd)

	case Msg:
		msg.update(&m)
	case spinner.TickMsg:
		switch m.state {
		case QUERYING:
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
	case RefreshMsg:
		switch m.state {
		case REFRESHING:
			cmds = append(cmds, m.UpdateRefreshing())
		}
	case ReRunMsg:
		switch m.state {
		case REFRESHING:
			cmds = append(cmds, m.RunQuery(m.query.apl))
		}
	case PulseMsg:
		if !m.ready {
			cmds = append(cmds, m.UpdatePulse())
		}
	default:
		if m.textarea.Focused() {
			m.textarea, cmd = m.textarea.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m Model) ViewMatches() string {
	if m.matchesTable == nil {
		return ""
	}

	if m.matchesTableHighlightedIdx != -1 {
		s := table.DefaultStyles()
		s.Selected = s.Selected.
			Foreground(lipgloss.Color("229")).
			Background(lipgloss.Color("57")).
			Bold(false)
		m.matchesTable.SetStyles(s)
	}

	return tableStyle.Render(m.matchesTable.View())
}

func (m Model) ViewTotals() string {
	if m.totalsTable == nil {
		return ""
	}

	if m.highlightedGroup != "" {
		s := table.DefaultStyles()
		s.Selected = s.Selected.Foreground(lipgloss.Color("229")).
			Background(lipgloss.Color("57")).
			Bold(false)

		m.totalsTable.SetStyles(s)
	}

	return tableStyle.Render(m.totalsTable.View())
}

func (m Model) ViewGraphs() string {
	if m.graphs == nil {
		return ""
	}

	graphWidth := 50
	graphHeight := 10

	focusedModelStyle := lipgloss.NewStyle().
		Width(graphWidth+15).
		Height(graphHeight).
		Align(lipgloss.Left, lipgloss.Top).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("69"))

	var plots []string = []string{}

	for _, graph := range *m.graphs {
		styledGraph := focusedModelStyle.Render(asciigraph.PlotMany(graph.data, asciigraph.Precision(0), asciigraph.SeriesColors(
			graph.colors...,
		), asciigraph.Height(graphHeight), asciigraph.Width(graphWidth), asciigraph.Caption(graph.title)))

		plots = append(plots, styledGraph)
	}

	return tableStyle.Render(lipgloss.JoinHorizontal(
		lipgloss.Left,
		plots...,
	))
}

func (m Model) ViewSpinner() string {
	if m.state != QUERYING {
		return ""
	}

	return lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View(), "Running query...")
}

func (m Model) ViewRefreshTimeout() string {
	if m.state != REFRESHING {
		return ""
	}

	return fmt.Sprintf("Refresh in %v", m.refreshTimeout)
}

func (m Model) ViewMatchDetails() string {
	if m.matchesTable == nil || m.matchesTableHighlightedIdx == -1 {
		return ""
	}

	str, _ := json.MarshalIndent(m.query.result.Matches[m.matchesTableHighlightedIdx], "", "  ")

	return tableStyle.Render(string(str))
}

func (m *Model) ViewSplashScreen() string {

	splashStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(PULSE_STEP_COLORS[m.pulseStep]))

	return splashStyle.Render(`
	█████  ██   ██ ██  ██████  ███    ███ 
	██   ██  ██ ██  ██ ██    ██ ████  ████ 
	███████   ███   ██ ██    ██ ██ ████ ██ 
	██   ██  ██ ██  ██ ██    ██ ██  ██  ██ 
	██   ██ ██   ██ ██  ██████  ██      ██ `)
}

func (m *Model) ViewError() string {
	if m.query.err == nil {
		return ""
	}

	return fmt.Sprintf("Error: %v", m.query.err)
}

func (m Model) View() string {
	if !m.ready {
		return m.ViewSplashScreen()
	}

	parts := []string{
		tableStyle.Render(lipgloss.JoinHorizontal(lipgloss.Left, m.ViewSpinner(), m.ViewRefreshTimeout())),
		tableStyle.Render(m.textarea.View()),
	}

	parts = appendIfNotEmpty(parts, m.ViewError())
	parts = appendIfNotEmpty(parts, m.ViewGraphs())
	parts = appendIfNotEmpty(parts, m.ViewTotals())
	parts = appendIfNotEmpty(parts, m.ViewMatches())
	parts = appendIfNotEmpty(parts, m.ViewMatchDetails())

	finalPlot := lipgloss.JoinVertical(
		lipgloss.Left,
		parts...,
	)

	return finalPlot
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(tea.EnterAltScreen, func() tea.Msg {
		return PulseMsg{}
	}, textarea.Blink)
}

func (m *Model) setMsg(msg string) {
	m.msg = msg
}

func (m *Model) setState(state int) {
	m.state = state
}

func stringInSlice(str string, slice []string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func appendIfNotEmpty(slice []string, str string) []string {
	if str != "" {
		slice = append(slice, str)
	}

	return slice
}

func (m *Model) makeGraphs() []GraphData {
	queryMeta := *m.queryMeta

	graphs := []GraphData{}

	// each op is a graph
	for _, op := range queryMeta.ops {
		data := make([][]float64, len(queryMeta.groups)) // One for each "series"

		for i := range data {
			data[i] = make([]float64, queryMeta.intervals)
		}

		seriesColors := []asciigraph.AnsiColor{}

		for _, group := range queryMeta.groups {
			color := queryMeta.groupColors[group]

			if m.highlightedGroup != "" && group != m.highlightedGroup {
				color = asciigraph.SlateGray
			}

			seriesColors = append(seriesColors, color)
		}

		graphs = append(graphs, GraphData{
			title:  op.name,
			data:   data,
			colors: seriesColors,
		})
	}

	return graphs
}

func getGroupKey(orderedGroupKeys []string, group map[string]interface{}) string {
	var keyVals []string = []string{}

	for _, k := range orderedGroupKeys {
		keyVals = append(keyVals, fmt.Sprintf("%v", group[k]))
	}

	return strings.Join(keyVals, ", ")
}

func hash(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	sum := h.Sum32()

	return int(sum)
}
