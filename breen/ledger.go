package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// TransactionBlock represents an immutable record in our ledger database.
type TransactionBlock struct {
	TxID       uint64    `json:"tx_id"`
	Timestamp  time.Time `json:"timestamp"`
	ContractID string    `json:"contract_id"`
	Action     string    `json:"action"` // BUY, SELL
	Size       int       `json:"size"`
	Price      float64   `json:"price"`
	Status     string    `json:"status"` // DRY_OPEN, DRY_FILLED, DRY_CLOSED, LIVE_OPEN, LIVE_FILLED, LIVE_CLOSED, DENIED
	YieldDelta float64   `json:"yield_delta,omitempty"`
	BrokerTxID string    `json:"broker_tx_id,omitempty"`
	PrevHash   string    `json:"prev_hash"`
	Hash       string    `json:"hash"`
}

// ImmutableLedger implements a thread-safe append-only ledger writing to a file.
type ImmutableLedger struct {
	filePath string
	mu       sync.RWMutex
}

func NewImmutableLedger(filePath string) *ImmutableLedger {
	return &ImmutableLedger{
		filePath: filePath,
	}
}

// ComputeHash calculates the SHA-256 cryptographic checksum of a block.
func ComputeHash(block TransactionBlock) string {
	data := fmt.Sprintf("%d|%s|%s|%s|%d|%.4f|%s|%.4f|%s|%s",
		block.TxID,
		block.Timestamp.Format(time.RFC3339Nano),
		block.ContractID,
		block.Action,
		block.Size,
		block.Price,
		block.Status,
		block.YieldDelta,
		block.BrokerTxID,
		block.PrevHash,
	)
	h := sha256.New()
	h.Write([]byte(data))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// AppendTransaction cryptographically links a new transaction to the previous block and writes it.
func (l *ImmutableLedger) AppendTransaction(contractID, action string, size int, price float64, status string, yieldDelta float64, brokerTxID string) (*TransactionBlock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 1. Read existing blocks to find the latest block
	blocks, err := l.readAllBlocks()
	if err != nil {
		return nil, fmt.Errorf("failed to read ledger blocks: %w", err)
	}

	var txID uint64 = 1
	prevHash := "0000000000000000000000000000000000000000000000000000000000000000" // genesis hash
	if len(blocks) > 0 {
		lastBlock := blocks[len(blocks)-1]
		txID = lastBlock.TxID + 1
		prevHash = lastBlock.Hash
	}

	block := TransactionBlock{
		TxID:       txID,
		Timestamp:  time.Now(),
		ContractID: contractID,
		Action:     action,
		Size:       size,
		Price:      price,
		Status:     status,
		YieldDelta: yieldDelta,
		BrokerTxID: brokerTxID,
		PrevHash:   prevHash,
	}
	block.Hash = ComputeHash(block)

	// 2. Append to ledger file
	file, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0660)
	if err != nil {
		return nil, fmt.Errorf("failed to open ledger file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	if err := encoder.Encode(block); err != nil {
		return nil, fmt.Errorf("failed to write block JSON: %w", err)
	}

	return &block, nil
}

// VerifyLedger verifies that the entire block chain is cryptographically intact.
func (l *ImmutableLedger) VerifyLedger() (bool, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	blocks, err := l.readAllBlocks()
	if err != nil {
		return false, err
	}

	expectedPrevHash := "0000000000000000000000000000000000000000000000000000000000000000"
	for idx, block := range blocks {
		// Verify backlink hash matches previous block hash
		if block.PrevHash != expectedPrevHash {
			return false, fmt.Errorf("block %d backlink verification failed: expected %s, got %s (tampering detected!)", 
				block.TxID, expectedPrevHash, block.PrevHash)
		}

		// Recalculate hash of current block parameters
		calculatedHash := ComputeHash(block)
		if block.Hash != calculatedHash {
			return false, fmt.Errorf("block %d hash verification failed: recorded %s, calculated %s (tampering detected!)", 
				block.TxID, block.Hash, calculatedHash)
		}

		expectedPrevHash = block.Hash
		_ = idx
	}

	return true, nil
}

// GetBlocks retrieves all stored ledger transactions.
func (l *ImmutableLedger) GetBlocks() ([]TransactionBlock, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.readAllBlocks()
}

// Reset truncates the ledger file, starting a fresh chain. Used when resetting a paper run.
func (l *ImmutableLedger) Reset() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return os.WriteFile(l.filePath, []byte{}, 0660)
}

// Helper to parse blocks from file
func (l *ImmutableLedger) readAllBlocks() ([]TransactionBlock, error) {
	file, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_RDONLY, 0660)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var blocks []TransactionBlock
	decoder := json.NewDecoder(file)
	for {
		var block TransactionBlock
		if err := decoder.Decode(&block); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}
