package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TelemetryRegistry is a thread-safe store for exchanging telemetry between ingestion worker and UI.
type TelemetryRegistry struct {
	mu              sync.RWMutex
	TotalConsumed   int64
	TotalProposed   int64
	OffsetLag       int64
	LastLatencyUs   int64
	AvgLatencyUs    int64
	ActiveOrderBook *OrderBook
	ProposedLogs    []string
	maxLogs         int
	startTime       time.Time
	// LLM source split: LLMProposals is events the Ollama pipeline actually decided;
	// RulesFallbacks is events that fell back to the keyword rules parser (LLM/embed
	// failure). Their ratio is the "LLM fallback ratio" surfaced on the System tab —
	// the 2026-07-05 forensics found 88% fallbacks, i.e. the fleet was barely using
	// the LLM at all.
	LLMProposals   int64
	RulesFallbacks int64
	// PregateSkips is events vetoed BEFORE the LLM call by the numeric pre-gate
	// (drawdown halt / family loss-limit / cooldown / near-expiry) — proposals the
	// executive would have rejected anyway, so each one is an Ollama slot saved.
	PregateSkips int64
}

func NewTelemetryRegistry() *TelemetryRegistry {
	return &TelemetryRegistry{
		ProposedLogs: make([]string, 0),
		maxLogs:      12,
		startTime:    time.Now(),
	}
}

func (tr *TelemetryRegistry) AddLatency(us int64) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.LastLatencyUs = us
	if tr.AvgLatencyUs == 0 {
		tr.AvgLatencyUs = us
	} else {
		// running average (alpha = 0.1)
		tr.AvgLatencyUs = int64(float64(tr.AvgLatencyUs)*0.9 + float64(us)*0.1)
	}
}

func (tr *TelemetryRegistry) AddProposalLog(logStr string) {
	// Stamp each activity line with a wall-clock time so the fleet activity feed
	// (and the TUI) show WHEN a gordon did something, not just the ordering.
	stamped := time.Now().Format("15:04:05") + " " + logStr
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.ProposedLogs = append([]string{stamped}, tr.ProposedLogs...)
	if len(tr.ProposedLogs) > tr.maxLogs {
		tr.ProposedLogs = tr.ProposedLogs[:tr.maxLogs]
	}
	if os.Getenv("HEADLESS") == "true" {
		log.Println(logStr)
	}
}

// RecentLogs returns a newest-first copy of the activity ring buffer — the per-
// gordon feed of what it just did (proposals emitted, declines, pre-gate skips,
// edge/skew/expiry vetoes, pauses). Exposed over /healthz for the Fleet Scorecard.
func (tr *TelemetryRegistry) RecentLogs() []string {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	out := make([]string, len(tr.ProposedLogs))
	copy(out, tr.ProposedLogs)
	return out
}

func (tr *TelemetryRegistry) SetOrderBook(ob *OrderBook) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.ActiveOrderBook = ob
}

func (tr *TelemetryRegistry) IncrementConsumed() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.TotalConsumed++
}

func (tr *TelemetryRegistry) IncrementProposed() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.TotalProposed++
}

// RecordProposalSource classifies one processed event by whether the Ollama
// pipeline produced the decision (fromLLM) or it fell back to the keyword rules
// parser. Feeds the LLM fallback-ratio health metric.
func (tr *TelemetryRegistry) RecordProposalSource(fromLLM bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if fromLLM {
		tr.LLMProposals++
	} else {
		tr.RulesFallbacks++
	}
}

// LLMSourceCounts returns the cumulative (LLM-decided, rules-fallback) event counts.
func (tr *TelemetryRegistry) LLMSourceCounts() (llm, fallback int64) {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	return tr.LLMProposals, tr.RulesFallbacks
}

// RecordPregateSkip counts one event vetoed before the LLM call by the numeric pre-gate.
func (tr *TelemetryRegistry) RecordPregateSkip() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.PregateSkips++
}

// PregateSkipCount returns the cumulative pre-LLM veto count.
func (tr *TelemetryRegistry) PregateSkipCount() int64 {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	return tr.PregateSkips
}

// HealthSnapshot returns a thread-safe copy of the headline telemetry counters.
func (tr *TelemetryRegistry) HealthSnapshot() (consumed, proposed, lag, avgLatencyUs int64, uptimeSeconds float64) {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	return tr.TotalConsumed, tr.TotalProposed, tr.OffsetLag, tr.AvgLatencyUs, time.Since(tr.startTime).Seconds()
}

func (tr *TelemetryRegistry) SetLag(lag int64) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.OffsetLag = lag
}

// Bubble Tea TUI Model
type tuiModel struct {
	registry *TelemetryRegistry
	width    int
	height   int
	lastTick time.Time
	err      error
	uptime   time.Duration

	// Local copy of telemetry for rendering
	consumed  int64
	lag       int64
	lastLat   int64
	avgLat    int64
	orderBook *OrderBook
	logLines  []string
}

func NewTUIModel(tr *TelemetryRegistry) *tuiModel {
	return &tuiModel{
		registry: tr,
		logLines: make([]string, 0),
	}
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *tuiModel) Init() tea.Cmd {
	return tickCmd()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		// Pull current state from thread-safe registry
		m.registry.mu.RLock()
		m.consumed = m.registry.TotalConsumed
		m.lag = m.registry.OffsetLag
		m.lastLat = m.registry.LastLatencyUs
		m.avgLat = m.registry.AvgLatencyUs
		m.orderBook = m.registry.ActiveOrderBook

		// Copy log lines
		m.logLines = make([]string, len(m.registry.ProposedLogs))
		copy(m.logLines, m.registry.ProposedLogs)
		m.registry.mu.RUnlock()

		m.uptime = time.Since(m.registry.startTime).Round(time.Second)
		m.lastTick = time.Time(msg)
		return m, tickCmd()
	}
	return m, nil
}

// Define Lip Gloss Color Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#4f46e5")).
			Padding(0, 1).
			Align(lipgloss.Center)

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#6366f1")).
			Padding(1)

	accentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#10b981")).
			Bold(true)

	alertStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ef4444")).
			Bold(true)

	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ca3af"))

	headerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f3f4f6")).
			Bold(true).
			Underline(true)

	bidStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981"))
	askStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))
)

func (m *tuiModel) View() string {
	if m.width < 40 {
		return "Terminal width is too narrow for TUI."
	}

	// 1. Title Header
	headerStr := titleStyle.Width(m.width - 2).Render(
		fmt.Sprintf("GORDON ENGINE // QUANTITATIVE ANALYSIS WORKER [Uptime: %v]", m.uptime),
	)

	// 2. Metrics Block (Left Panel)
	var metricsBuilder strings.Builder
	metricsBuilder.WriteString(headerStyle.Render("Telemetry & Engine Stats\n\n"))
	metricsBuilder.WriteString(fmt.Sprintf("Events Consumed : %s\n", accentStyle.Render(fmt.Sprintf("%d", m.consumed))))
	metricsBuilder.WriteString(fmt.Sprintf("Ingest Group Lag: %s events\n", alertStyle.Render(fmt.Sprintf("%d", m.lag))))
	metricsBuilder.WriteString(fmt.Sprintf("Processing Lat  : %s µs\n", accentStyle.Render(fmt.Sprintf("%d", m.lastLat))))
	metricsBuilder.WriteString(fmt.Sprintf("Avg Proc Latency: %s µs\n", accentStyle.Render(fmt.Sprintf("%d", m.avgLat))))
	metricsBuilder.WriteString(fmt.Sprintf("Engine Core CPU : %s\n", mutedStyle.Render("Active (OK)")))
	metricsBuilder.WriteString(fmt.Sprintf("Storage State   : %s\n", mutedStyle.Render("Redis Linked")))
	metricsBox := borderStyle.Width(m.width/2 - 2).Height(9).Render(metricsBuilder.String())

	// 3. Order Book Depth Visualizer (Right Panel)
	var obBuilder strings.Builder
	if m.orderBook == nil {
		obBuilder.WriteString(headerStyle.Render("Order Book depth\n\n"))
		obBuilder.WriteString("\n\nAwaiting ingestion events...\n")
	} else {
		obBuilder.WriteString(headerStyle.Render(fmt.Sprintf("Order Book: %s\n", m.orderBook.ContractID)))

		// Render asks high to low (reverse slice to visual top-to-bottom depth)
		for i := len(m.orderBook.Asks) - 1; i >= 0; i-- {
			ask := m.orderBook.Asks[i]
			bar := strings.Repeat("█", min(ask.Volume/50, 10))
			obBuilder.WriteString(fmt.Sprintf("  %s %-5.2f | Vol: %-4d %s\n", askStyle.Render("ASK"), ask.Price, ask.Volume, askStyle.Render(bar)))
		}

		obBuilder.WriteString("  ---------------------------------------\n")

		// Render bids
		for _, bid := range m.orderBook.Bids {
			bar := strings.Repeat("█", min(bid.Volume/50, 10))
			obBuilder.WriteString(fmt.Sprintf("  %s %-5.2f | Vol: %-4d %s\n", bidStyle.Render("BID"), bid.Price, bid.Volume, bidStyle.Render(bar)))
		}
	}
	obBox := borderStyle.Width(m.width/2 - 2).Height(12).Render(obBuilder.String())

	// 4. Ingestion & Outbound Proposed Log console (Bottom Panel)
	var logBuilder strings.Builder
	logBuilder.WriteString(headerStyle.Render("rolling strategy log (outbound topic: gordon.strategies.proposed)\n\n"))
	if len(m.logLines) == 0 {
		logBuilder.WriteString("No strategies proposed yet. Listening to incoming market channels...")
	} else {
		for _, line := range m.logLines {
			logBuilder.WriteString(line + "\n")
		}
	}
	logBox := borderStyle.Width(m.width - 2).Height(10).Render(logBuilder.String())

	// Assemble view
	topPanel := lipgloss.JoinHorizontal(lipgloss.Top, metricsBox, obBox)
	helpText := mutedStyle.Render("Press [q] or [Ctrl+C] to shut down worker and exit dashboard cleanly.")

	return lipgloss.JoinVertical(
		lipgloss.Left,
		headerStr,
		topPanel,
		logBox,
		helpText,
	)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
