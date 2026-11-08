package main

import (
	"context"
	"log"
	"strings"
)

// Ledger status strings (mirror the AppendTransaction call sites).
const (
	statusDryFilled   = "DRY_FILLED"
	statusLiveFilled  = "LIVE_FILLED"
	statusDryClosed   = "DRY_CLOSED"
	statusLiveClosed  = "LIVE_CLOSED"
	statusDryResolved = "DRY_RESOLVED" // expiry/real-Kalshi settlement (account.go settleExpired)
	// statusRunReset marks a run-reset boundary. resetRun truncates the ledger AND
	// appends this marker, so even if truncation fails (the −$283k incident: replay
	// walked blocks from prior runs) replay only considers blocks after the last reset.
	statusRunReset = "RUN_RESET"
)

// replayedState is the account snapshot reconstructed purely from the ledger.
type replayedState struct {
	cash         float64
	realizedPnL  float64
	positions    map[string]*Position
	closedTrades []ClosedTrade
	applied      int // number of position-affecting blocks replayed
}

// replayLedger rebuilds the paper account from the hash-chained ledger by walking
// blocks in order: *_FILLED open/add a position (at the fill price, debiting cash),
// *_CLOSED/*_RESOLVED reduce/close a position (crediting proceeds + realized yield).
// Receipts (*_OPEN) and DENIED/REJECTED have no position effect. Only blocks after
// the last RUN_RESET marker are replayed — earlier blocks belong to a previous run.
// Pure — unit-tested.
//
// Limitation: the ledger does not record persona or expiry, so replayed positions
// are unattributed and get a fresh expiry assigned by the caller.
func replayLedger(blocks []TransactionBlock, startingCapital float64) replayedState {
	st := replayedState{
		cash:      startingCapital,
		positions: make(map[string]*Position),
	}
	// Replay from the last run-reset boundary only.
	for i := len(blocks) - 1; i >= 0; i-- {
		if strings.EqualFold(blocks[i].Status, statusRunReset) {
			blocks = blocks[i+1:]
			break
		}
	}
	for _, b := range blocks {
		switch strings.ToUpper(b.Status) {
		case statusDryFilled, statusLiveFilled:
			cost := b.Price * float64(b.Size)
			if pos, ok := st.positions[b.ContractID]; ok {
				newSize := pos.Size + b.Size
				if newSize > 0 {
					pos.EntryPrice = (pos.EntryPrice*float64(pos.Size) + b.Price*float64(b.Size)) / float64(newSize)
				}
				pos.Size = newSize
			} else {
				st.positions[b.ContractID] = &Position{
					ContractID: b.ContractID,
					EntryPrice: b.Price,
					Size:       b.Size,
					Timestamp:  b.Timestamp,
				}
			}
			st.cash -= cost
			st.applied++

		case statusDryClosed, statusLiveClosed, statusDryResolved:
			pos, ok := st.positions[b.ContractID]
			if !ok {
				continue // close with no live position (already flat) — ignore
			}
			sellSize := b.Size
			if sellSize > pos.Size {
				sellSize = pos.Size
			}
			st.cash += b.Price * float64(sellSize)
			st.realizedPnL += b.YieldDelta
			ret := 0.0
			if pos.EntryPrice > 0 {
				ret = (b.Price - pos.EntryPrice) / pos.EntryPrice * 100.0
			}
			st.closedTrades = append(st.closedTrades, ClosedTrade{
				ContractID:  pos.ContractID,
				Exchange:    inferExchange(pos.ContractID),
				Side:        "CLOSE",
				EntryPrice:  pos.EntryPrice,
				ExitPrice:   b.Price,
				Size:        sellSize,
				RealizedPnL: b.YieldDelta,
				ReturnPct:   ret,
				OpenedAtMs:  pos.Timestamp.UnixMilli(),
				ClosedAtMs:  b.Timestamp.UnixMilli(),
				Status:      b.Status,
				Persona:     pos.Persona,
			})
			pos.Size -= sellSize
			if pos.Size <= 0 {
				delete(st.positions, pos.ContractID)
			}
			st.applied++
		}
	}
	return st
}

// recoverState restores the paper account on startup. Redis is the fast path; if
// Redis has been wiped (e.g. ran with persistence off and the stack restarted) but
// the durable ledger has history, we rebuild from the ledger and re-seed Redis.
func (pt *PositionTracker) recoverState(ctx context.Context) {
	pt.reloadFromRedis(ctx)

	pt.mu.RLock()
	redisHadState := len(pt.positions) > 0 || len(pt.closedTrades) > 0 || pt.cash != pt.startingCapital
	pt.mu.RUnlock()
	if redisHadState {
		return // Redis restore succeeded — nothing to rebuild
	}

	if pt.ledger == nil {
		return
	}
	blocks, err := pt.ledger.GetBlocks()
	if err != nil || len(blocks) == 0 {
		return
	}
	st := replayLedger(blocks, pt.startingCapital)
	if st.applied == 0 {
		return // ledger has only receipts/denials — nothing to restore
	}
	// Sanity: a replay can only produce negative cash if the ledger is inconsistent
	// (e.g. spans runs whose settlement credits are missing). Adopting such a state
	// once had the bot trading from cash=−$283k; start fresh and shout instead.
	if st.cash < 0 {
		log.Printf("[ERROR] Ledger replay produced implausible state (cash=$%.2f, %d open positions from %d blocks) — refusing to adopt it; starting fresh at $%.2f. Inspect/reset the ledger file.",
			st.cash, len(st.positions), len(blocks), pt.startingCapital)
		return
	}

	pt.mu.Lock()
	pt.cash = st.cash
	pt.realizedPnL = st.realizedPnL
	pt.closedTrades = st.closedTrades
	for id, pos := range st.positions {
		// Ledger lacks expiry; recover the contract's real resolution time from its
		// ticker, falling back to a random window for undateable ids (POLY/synthetic).
		pos.Expiry = pt.resolveExpiry(id)
		pt.positions[id] = pos
	}
	openN := len(pt.positions)
	cash := pt.cash
	realized := pt.realizedPnL
	pt.mu.Unlock()

	log.Printf("[INFO] Redis empty — rebuilt paper account from ledger (%d blocks applied): cash=$%.2f, realized=$%.2f, %d open positions, %d closed trades.",
		st.applied, cash, realized, openN, len(st.closedTrades))

	// Re-seed Redis so the rest of the system sees the recovered state immediately.
	pt.syncToRedis(ctx)
	pt.syncAccountToRedis(ctx)
}
