package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// GekkoIngestEvent matches kahuna's inbound payload model.
type GekkoIngestEvent struct {
	SourceID             string `json:"SourceID"`
	RawContent           string `json:"RawContent"`
	SourceURL            string `json:"SourceURL"`
	ExtractedTimestampNS int64  `json:"ExtractedTimestampNS"`
	PayloadHash          string `json:"PayloadHash"`
}

func main() {
	log.Println("[INFO] Initializing Gekko Breen...")

	// 1. Load Configurations
	cfg := LoadConfig()
	modeName := "LIVE ROUTING"
	if cfg.DryMode {
		modeName = "DETEIMINISTIC DRY SIMULATION"
	}
	log.Printf("[INFO] Breen Mode: %s", modeName)
	log.Printf("[INFO] Database Location: %s", cfg.LedgerFilePath)

	// 2. Initialize Ledger and Broker Unix client
	ledger := NewImmutableLedger(cfg.LedgerFilePath)
	valid, err := ledger.VerifyLedger()
	if err != nil {
		log.Fatalf("[FATAL] Ledger hash verification failed: %v. Tampering detected!", err)
	}
	if !valid {
		log.Fatalf("[FATAL] Cryptographic ledger chain is corrupted!")
	}
	log.Println("[INFO] Cryptographic Ledger chain verified and intact.")

	brokerClient := NewBrokerClient(cfg.BrokerSocketPath, cfg.BrokerToken)
	tracker := NewPositionTracker(cfg.RedisAddr, ledger, brokerClient, cfg.DryMode, cfg.StartingCapital, cfg.ExpiryMin, cfg.ExpiryMax)
	// Live NAV is marked against real order books only. When a book is missing the
	// mark falls back to the position's entry price (0 unrealized) rather than a
	// fabricated book. ALLOW_MOCK_ORDERBOOK re-enables synthetic books for keyless
	// dev/tests only.
	tracker.allowMockBook = getEnvBool("ALLOW_MOCK_ORDERBOOK", false)
	if tracker.allowMockBook {
		log.Println("[WARN] ALLOW_MOCK_ORDERBOOK=true — open positions may be marked against SYNTHETIC order books (dev/test only).")
	}
	tracker.SetRiskCaps(RiskCaps{
		MaxContractExposure:    cfg.MaxContractExposure,
		MaxContractExposurePct: cfg.MaxContractExposurePct,
		MaxCategoryExposure:    cfg.MaxCategoryExposure,
		MaxCategoryExposurePct: cfg.MaxCategoryExposurePct,
		MaxTotalDeployed:       cfg.MaxTotalDeployed,
		MaxOpenPositions:       cfg.MaxOpenPositions,
		RejectOnBreach:         cfg.RejectOnBreach,
		PerContractLossLimit:   cfg.PerContractLossLimit,
		ExitCooldown:           cfg.ExitCooldown,
		MaxDrawdownHaltPct:     cfg.MaxDrawdownHaltPct,
		SizingEnabled:          cfg.SizingEnabled,
		DrawdownEaseStart:      cfg.DrawdownEaseStart,
		DrawdownEaseFloor:      cfg.DrawdownEaseFloor,
		CategoryLossLimit:      cfg.CategoryLossLimit,
		OneStrikePerFamily:     cfg.OneStrikePerFamily,
		MidBandLow:             cfg.MidBandLow,
		MidBandHigh:            cfg.MidBandHigh,
		MidBandMinConfidence:   cfg.MidBandMinConfidence,
		MinClipNotional:        cfg.MinClipNotional,
		MaxEntriesPerDay:       cfg.MaxEntriesPerDay,
		MaxFamilyEntriesPerDay: cfg.MaxFamilyEntriesPerDay,
	})
	tracker.forceExitWindow = cfg.ForceExitBeforeExpiry
	tracker.stopLossPct = cfg.StopLossPct
	tracker.stopLossUSD = cfg.StopLossUSD
	tracker.stopLossMinAbs = cfg.StopLossMinAbs
	tracker.takeProfitPct = cfg.TakeProfitPct
	tracker.takeProfitUSD = cfg.TakeProfitUSD
	tracker.trailArmPct = cfg.TrailArmPct
	tracker.trailRetracePct = cfg.TrailRetracePct
	tracker.settlementHoldPersonas = parsePersonaSet(cfg.SettlementHoldPersonas)
	tracker.settlementHoldTPPrice = cfg.SettlementHoldTPPrice
	tracker.shadowPersonas = parsePersonaSet(cfg.ShadowPersonas)
	tracker.shadowBarProposals = cfg.ShadowBarProposals
	tracker.shadowBarMinContracts = cfg.ShadowBarMinContracts
	tracker.familyBlacklist = parsePersonaSet(cfg.FamilyBlacklist)
	tracker.perContractLossCloses = cfg.PerContractLossCloses
	tracker.markHaircut = cfg.MarkFallbackHaircut
	tracker.markStaleMaxAge = cfg.MarkStaleMaxAge
	tracker.settlePrior = cfg.SettlePrior
	tracker.metricsMinSamples = cfg.MetricsMinSamples
	tracker.entryRequireLiveBook = cfg.EntryRequireLiveBook
	tracker.entryMaxSpreadPct = cfg.EntryMaxSpreadPct
	tracker.entryMaxSpreadAbs = cfg.EntryMaxSpreadAbs
	tracker.bookFreshMaxAge = cfg.BookFreshMaxAge
	tracker.entryEdgeFrictionMult = cfg.EntryEdgeFrictionMult
	tracker.feeModel = cfg.KalshiFeeModel
	tracker.feeCharge = cfg.KalshiFeeCharge
	tracker.feeRate = cfg.KalshiFeeRate
	tracker.calibrationBins = cfg.CalibrationBins
	tracker.calibrationEnabled = cfg.CalibrationEnabled
	tracker.calibrationMinSamples = cfg.CalibrationMinSamples
	tracker.calibrationFactorMin = cfg.CalibrationFactorMin
	tracker.calibrationFactorMax = cfg.CalibrationFactorMax
	log.Printf("[INFO] Portfolio risk gate: per-contract ≤ $%.0f (≤%.1f%% NAV) | per-category ≤ %.1f%% NAV | total deployed ≤ $%.0f | max open %d | reject-on-breach=%t",
		cfg.MaxContractExposure, cfg.MaxContractExposurePct*100, cfg.MaxCategoryExposurePct*100, cfg.MaxTotalDeployed, cfg.MaxOpenPositions, cfg.RejectOnBreach)
	log.Printf("[INFO] Post-mortem guards: per-contract loss limit -$%.0f (closes=%t) | exit cooldown %v | drawdown halt %.0f%% | force-exit %v before resolution | stop-loss -%.0f%% / -$%.0f (move ≥ $%.2f)",
		cfg.PerContractLossLimit, cfg.PerContractLossCloses, cfg.ExitCooldown, cfg.MaxDrawdownHaltPct, cfg.ForceExitBeforeExpiry, cfg.StopLossPct, cfg.StopLossUSD, cfg.StopLossMinAbs)
	log.Printf("[INFO] Entry gates: category loss limit -$%.0f | mid-band [%.2f, %.2f] needs confidence ≥ %.2f | one strike per family=%t",
		cfg.CategoryLossLimit, cfg.MidBandLow, cfg.MidBandHigh, cfg.MidBandMinConfidence, cfg.OneStrikePerFamily)
	log.Printf("[INFO] Friction/churn gates: min clip $%.0f notional | max %d entries/day | max %d round trip(s) per family per day | edge ≥ %.1f× round-trip friction (0 = off)",
		cfg.MinClipNotional, cfg.MaxEntriesPerDay, cfg.MaxFamilyEntriesPerDay, cfg.EntryEdgeFrictionMult)
	log.Printf("[INFO] Entry book gate: require live book=%t | spread ≤ %.0f%% of mid (or ≤ $%.2f abs) | book age ≤ %v",
		cfg.EntryRequireLiveBook, cfg.EntryMaxSpreadPct*100, cfg.EntryMaxSpreadAbs, cfg.BookFreshMaxAge)
	log.Printf("[INFO] Cost accounting: fee model=%t (rate %.3f, charged to cash=%t) | excursions (MAE/MFE) recorded each tick | calibration table %d bins",
		cfg.KalshiFeeModel, cfg.KalshiFeeRate, cfg.KalshiFeeCharge, cfg.CalibrationBins)
	if len(tracker.familyBlacklist) > 0 {
		log.Printf("[INFO] Family blacklist: %s — new entries on these families are refused outright.", cfg.FamilyBlacklist)
	}
	if len(tracker.shadowPersonas) > 0 {
		log.Printf("[INFO] Shadow mode: %s — these personas keep proposing but NEVER fill; proposals are recorded to %s for later scoring.",
			cfg.ShadowPersonas, shadowProposalsKey)
	}
	if len(tracker.settlementHoldPersonas) > 0 {
		log.Printf("[INFO] Settlement hold: %s — %% stop / %% take-profit / trailing stop / force-exit all SKIPPED for these personas; dollar backstops (-$%.0f / +$%.0f) and family loss limits still apply.",
			cfg.SettlementHoldPersonas, cfg.StopLossUSD, cfg.TakeProfitUSD)
		if cfg.SettlementHoldTPPrice > 0 {
			log.Printf("[INFO] Settlement take-profit: held positions are banked at a bid ≥ %.2f — at that price the market has already paid the edge out and only the tail is left.",
				cfg.SettlementHoldTPPrice)
		} else {
			log.Printf("[INFO] Settlement take-profit: disabled (SETTLEMENT_HOLD_TP_PRICE=0 — held positions ride all the way to resolution)")
		}
	} else {
		log.Printf("[INFO] Settlement hold: disabled (SETTLEMENT_HOLD_PERSONAS empty — every position is managed on the mark)")
	}
	if cfg.TrailArmPct > 0 && cfg.TrailRetracePct > 0 {
		log.Printf("[INFO] Trailing stop: arms at +%.0f%% of entry, fires on a %.0f%% give-back from the best mark (noise floor $%.2f)",
			cfg.TrailArmPct, cfg.TrailRetracePct, cfg.StopLossMinAbs)
	} else {
		log.Printf("[INFO] Trailing stop: disabled (set both TRAIL_ARM_PCT and TRAIL_RETRACE_PCT to arm it)")
	}
	log.Printf("[INFO] Calibrated confidence: enabled=%t | ≥%d trades/bucket to override the stated value | factor clamped [%.2f, %.2f]",
		cfg.CalibrationEnabled, cfg.CalibrationMinSamples, cfg.CalibrationFactorMin, cfg.CalibrationFactorMax)

	// Buy-and-hold benchmark (default SPY): "how would the seed money have done if
	// just invested in the index?" Polls a keyless live quote and marks a position
	// opened with the starting capital at run start. Off with BENCHMARK_ENABLED=false.
	if getEnvBool("BENCHMARK_ENABLED", true) {
		benchTicker := getEnv("BENCHMARK_TICKER", "SPY")
		tracker.benchmark = NewBenchmarkTracker(tracker.redisClient, benchTicker,
			getEnv("BENCHMARK_QUOTE_URL", defaultBenchmarkQuoteURL),
			getEnv("BENCHMARK_HISTORY_URL", defaultBenchmarkHistoryURL))
		log.Printf("[INFO] Benchmark ENABLED: %s buy-and-hold from $%.0f seed.", benchTicker, cfg.StartingCapital)
	}

	// Adaptive allocation (Phase 4): bandit-style persona weighting that learns from
	// realized performance and persists across restarts. Off unless enabled.
	if cfg.AdaptiveEnabled {
		tracker.allocator = NewAdaptiveAllocator(tracker.redisClient, cfg.AdaptiveDecay, cfg.AdaptiveGain,
			cfg.AdaptiveExploreFloor, cfg.AdaptiveMinWeight, 1.5, cfg.AdaptiveMinTrades)
		tracker.allocator.SetBenchPolicy(cfg.PersonaBenchScore, cfg.PersonaBenchMinTrades, cfg.PersonaBenchDuration)
		tracker.allocator.load(context.Background())
		log.Printf("[INFO] Adaptive allocation ENABLED: decay %.2f | gain %.2f | explore floor %.2f | min weight %.2f | min trades %d | bench score < %.2f after %d trades for %v.",
			cfg.AdaptiveDecay, cfg.AdaptiveGain, cfg.AdaptiveExploreFloor, cfg.AdaptiveMinWeight, cfg.AdaptiveMinTrades,
			cfg.PersonaBenchScore, cfg.PersonaBenchMinTrades, cfg.PersonaBenchDuration)
	}

	// Consensus engine (Phase 3): when enabled, competing persona proposals are
	// buffered and consolidated; otherwise each proposal flows straight through.
	var consensus *ConsensusEngine
	if cfg.ConsensusEnabled {
		consensus = NewConsensusEngine(tracker, cfg.ConsensusWindow, cfg.ConsensusMinConviction, cfg.ConsensusBearVeto, cfg.ConsensusBearCacheTTL)
		log.Printf("[INFO] Consensus engine ENABLED: window %v | min conviction %.2f | bear-veto %v (bear-cache %v) (%s weights).",
			cfg.ConsensusWindow, cfg.ConsensusMinConviction, cfg.ConsensusBearVeto, cfg.ConsensusBearCacheTTL,
			map[bool]string{true: "adaptive", false: "static performance"}[cfg.AdaptiveEnabled])
	}
	tracker.exporter = NewRunExporter(cfg.RunExportDir)
	if cfg.RunExportDir != "" {
		log.Printf("[INFO] Run-log export → %s (trades.csv, equity.csv, *.jsonl, run_summary.txt)", cfg.RunExportDir)
	}

	// Paper run: enable auto-approve from config (required for an unattended run).
	if cfg.AutoApprove {
		tracker.mu.Lock()
		tracker.autoApprove = true
		tracker.mu.Unlock()
		log.Println("[INFO] Auto-approve ENABLED at startup (unattended paper run).")
	}
	log.Printf("[INFO] Paper account: starting capital $%.2f | expiry %v–%v | snapshot %v",
		cfg.StartingCapital, cfg.ExpiryMin, cfg.ExpiryMax, cfg.SnapshotInterval)

	// Expose /healthz for the status dashboard (executive has no other HTTP surface).
	healthAddr := ":8091"
	if v, ok := os.LookupEnv("HEALTH_ADDR"); ok {
		healthAddr = v
	}
	startHealthServer(healthAddr, tracker, ledger)

	// Start Redis commands channel listener
	if tracker.redisClient != nil {
		// Restore an in-flight run: Redis fast-path, with ledger replay as the
		// durable fallback if Redis was wiped.
		tracker.recoverState(context.Background())

		go func() {
			pubsub := tracker.redisClient.Subscribe(context.Background(), "gekko:commands")
			defer pubsub.Close()
			for {
				msg, err := pubsub.ReceiveMessage(context.Background())
				if err != nil {
					time.Sleep(1 * time.Second)
					continue
				}
				tracker.webCommandChannel <- msg.Payload
			}
		}()
		// Sync initial states on startup
		tracker.syncToRedis(context.Background())
		// Expose the run configuration so liveview can include it in export bundles.
		if data, err := json.Marshal(cfg.Snapshot()); err == nil {
			_ = tracker.redisClient.Set(context.Background(), "gekko:run_config", string(data), 0).Err()
		}
	}

	// Background loops for the paper run: settlement at expiry + equity recording.
	settleCtx, settleCancel := context.WithCancel(context.Background())
	defer settleCancel()
	go tracker.runSettlementLoop(settleCtx)
	if tracker.benchmark != nil {
		// Anchor + backfill synchronously so the first equity sample (recorded
		// immediately by the recorder below) already carries benchmark data.
		primeCtx, primeCancel := context.WithTimeout(context.Background(), 15*time.Second)
		tracker.benchmark.Prime(primeCtx, cfg.StartingCapital)
		primeCancel()
		go tracker.benchmark.run(settleCtx, getEnvDuration("BENCHMARK_REFRESH", 5*time.Minute), cfg.StartingCapital)
	}
	go tracker.runEquityRecorder(settleCtx, cfg.SnapshotInterval)
	// Scores the shadow book against realized outcomes, so the re-activation bar in
	// shadow.go is a number someone can read rather than an unmeasured promise. No-op
	// when nothing is shadowed.
	go tracker.runShadowScorer(settleCtx, cfg.ShadowScoreInterval)

	// 3. Connect to Kafka
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var kClient *kgo.Client
	if len(cfg.KafkaBrokers) > 0 {
		opts := []kgo.Opt{
			kgo.SeedBrokers(cfg.KafkaBrokers...),
			kgo.ConsumerGroup("breen-group"),
			kgo.ConsumeTopics(cfg.KafkaProposalsTopic, cfg.KafkaInboundTopic),
		}
		cl, err := kgo.NewClient(opts...)
		if err == nil {
			pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
			if err := cl.Ping(pingCtx); err == nil {
				kClient = cl
				log.Println("[INFO] Successfully connected to Kafka brokers.")
			} else {
				log.Printf("[WARNING] Kafka brokers unreachable: %v. Running in sandbox console-only mode.", err)
				cl.Close()
			}
			pingCancel()
		} else {
			log.Printf("[WARNING] Failed to create Kafka client: %v. Running in console-only mode.", err)
		}
	}

	// 4. Start Kafka Ingestion Goroutine if connected
	if kClient != nil {
		go func() {
			defer kClient.Close()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					fetches := kClient.PollRecords(ctx, 1)
					if fetches.IsClientClosed() {
						return
					}
					iter := fetches.RecordIter()
					for !iter.Done() {
						record := iter.Next()
						if record.Topic == cfg.KafkaProposalsTopic {
							var proposal StrategyProposal
							if err := json.Unmarshal(record.Value, &proposal); err == nil && proposal.ContractID != "" {
								// Warm the order book for prospective entries: BUY tickers go on
								// marketdata's hot watch set now, so by the time consensus flushes
								// (and the entry book gate runs) the book is seconds old.
								if !proposal.Bearish && strings.ToUpper(proposal.Action) == "BUY" {
									tracker.markHotTicker(proposal.ContractID)
								}
								// Shadow mode (desk review fix #3) is checked BEFORE routing:
								// a suppressed persona must not reach consensus either, or
								// it would still shape a filling order — adding conviction
								// to someone else's BUY, or vetoing it with a bear signal.
								if tracker.captureShadowProposal(&proposal) {
									continue
								}
								if consensus != nil {
									consensus.Submit(&proposal)
								} else {
									tracker.ProcessStrategyProposal(&proposal)
								}
							}
						} else if record.Topic == cfg.KafkaInboundTopic {
							var ev GekkoIngestEvent
							if err := json.Unmarshal(record.Value, &ev); err == nil {
								tracker.TrackSettlement(ev.RawContent)
							}
						}
					}
				}
			}
		}()
	}

	// 5. Setup Stdin command scanner
	stdinChan := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			stdinChan <- strings.TrimSpace(scanner.Text())
		}
	}()

	// 6. CLI interactive approval/denial loop
	fmt.Println("\n=======================================================")
	fmt.Println("         GEKKO BREEN CONSOLE ACTIVE                    ")
	fmt.Println("=======================================================")
	fmt.Printf(" Mode      : %s\n", modeName)
	fmt.Printf(" Ledger    : %s\n", cfg.LedgerFilePath)
	fmt.Println(" Commands  : [sim]ulate proposal, [verify] ledger hash chain,")
	fmt.Println("             [status] report active positions, [help], [exit]")
	fmt.Println("=======================================================")

	var pendingProposal *StrategyProposal

	for {
		if pendingProposal == nil {
			fmt.Print("breen> ")
		}

		select {
		case proposal := <-tracker.proposalChannel:
			pendingProposal = proposal
			tracker.syncPendingProposal(ctx, proposal)
			renderProposalCard(proposal)
			fmt.Print("Approval Action [a/d/s]> ")

		case text := <-stdinChan:
			if pendingProposal != nil {
				// Process approval loop inputs
				input := strings.ToLower(text)
				if input == "a" || input == "approve" {
					fmt.Println("[INFO] Strategy proposal APPROVED.")
					tracker.ExecuteOrder(pendingProposal)
					tracker.clearPendingProposal(ctx)
					pendingProposal = nil
				} else if input == "d" || input == "deny" {
					fmt.Println("[INFO] Strategy proposal DENIED.")
					// Log DENIED to the ledger
					_, _ = ledger.AppendTransaction(pendingProposal.ContractID, pendingProposal.Action, pendingProposal.TotalSize, pendingProposal.AveragePrice, "DENIED", 0, "USER_DENIED")
					tracker.clearPendingProposal(ctx)
					pendingProposal = nil
				} else if input == "s" || input == "auto" {
					fmt.Println("[INFO] Auto-Approve mode ENABLED. Executing this and all future orders automatically.")
					tracker.mu.Lock()
					tracker.autoApprove = true
					tracker.mu.Unlock()
					tracker.ExecuteOrder(pendingProposal)
					tracker.clearPendingProposal(ctx)
					pendingProposal = nil
				} else {
					fmt.Println("[ERROR] Invalid choice. Enter 'a' to approve, 'd' to deny, or 's' for auto-approve.")
					fmt.Print("Approval Action [a/d/s]> ")
				}
			} else {
				// General console commands
				cmd := strings.ToLower(text)
				switch cmd {
				case "exit":
					log.Println("[INFO] Shutting down Breen...")
					return
				case "help":
					fmt.Println("Commands:")
					fmt.Println("  sim    - Generate a mock Strategy Proposal to test the approval loop")
					fmt.Println("  verify - Cryptographically audit all blocks in the immutable ledger")
					fmt.Println("  status - Report held positions and config flags")
					fmt.Println("  exit   - Stop the service")
				case "verify":
					valid, err := ledger.VerifyLedger()
					if err != nil {
						fmt.Printf("[AUDIT ERROR] Cryptographic chain audit failed: %v\n", err)
					} else if valid {
						blocks, _ := ledger.GetBlocks()
						fmt.Printf("[AUDIT OK] Ledger is cryptographically secure. Total verified blocks: %d\n", len(blocks))
					} else {
						fmt.Println("[AUDIT FAILED] Ledger chain check failed! Tampering detected.")
					}
				case "status":
					tracker.mu.RLock()
					fmt.Println("=== Breen Status Report ===")
					fmt.Printf(" Dry Mode Enabled    : %t\n", tracker.dryMode)
					fmt.Printf(" Auto-Approve Enabled: %t\n", tracker.autoApprove)
					fmt.Printf(" Active Positions Held: %d\n", len(tracker.positions))
					for _, pos := range tracker.positions {
						fmt.Printf("   - %s: Size=%d | AvgEntryPrice=%.4f | OpenFor=%v\n",
							pos.ContractID, pos.Size, pos.EntryPrice, time.Since(pos.Timestamp).Round(time.Second))
					}
					tracker.mu.RUnlock()
				case "sim":
					// Trigger simulated proposal
					contracts := []string{"AAPL-UP-200", "BTC-100K-DEC", "FED-RATE-2026"}
					actions := []string{"BUY", "SELL"}
					rng := rand.New(rand.NewSource(time.Now().UnixNano()))
					ticker := contracts[rng.Intn(len(contracts))]
					act := actions[rng.Intn(len(actions))]
					price := 0.40 + rng.Float64()*0.40
					size := 200 + rng.Intn(800)

					mockProp := &StrategyProposal{
						ContractID:    ticker,
						Action:        act,
						AveragePrice:  price,
						TotalSize:     size,
						SlippagePct:   0.005 + rng.Float64()*0.015,
						LimitBreached: false,
						Timestamp:     time.Now(),
						EventHash:     fmt.Sprintf("mock-%d", time.Now().UnixNano()),
					}
					tracker.ProcessStrategyProposal(mockProp)
				case "":
					// do nothing
				default:
					fmt.Printf("[ERROR] Unknown command: %s. Type 'help' for instructions.\n", cmd)
				}
			}

		case cmdText := <-tracker.webCommandChannel:
			if cmdText == "reset_run" || strings.HasPrefix(cmdText, "reset_run:") {
				// Optional note after the colon is archived alongside the run files.
				note := strings.TrimPrefix(strings.TrimPrefix(cmdText, "reset_run"), ":")
				tracker.resetRun(context.Background(), note)
				fmt.Printf("\n[WEB INFO] Paper run reset to $%.2f.\n", cfg.StartingCapital)
				if pendingProposal == nil {
					fmt.Print("breen> ")
				} else {
					fmt.Print("Approval Action [a/d/s]> ")
				}
				continue
			}
			if cmdText == "liquidate_all" {
				n := tracker.liquidateAllPositions(context.Background())
				fmt.Printf("\n[WEB INFO] Liquidated %d open position(s) at current marks.\n", n)
				if pendingProposal == nil {
					fmt.Print("breen> ")
				} else {
					fmt.Print("Approval Action [a/d/s]> ")
				}
				continue
			}
			if cmdText == "toggle_auto" {
				tracker.mu.Lock()
				tracker.autoApprove = !tracker.autoApprove
				newVal := tracker.autoApprove
				tracker.mu.Unlock()
				tracker.syncToRedis(context.Background())
				fmt.Printf("\n[WEB INFO] Web client toggled Auto-Approve to: %t\n", newVal)
				if pendingProposal == nil {
					fmt.Print("breen> ")
				} else {
					fmt.Print("Approval Action [a/d/s]> ")
				}
				continue
			}

			if pendingProposal != nil {
				parts := strings.SplitN(cmdText, ":", 2)
				if len(parts) == 2 {
					action := parts[0]
					hash := parts[1]
					if pendingProposal.EventHash == hash {
						if action == "approve" {
							fmt.Printf("\n[WEB INFO] Web client APPROVED proposal for %s.\n", pendingProposal.ContractID)
							tracker.ExecuteOrder(pendingProposal)
							tracker.clearPendingProposal(ctx)
							pendingProposal = nil
						} else if action == "deny" {
							fmt.Printf("\n[WEB INFO] Web client DENIED proposal for %s.\n", pendingProposal.ContractID)
							_, _ = ledger.AppendTransaction(pendingProposal.ContractID, pendingProposal.Action, pendingProposal.TotalSize, pendingProposal.AveragePrice, "DENIED", 0, "USER_DENIED")
							tracker.clearPendingProposal(ctx)
							pendingProposal = nil
						}
					} else {
						fmt.Printf("\n[WEB WARN] Received web command for mismatching event hash %s (expected %s)\n", hash, pendingProposal.EventHash)
					}
				}
			} else {
				fmt.Println("\n[WEB WARN] Received web command but no proposal is currently pending.")
			}
		}
	}
}

func renderProposalCard(p *StrategyProposal) {
	fmt.Println("\n=======================================================")
	fmt.Println("               NEW TRADING PROPOSAL RECEIVED           ")
	fmt.Println("=======================================================")
	fmt.Printf("  Contract ID  : %s\n", p.ContractID)
	fmt.Printf("  Action       : %s\n", p.Action)
	fmt.Printf("  Size         : %d contracts\n", p.TotalSize)
	fmt.Printf("  Target Price : $%.4f\n", p.AveragePrice)
	fmt.Printf("  Est Slippage : %.4f%%\n", p.SlippagePct*100)
	fmt.Printf("  Limit Breach : %t\n", p.LimitBreached)
	if p.BreachReason != "" {
		fmt.Printf("  Reason       : %s\n", p.BreachReason)
	}
	fmt.Println("-------------------------------------------------------")
	fmt.Println("  [a]pprove | [d]eny | [s]witch to auto-approve")
	fmt.Println("=======================================================")
}
