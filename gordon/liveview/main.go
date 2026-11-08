package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

// IcebergOrder represents a single chunked transaction.
type IcebergOrder struct {
	ContractID string    `json:"contract_id"`
	Action     string    `json:"action"`
	Size       int       `json:"size"`
	Price      float64   `json:"price"`
	DelayMS    int       `json:"delay_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// StrategyProposal represents the outbound quantitative proposal.
type StrategyProposal struct {
	ContractID      string         `json:"contract_id"`
	Action          string         `json:"action"`
	AveragePrice    float64        `json:"average_price"`
	TotalSize       int            `json:"total_size"`
	SlippagePct     float64        `json:"slippage_pct"`
	LimitBreached   bool           `json:"limit_breached"`
	BreachReason    string         `json:"breach_reason,omitempty"`
	IcebergOrders   []IcebergOrder `json:"iceberg_orders"`
	Timestamp       time.Time      `json:"timestamp"`
	EventHash       string         `json:"event_hash"`
	ProcessingTime  string         `json:"processing_time"`
	ProposalText    string         `json:"proposal_text,omitempty"`
	SliceBoundaries []int          `json:"slice_boundaries,omitempty"`
	Persona         string         `json:"persona,omitempty"`
	SourceID        string         `json:"source_id,omitempty"`
	SourceURL       string         `json:"source_url,omitempty"`
	ModelUsed       string         `json:"model_used,omitempty"`
	EmbedLatencyMS  int64          `json:"embed_latency_ms,omitempty"`
	QdrantLatencyMS int64          `json:"qdrant_latency_ms,omitempty"`
	LLMLatencyMS    int64          `json:"llm_latency_ms,omitempty"`
}

// Styles
var (
	okBorder = lipgloss.Border{
		Top:         "─",
		Bottom:      "─",
		Left:        "│",
		Right:       "│",
		TopLeft:     "╭",
		TopRight:    "╮",
		BottomLeft:  "╰",
		BottomRight: "╯",
	}

	okStyle = lipgloss.NewStyle().
		Border(okBorder).
		BorderForeground(lipgloss.Color("#10b981")). // emerald green
		Padding(0, 2).
		Margin(1, 0)

	breachedStyle = lipgloss.NewStyle().
			Border(okBorder).
			BorderForeground(lipgloss.Color("#ef4444")). // bright red
			Padding(0, 2).
			Margin(1, 0)

	buyBadge = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#10b981")). // emerald
			Padding(0, 1)

	sellBadge = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#ef4444")). // red
			Padding(0, 1)

	contractStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#4f46e5")). // indigo
			Padding(0, 1)

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9ca3af"))

	valStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#f3f4f6"))

	warnValStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#f59e0b")) // amber/orange

	breachValStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ef4444")) // red

	breachReasonStyle = lipgloss.NewStyle().
				Italic(true).
				Foreground(lipgloss.Color("#f87171")) // soft red

	rawLogStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6b7280")) // greyed out for logs that aren't strategy JSONs

	timeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#818cf8")) // soft purple
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed dashboard.js
var dashboardJS string

var (
	proposals   []StrategyProposal
	proposalsMu sync.RWMutex
	clients     []chan string
	clientsMu   sync.Mutex
	redisClient *redis.Client
)

func broadcastProposal(p StrategyProposal) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	proposalsBroadcast.Add(1)

	proposalsMu.Lock()
	proposals = append([]StrategyProposal{p}, proposals...)
	if len(proposals) > 50 {
		proposals = proposals[:50]
	}
	proposalsMu.Unlock()

	clientsMu.Lock()
	defer clientsMu.Unlock()
	for _, ch := range clients {
		select {
		case ch <- string(data):
		default:
		}
	}
}

func handleProposals(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	proposalsMu.RLock()
	defer proposalsMu.RUnlock()
	_ = json.NewEncoder(w).Encode(proposals)
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Accel-Buffering", "no") // Disable Nginx response buffering for SSE

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := make(chan string, 10)
	clientsMu.Lock()
	clients = append(clients, ch)
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		for i, c := range clients {
			if c == ch {
				clients = append(clients[:i], clients[i+1:]...)
				break
			}
		}
		clientsMu.Unlock()
	}()

	fmt.Fprint(w, "event: open\ndata: connection established\n\n")
	flusher.Flush()

	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, dashboardHTML)
}

func handleJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, dashboardJS)
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

// renderCards controls whether full TUI box-art proposal cards are printed to
// stdout. Defaults on only when stdout is a real terminal; when liveview runs
// headless under run.sh (stdout redirected to logs/liveview.log) the box-art
// otherwise bloats the log to hundreds of MB/day, so we emit one compact line
// per proposal instead. Force either mode with LIVEVIEW_RENDER_CARDS=true|false.
var renderCards = detectRenderCards()

func detectRenderCards() bool {
	if v, ok := os.LookupEnv("LIVEVIEW_RENDER_CARDS"); ok {
		return v == "1" || strings.EqualFold(v, "true")
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// logProposalLine writes a single-line summary of a proposal, used in headless
// mode in place of the multi-line box-art card.
func logProposalLine(p StrategyProposal) {
	status := "ok"
	if p.LimitBreached {
		status = "BREACHED"
		if p.BreachReason != "" {
			status += ": " + p.BreachReason
		}
	}
	model := p.ModelUsed
	if model == "" {
		model = "-"
	}
	log.Printf("[PROPOSAL] %s %s size=%d avg=%.3f slip=%.2f%% model=%s %s",
		p.ContractID, strings.ToUpper(p.Action), p.TotalSize, p.AveragePrice, p.SlippagePct, model, status)
}

func main() {
	brokersEnv := getEnv("KAFKA_BROKERS", "localhost:9092")
	brokers := strings.Split(brokersEnv, ",")
	topic := getEnv("KAFKA_OUTBOUND_TOPIC", "gordon.strategies.proposed")

	brokersFlag := flag.String("brokers", "", "Kafka seed brokers (comma-separated). Overrides KAFKA_BROKERS env.")
	topicFlag := flag.String("topic", "", "Kafka topic to consume. Overrides KAFKA_OUTBOUND_TOPIC env.")
	delaySec := flag.Int("delay", 0, "Display delay in seconds to hold each proposal on a cleared screen.")
	flag.Parse()

	if *brokersFlag != "" {
		brokers = strings.Split(*brokersFlag, ",")
	}
	for i, b := range brokers {
		brokers[i] = strings.TrimSpace(b)
	}
	if *topicFlag != "" {
		topic = *topicFlag
	}

	groupName := fmt.Sprintf("liveview-group-%d", time.Now().UnixNano())

	opts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(groupName),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()), // Show new real-time messages only
	}

	cl, err := kgo.NewClient(opts...)
	if err != nil {
		log.Fatalf("Failed to create Kafka client: %v", err)
	}
	defer cl.Close()

	// Connect to Redis
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	redisClient = redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	log.Printf("Liveview connected to Redis at: %s\n", redisAddr)

	// Spin up HTTP web server for dashboard
	port := getEnv("PORT", "8082")
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/", handleDashboard)
		mux.HandleFunc("/dashboard.js", handleJS)
		mux.HandleFunc("/api/proposals", handleProposals)
		mux.HandleFunc("/api/stream", handleStream)
		mux.HandleFunc("/api/positions", handlePositions)
		mux.HandleFunc("/api/ledger", handleLedger)
		mux.HandleFunc("/api/proposals/pending", handlePendingProposal)
		mux.HandleFunc("/api/proposals/approve", handleApprove)
		mux.HandleFunc("/api/proposals/deny", handleDeny)
		mux.HandleFunc("/api/controls", handleControls)
		mux.HandleFunc("/api/controls/toggle", handleToggleAuto)
		mux.HandleFunc("/api/controls/model", handleModelControls)
		mux.HandleFunc("/api/controls/priority", handleGordonPriority)
		mux.HandleFunc("/api/gordons", handleGordons)
		mux.HandleFunc("/api/gordons/reset", handleGordonsReset)
		mux.HandleFunc("/api/personas", handlePersonas)
		mux.HandleFunc("/api/book_metrics", handleBookMetrics)
		mux.HandleFunc("/api/calibration", handleCalibration)
		mux.HandleFunc("/api/run/config", handleRunConfig)
		mux.HandleFunc("/healthz", handleHealthz)
		mux.HandleFunc("/api/status", handleStatus)
		mux.HandleFunc("/api/chat", handleChat)
		mux.HandleFunc("/api/account", handleAccount)
		mux.HandleFunc("/api/equity_curve", handleEquityCurve)
		mux.HandleFunc("/api/trades", handleTrades)
		mux.HandleFunc("/api/trmnl", handleTRMNL)
		mux.HandleFunc("/api/run/reset", handleRunReset)
		mux.HandleFunc("/api/run/export", handleRunExport)
		mux.HandleFunc("/api/run/bundle", handleRunBundle)
		mux.HandleFunc("/api/lou/lessons", handleLouLessons)
		mux.HandleFunc("/api/lou/autonomy", handleLouAutonomy)
		mux.HandleFunc("/api/run/liquidate", handleRunLiquidate)
		mux.HandleFunc("/api/logs/stream", handleLogsStream)
		mux.HandleFunc("/api/logs/snapshot", handleLogsSnapshot)
		mux.HandleFunc("/api/ledger/stats", handleLedgerStats)

		log.Printf("Starting liveview HTTP server on port %s...", port)
		if err := http.ListenAndServe(":"+port, mux); err != nil {
			log.Printf("Liveview HTTP server error: %v", err)
		}
	}()

	go startLogTailer(getEnv("LOG_DIR", ""))

	ctx := context.Background()
	log.Printf("Connecting to Kafka brokers: %v\n", brokers)
	log.Printf("Listening to topic: %s (Consumer Group: %s)\n", topic, groupName)
	log.Println("Awaiting live quantitative strategy proposals...")

	for {
		fetches := cl.PollRecords(ctx, 1)
		if fetches.IsClientClosed() {
			break
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, err := range errs {
				log.Printf("Kafka poll error: %v\n", err.Err)
			}
		}

		iter := fetches.RecordIter()
		for !iter.Done() {
			record := iter.Next()
			var proposal StrategyProposal
			if err := json.Unmarshal(record.Value, &proposal); err == nil && proposal.ContractID != "" {
				if *delaySec > 0 {
					// ANSI escape codes to clear screen and reset cursor position
					fmt.Print("\033[H\033[2J")
				}
				renderProposal(proposal)
				broadcastProposal(proposal)
				if *delaySec > 0 {
					time.Sleep(time.Duration(*delaySec) * time.Second)
				}
			} else if renderCards {
				// Print other payload formats in a dimmed style (TTY only; the
				// styled output is just ANSI noise in a redirected log file).
				fmt.Println(rawLogStyle.Render(string(record.Value)))
			}
		}
	}
}

func renderProposal(p StrategyProposal) {
	// In headless/server mode skip the box-art card (it bloats the log) and emit a
	// single compact line. The dashboard still receives the full proposal via
	// broadcastProposal in the consume loop.
	if !renderCards {
		logProposalLine(p)
		return
	}

	// 1. Determine action badge
	var actionStr string
	if p.Action == "BUY" {
		actionStr = buyBadge.Render(" BUY ")
	} else {
		actionStr = sellBadge.Render(" SELL ")
	}

	// 2. Prepare left column details
	var col1Builder strings.Builder
	col1Builder.WriteString(fmt.Sprintf("%s  %s\n\n", actionStr, contractStyle.Render(" "+p.ContractID+" ")))
	col1Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Total Size:"), valStyle.Render(fmt.Sprintf("%d units", p.TotalSize))))
	col1Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Avg Fill Price:"), valStyle.Render(fmt.Sprintf("$%.4f", p.AveragePrice))))

	slippageColor := valStyle
	if p.SlippagePct > 0.02 {
		slippageColor = warnValStyle
	}
	if p.SlippagePct > 0.03 {
		slippageColor = breachValStyle
	}
	col1Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Slippage:"), slippageColor.Render(fmt.Sprintf("%.4f%%", p.SlippagePct*100))))

	// 3. Prepare right column details
	var col2Builder strings.Builder
	statusStr := okStyle.BorderForeground(lipgloss.Color("#10b981")).Render("✓ RISK OK")
	if p.LimitBreached {
		statusStr = breachedStyle.BorderForeground(lipgloss.Color("#ef4444")).Render("⚠ BREACHED")
	}
	// Align visually
	col2Builder.WriteString(fmt.Sprintf("%s\n\n", statusStr))
	col2Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Latency:"), valStyle.Render(p.ProcessingTime)))

	formattedTime := p.Timestamp.Local().Format("15:04:05.000")
	col2Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Timestamp:"), timeStyle.Render(formattedTime)))

	shortHash := p.EventHash
	if len(shortHash) > 12 {
		shortHash = shortHash[:12]
	}
	col2Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Event Hash:"), valStyle.Render(shortHash)))

	if p.SourceID != "" {
		col2Builder.WriteString(fmt.Sprintf("%-15s %s\n", labelStyle.Render("Source:"), valStyle.Render(p.SourceID)))
	}

	// Join left and right columns
	col1 := col1Builder.String()
	col2 := col2Builder.String()
	detailsBlock := lipgloss.JoinHorizontal(lipgloss.Top, col1, "      ", col2)

	// 4. Breach Details
	var breachBlock string
	if p.LimitBreached && p.BreachReason != "" {
		breachBlock = fmt.Sprintf("\n%s %s\n", warnValStyle.Render("Reason:"), breachReasonStyle.Render(p.BreachReason))
	}

	// 4.1 Rationale / Proposal Text
	var proposalBlock string
	if p.ProposalText != "" {
		proposalBlock = fmt.Sprintf("\n%s\n%s\n", labelStyle.Render("Trading Proposal Rationale:"), valStyle.Render(p.ProposalText))
	}

	// 4.2 Slippage Slice Boundaries
	var boundariesBlock string
	if len(p.SliceBoundaries) > 0 {
		var bStrs []string
		for _, b := range p.SliceBoundaries {
			bStrs = append(bStrs, fmt.Sprintf("%d", b))
		}
		boundariesBlock = fmt.Sprintf("\n%s [ %s ]\n", labelStyle.Render("Slippage Impact Slice Boundaries:"), valStyle.Render(strings.Join(bStrs, " -> ")))
	}

	// 5. Iceberg Schedule Visualizer
	var icebergBlock strings.Builder
	numChunks := len(p.IcebergOrders)
	if numChunks > 0 {
		icebergBlock.WriteString(fmt.Sprintf("\n%s\n", labelStyle.Render(fmt.Sprintf("Iceberg Schedule (%d Chunks):", numChunks))))

		// Find max delay to display total execution time
		maxDelay := 0
		for _, o := range p.IcebergOrders {
			if o.DelayMS > maxDelay {
				maxDelay = o.DelayMS
			}
		}

		// Display details of the chunks (limit to first 3 and last 3 if too long)
		const maxToShow = 6
		if numChunks <= maxToShow {
			for i, o := range p.IcebergOrders {
				icebergBlock.WriteString(renderChunkRow(i+1, o))
			}
		} else {
			// Show first 3
			for i := 0; i < 3; i++ {
				icebergBlock.WriteString(renderChunkRow(i+1, p.IcebergOrders[i]))
			}
			// Ellipsis
			icebergBlock.WriteString(fmt.Sprintf("  %s\n", labelStyle.Render(fmt.Sprintf("... [ %d intermediate chunks hidden ] ...", numChunks-6))))
			// Show last 3
			for i := numChunks - 3; i < numChunks; i++ {
				icebergBlock.WriteString(renderChunkRow(i+1, p.IcebergOrders[i]))
			}
		}

		// Simple horizontal execution timeline bar
		timelineWidth := 40
		var timeline strings.Builder
		timeline.WriteString("  [")
		for j := 0; j < timelineWidth; j++ {
			// Find if any chunk falls in this bin
			binStart := float64(j) / float64(timelineWidth) * float64(maxDelay)
			binEnd := float64(j+1) / float64(timelineWidth) * float64(maxDelay)

			hasChunk := false
			for _, o := range p.IcebergOrders {
				if float64(o.DelayMS) >= binStart && float64(o.DelayMS) < binEnd {
					hasChunk = true
					break
				}
			}
			if hasChunk {
				timeline.WriteString("⬢")
			} else {
				timeline.WriteString("┄")
			}
		}
		timeline.WriteString(fmt.Sprintf("] Total Duration: %d ms", maxDelay))
		icebergBlock.WriteString("\n" + timeStyle.Render(timeline.String()) + "\n")
	}

	// 6. Combine all components into the final card
	var cardContent strings.Builder
	cardContent.WriteString(detailsBlock)
	if breachBlock != "" {
		cardContent.WriteString(breachBlock)
	}
	if proposalBlock != "" {
		cardContent.WriteString(proposalBlock)
	}
	if boundariesBlock != "" {
		cardContent.WriteString(boundariesBlock)
	}
	if icebergBlock.Len() > 0 {
		cardContent.WriteString(icebergBlock.String())
	}

	// Render the container style
	var finalRender string
	if p.LimitBreached {
		finalRender = breachedStyle.Render(cardContent.String())
	} else {
		finalRender = okStyle.Render(cardContent.String())
	}

	fmt.Println(finalRender)
}

func renderChunkRow(idx int, o IcebergOrder) string {
	return fmt.Sprintf("  %s %s %s  %s  %s\n",
		labelStyle.Render(fmt.Sprintf("#%02d:", idx)),
		valStyle.Render(fmt.Sprintf("Size: %-4d", o.Size)),
		labelStyle.Render("|"),
		valStyle.Render(fmt.Sprintf("Price: $%-5.2f", o.Price)),
		timeStyle.Render(fmt.Sprintf("Delay: +%-4dms", o.DelayMS)),
	)
}

func handlePositions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte("[]"))
		return
	}

	val, err := redisClient.Get(r.Context(), "gekko:positions").Result()
	if err != nil {
		if err == redis.Nil {
			w.Write([]byte("[]"))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(val))
}

func handleLedger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte(`{"verified":true,"block_count":0,"blocks":[]}`))
		return
	}

	val, err := redisClient.Get(r.Context(), "gekko:ledger_summary").Result()
	if err != nil {
		if err == redis.Nil {
			w.Write([]byte(`{"verified":true,"block_count":0,"blocks":[]}`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(val))
}

func handlePendingProposal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Redis not connected"}`))
		return
	}

	if r.Method == http.MethodPost {
		// Read the posted proposal body
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		// Save the posted proposal to Redis so that subsequent GETs see it
		err = redisClient.Set(r.Context(), "gekko:pending_proposal", string(bodyBytes), 0).Err()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"message":"Pending proposal saved to Redis"}`))
		return
	}

	val, err := redisClient.Get(r.Context(), "gekko:pending_proposal").Result()
	if err != nil {
		if err == redis.Nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No pending proposals"}`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(val))
}

func handleApprove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		EventHash string `json:"event_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.EventHash == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if redisClient == nil {
		http.Error(w, "Redis not connected", http.StatusInternalServerError)
		return
	}

	err := redisClient.Publish(r.Context(), "gekko:commands", "approve:"+body.EventHash).Err()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true,"message":"Approval command sent to breen"}`))
}

func handleDeny(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		EventHash string `json:"event_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.EventHash == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if redisClient == nil {
		http.Error(w, "Redis not connected", http.StatusInternalServerError)
		return
	}

	err := redisClient.Publish(r.Context(), "gekko:commands", "deny:"+body.EventHash).Err()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true,"message":"Denial command sent to breen"}`))
}

func handleControls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte(`{"auto_approve":false}`))
		return
	}

	val, err := redisClient.Get(r.Context(), "gekko:auto_approve").Result()
	if err != nil {
		if err == redis.Nil {
			w.Write([]byte(`{"auto_approve":false}`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	isAuto := val == "true"
	json.NewEncoder(w).Encode(map[string]interface{}{
		"auto_approve": isAuto,
	})
}

// modelControlsDefault is returned when no controls have been set yet. Must match
// gordon's seed defaults (see gordon/controls.go).
const modelControlsDefault = `{"stance":"neutral","risk_tier":"balanced","match_threshold":0.5,"max_slippage_pct":0.03,"updated_at_ms":0}`

// handleModelControls serves GET (current knobs) and POST (update knobs). The
// blob is written to gekko:model_controls, which gordon polls live (no restart).
func handleModelControls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte(modelControlsDefault))
		return
	}

	if r.Method == http.MethodGet {
		val, err := redisClient.Get(r.Context(), "gekko:model_controls").Result()
		if err != nil || val == "" {
			w.Write([]byte(modelControlsDefault))
			return
		}
		w.Write([]byte(val))
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in struct {
		Stance         string  `json:"stance"`
		RiskTier       string  `json:"risk_tier"`
		MatchThreshold float64 `json:"match_threshold"`
		MaxSlippagePct float64 `json:"max_slippage_pct"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024)).Decode(&in); err != nil {
		http.Error(w, "Bad request JSON", http.StatusBadRequest)
		return
	}

	stanceOK := map[string]bool{"bearish": true, "neutral": true, "bullish": true}
	tierOK := map[string]bool{"conservative": true, "balanced": true, "aggressive": true}
	if !stanceOK[in.Stance] || !tierOK[in.RiskTier] {
		http.Error(w, "Invalid stance or risk_tier", http.StatusBadRequest)
		return
	}
	if in.MatchThreshold < 0.35 || in.MatchThreshold > 0.80 {
		http.Error(w, "match_threshold out of range [0.35, 0.80]", http.StatusBadRequest)
		return
	}
	// Optional (old clients omit it → 0): default rather than reject, so a blob
	// written by an older UI doesn't lose the field.
	if in.MaxSlippagePct == 0 {
		in.MaxSlippagePct = 0.03
	}
	if in.MaxSlippagePct < 0.001 || in.MaxSlippagePct > 0.10 {
		http.Error(w, "max_slippage_pct out of range [0.001, 0.10]", http.StatusBadRequest)
		return
	}

	out := map[string]interface{}{
		"stance":           in.Stance,
		"risk_tier":        in.RiskTier,
		"match_threshold":  in.MatchThreshold,
		"max_slippage_pct": in.MaxSlippagePct,
		"updated_at_ms":    time.Now().UnixMilli(),
	}
	blob, _ := json.Marshal(out)
	if err := redisClient.Set(r.Context(), "gekko:model_controls", string(blob), 0).Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(blob)
}

// gordonPriorityDefault is returned when no priority knobs have been set yet.
// fleet_rate 0 tells each gordon to auto-estimate the sustainable fleet rate; an
// empty weights map means every gordon runs at weight 1 (normal).
const gordonPriorityDefault = `{"fleet_rate":0,"weights":{},"updated_at_ms":0}`

// handleGordonPriority serves GET (current promote/demote knobs) and POST (update
// them). The blob is written to gekko:gordon_priority, which every gordon polls
// live (no restart) to pace its proposal generation. Weights: 1 = normal, <1 =
// demoted (slower), >1 = promoted (faster), 0 = paused.
func handleGordonPriority(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte(gordonPriorityDefault))
		return
	}

	if r.Method == http.MethodGet {
		// Merge the stored knobs with the LIVE fleet roster (gordon heartbeats) so the
		// pane lists exactly the running gordons, not a hardcoded set.
		out := map[string]interface{}{"fleet_rate": 0.0, "weights": map[string]float64{}, "updated_at_ms": int64(0)}
		if val, err := redisClient.Get(r.Context(), "gekko:gordon_priority").Result(); err == nil && val != "" {
			_ = json.Unmarshal([]byte(val), &out)
		}
		out["roster"] = gordonRoster(r.Context())
		blob, _ := json.Marshal(out)
		w.Write(blob)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in struct {
		FleetRate float64            `json:"fleet_rate"`
		Weights   map[string]float64 `json:"weights"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&in); err != nil {
		http.Error(w, "Bad request JSON", http.StatusBadRequest)
		return
	}
	if in.FleetRate < 0 || in.FleetRate > 100 {
		http.Error(w, "fleet_rate out of range [0, 100]", http.StatusBadRequest)
		return
	}
	// Clamp each weight to [0, 4]: 0 pauses a gordon, 4 is a hard promote ceiling so
	// a typo can't starve the rest of the fleet.
	weights := map[string]float64{}
	for id, v := range in.Weights {
		if id == "" || len(id) > 64 {
			http.Error(w, "invalid persona id", http.StatusBadRequest)
			return
		}
		if v < 0 {
			v = 0
		}
		if v > 4 {
			v = 4
		}
		weights[id] = v
	}

	out := map[string]interface{}{
		"fleet_rate":    in.FleetRate,
		"weights":       weights,
		"updated_at_ms": time.Now().UnixMilli(),
	}
	blob, _ := json.Marshal(out)
	if err := redisClient.Set(r.Context(), "gekko:gordon_priority", string(blob), 0).Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(blob)
}

// gordonRoster reads the live fleet-presence heartbeats each gordon writes
// (gekko:gordon_fleet:<id> = label, TTL-refreshed) into an {id,label} list. Absent
// heartbeats (old gordon binaries) ⇒ empty roster, and the pane shows a hint.
func gordonRoster(ctx context.Context) []map[string]string {
	roster := []map[string]string{}
	if redisClient == nil {
		return roster
	}
	keys, err := redisClient.Keys(ctx, "gekko:gordon_fleet:*").Result()
	if err != nil {
		return roster
	}
	for _, k := range keys {
		id := strings.TrimPrefix(k, "gekko:gordon_fleet:")
		label, _ := redisClient.Get(ctx, k).Result()
		if label == "" {
			label = id
		}
		roster = append(roster, map[string]string{"id": id, "label": label})
	}
	return roster
}

// handlePersonas serves the per-persona scorecard the executive publishes to
// gekko:persona_stats (Phase 2 tracking).
func handlePersonas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if redisClient == nil {
		w.Write([]byte(`[]`))
		return
	}
	val, err := redisClient.Get(r.Context(), "gekko:persona_stats").Result()
	if err != nil || val == "" {
		w.Write([]byte(`[]`))
		return
	}
	w.Write([]byte(val))
}

// handleBookMetrics serves the whole-account ("breen") risk-adjusted scorecard the
// executive publishes to gekko:book_metrics.
func handleBookMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if redisClient == nil {
		w.Write([]byte(`{}`))
		return
	}
	val, err := redisClient.Get(r.Context(), "gekko:book_metrics").Result()
	if err != nil || val == "" {
		w.Write([]byte(`{}`))
		return
	}
	w.Write([]byte(val))
}

// handleCalibration serves the per-persona confidence-calibration table the executive
// publishes to gekko:calibration — stated conviction binned against the win rate each
// bucket actually delivered. Display-only: nothing trades off it (breen-intelligence
// Phase 0), it exists so the fleet's miscalibration can be seen before it is corrected.
func handleCalibration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if redisClient == nil {
		w.Write([]byte(`{}`))
		return
	}
	val, err := redisClient.Get(r.Context(), "gekko:calibration").Result()
	if err != nil || val == "" {
		w.Write([]byte(`{}`))
		return
	}
	w.Write([]byte(val))
}

// handleRunConfig serves breen's run-configuration snapshot (the executive's knobs:
// starting capital, exposure caps, stop-loss floors, drawdown halt, entry gates,
// consensus/adaptive settings…), which breen publishes to gekko:run_config at startup.
// Read-only — these are set from run.sh env at launch, so changing them needs a restart.
func handleRunConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if redisClient == nil {
		w.Write([]byte(`{}`))
		return
	}
	val, err := redisClient.Get(r.Context(), "gekko:run_config").Result()
	if err != nil || val == "" {
		w.Write([]byte(`{}`))
		return
	}
	w.Write([]byte(val))
}

func handleToggleAuto(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if redisClient == nil {
		http.Error(w, "Redis not connected", http.StatusInternalServerError)
		return
	}

	err := redisClient.Publish(r.Context(), "gekko:commands", "toggle_auto").Err()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true,"message":"Auto-Approve toggle command sent to breen"}`))
}
