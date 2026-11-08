package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
)

// PortfolioLimits represents capital risk constraints.
type PortfolioLimits struct {
	MaxPositionSize int     `json:"max_position_size"`
	MaxExposure     float64 `json:"max_exposure"`
	MaxSlippagePct  float64 `json:"max_slippage_pct"` // e.g. 0.025 for 2.5%
}

// LimitsReader defines the contract to fetch portfolio limits.
type LimitsReader interface {
	ReadLimits() (*PortfolioLimits, error)
}

// LocalLimitsReader implements LimitsReader returning static mock limits.
type LocalLimitsReader struct {
	limits *PortfolioLimits
}

func NewLocalLimitsReader(maxSize int, maxExp float64, maxSlip float64) *LocalLimitsReader {
	return &LocalLimitsReader{
		limits: &PortfolioLimits{
			MaxPositionSize: maxSize,
			MaxExposure:     maxExp,
			MaxSlippagePct:  maxSlip,
		},
	}
}

func (l *LocalLimitsReader) ReadLimits() (*PortfolioLimits, error) {
	return l.limits, nil
}

// PriceLevel represents a single row in the order book.
type PriceLevel struct {
	Price  float64 `json:"price"`
	Volume int     `json:"volume"`
}

// OrderBook represents the top 5 bid/ask depth for a contract.
type OrderBook struct {
	ContractID string       `json:"contract_id"`
	Bids       []PriceLevel `json:"bids"` // Sorted descending
	Asks       []PriceLevel `json:"asks"` // Sorted ascending
	// AsOfMs is the capture time stamped by marketdata (0 = unstamped/mock).
	AsOfMs int64 `json:"as_of_ms,omitempty"`
}

// OrderBookFetcher wraps a Redis client to query real-time order books.
type OrderBookFetcher struct {
	client    *redis.Client
	redisAddr string
	// allowMock permits fabricated fallback books when no live book exists in
	// Redis. Default false, mirroring breen's ALLOW_MOCK_ORDERBOOK: the 2026-07-05
	// post-mortem found proposals were routinely priced/sized against books
	// synthesized from the ticker string length whenever a real book was missing.
	// Enable only for keyless dev/tests.
	allowMock bool
}

func NewOrderBookFetcher(redisAddr string) *OrderBookFetcher {
	var client *redis.Client
	if redisAddr != "" {
		client = redis.NewClient(&redis.Options{
			Addr:        redisAddr,
			DialTimeout: 2 * time.Second,
			ReadTimeout: 1 * time.Second,
		})
	}
	return &OrderBookFetcher{
		client:    client,
		redisAddr: redisAddr,
		allowMock: getEnvBool("ALLOW_MOCK_ORDERBOOK", false),
	}
}

// GetOrderBook retrieves the order book from Redis. With no live book it errors
// (skipping the proposal) unless mock fallback is explicitly enabled for keyless dev.
func (obf *OrderBookFetcher) GetOrderBook(ctx context.Context, contractID string) (*OrderBook, error) {
	if obf.client != nil {
		val, err := obf.client.Get(ctx, "orderbook:"+contractID).Result()
		if err == nil {
			var ob OrderBook
			if err := json.Unmarshal([]byte(val), &ob); err == nil {
				return &ob, nil
			}
		}
	}
	if obf.allowMock {
		return obf.generateMockBook(contractID), nil
	}
	return nil, fmt.Errorf("no live order book for %s (mock fallback disabled)", contractID)
}

func (obf *OrderBookFetcher) generateMockBook(contractID string) *OrderBook {
	// Base price derived from contract name length or hash to keep it deterministic but distinct
	basePrice := 0.50
	if len(contractID) > 0 {
		basePrice = 0.30 + float64(len(contractID)%40)/100.0
	}

	// Generate 5 bids and 5 asks
	bids := make([]PriceLevel, 5)
	asks := make([]PriceLevel, 5)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < 5; i++ {
		// Bids: price decreases, volume decreases or fluctuates
		bidPrice := basePrice - float64(i+1)*0.01
		bidVol := 300 + rng.Intn(400) - (i * 50)
		if bidVol < 50 {
			bidVol = 50
		}
		bids[i] = PriceLevel{Price: math.Round(bidPrice*100) / 100, Volume: bidVol}

		// Asks: price increases, volume decreases or fluctuates
		askPrice := basePrice + float64(i)*0.01
		askVol := 250 + rng.Intn(350) - (i * 40)
		if askVol < 50 {
			askVol = 50
		}
		asks[i] = PriceLevel{Price: math.Round(askPrice*100) / 100, Volume: askVol}
	}

	return &OrderBook{
		ContractID: contractID,
		Bids:       bids,
		Asks:       asks,
	}
}

// IcebergOrder represents a single chunked transaction.
type IcebergOrder struct {
	ContractID string    `json:"contract_id"`
	Action     string    `json:"action"`
	Size       int       `json:"size"`
	Price      float64   `json:"price"`
	DelayMS    int       `json:"delay_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// EvaluationResult summarizes the Liquidity Impact Engine's output.
type EvaluationResult struct {
	ContractID     string         `json:"contract_id"`
	Action         string         `json:"action"`
	OriginalSize   int            `json:"original_size"`
	AdjustedSize   int            `json:"adjusted_size"`
	AvgFillPrice   float64        `json:"avg_fill_price"`
	SlippagePct    float64        `json:"slippage_pct"`
	LimitBreached  bool           `json:"limit_breached"`
	BreachReason   string         `json:"breach_reason,omitempty"`
	IcebergChunks  []IcebergOrder `json:"iceberg_chunks"`
	EvaluationTime time.Duration  `json:"evaluation_time"`
}

// LiquidityEngine coordinates the trading evaluation logic.
type LiquidityEngine struct {
	limitsReader     LimitsReader
	orderBookFetcher *OrderBookFetcher
}

func NewLiquidityEngine(lr LimitsReader, obf *OrderBookFetcher) *LiquidityEngine {
	return &LiquidityEngine{
		limitsReader:     lr,
		orderBookFetcher: obf,
	}
}

// Evaluate performs deterministic slippage bounds calculation and outputs chunked execution steps.
func (le *LiquidityEngine) Evaluate(ctx context.Context, contractID string, action string, targetSize int, maxPrice float64) (*EvaluationResult, error) {
	start := time.Now()
	res := &EvaluationResult{
		ContractID:   contractID,
		Action:       action,
		OriginalSize: targetSize,
	}

	if targetSize <= 0 {
		res.EvaluationTime = time.Since(start)
		return res, errors.New("target size must be greater than zero")
	}

	// 1. Fetch Portfolio Limits
	limits, err := le.limitsReader.ReadLimits()
	if err != nil {
		limits = &PortfolioLimits{MaxPositionSize: 2000, MaxExposure: 5000.0, MaxSlippagePct: 0.025}
	}

	// 2. Apply Risk Sizing Capping
	adjustedSize := targetSize

	// Cap by absolute position size limits
	if adjustedSize > limits.MaxPositionSize {
		adjustedSize = limits.MaxPositionSize
		res.LimitBreached = true
		res.BreachReason = fmt.Sprintf("Target size %d exceeded max position limit %d", targetSize, limits.MaxPositionSize)
	}

	// Cap by exposure (targetSize * maxPrice cannot exceed exposure limit)
	estimatedExposure := float64(adjustedSize) * maxPrice
	if estimatedExposure > limits.MaxExposure {
		maxAffordableSize := int(limits.MaxExposure / maxPrice)
		if maxAffordableSize < adjustedSize {
			adjustedSize = maxAffordableSize
			res.LimitBreached = true
			res.BreachReason = fmt.Sprintf("Exposure $%.2f exceeded limit $%.2f. Scaled size to %d", estimatedExposure, limits.MaxExposure, adjustedSize)
		}
	}

	if adjustedSize <= 0 {
		res.AdjustedSize = 0
		res.EvaluationTime = time.Since(start)
		return res, nil
	}

	// 3. Query Order Book Depth
	ob, err := le.orderBookFetcher.GetOrderBook(ctx, contractID)
	if err != nil {
		res.EvaluationTime = time.Since(start)
		return nil, fmt.Errorf("failed to fetch order book: %w", err)
	}

	// 4. Compute Slippage over Depth
	var levels []PriceLevel
	if action == "BUY" {
		levels = ob.Asks
	} else {
		levels = ob.Bids
	}

	if len(levels) == 0 {
		res.EvaluationTime = time.Since(start)
		return nil, errors.New("empty order book side for execution")
	}

	bestPrice := levels[0].Price
	remaining := adjustedSize
	totalCost := 0.0
	filled := 0

	for _, level := range levels {
		if remaining <= 0 {
			break
		}
		fillAmount := level.Volume
		if fillAmount > remaining {
			fillAmount = remaining
		}

		totalCost += float64(fillAmount) * level.Price
		filled += fillAmount
		remaining -= fillAmount
	}

	// If liquidity is too thin to fulfill adjustedSize, scale down to what was actually fillable
	if remaining > 0 {
		adjustedSize = filled
		res.LimitBreached = true
		res.BreachReason = fmt.Sprintf("Insufficient order book depth. Truncated size to %d", adjustedSize)
	}

	if adjustedSize == 0 {
		res.AdjustedSize = 0
		res.EvaluationTime = time.Since(start)
		return res, nil
	}

	avgFillPrice := totalCost / float64(adjustedSize)

	// Calculate Slippage in ABSOLUTE price terms — the adverse move from the best
	// quote to the realized average fill, measured in contract price (0–1), not as a
	// fraction of the best price. Relative-to-best slippage explodes on sub-dime
	// binaries (a 0.01→0.02 fill reads as 100%) and was rejecting/shrinking otherwise
	// fine trades; on a probability contract the meaningful quantity is cents of price
	// moved. limits.MaxSlippagePct is likewise read as an absolute price cap (0.03 = 3¢).
	var slippagePct float64
	if action == "BUY" {
		slippagePct = avgFillPrice - bestPrice
	} else {
		slippagePct = bestPrice - avgFillPrice
	}

	// If slippage exceeds maximum allowed, scale down trade size iteratively to stay within boundaries
	if slippagePct > limits.MaxSlippagePct {
		res.LimitBreached = true
		res.BreachReason = fmt.Sprintf("Calculated slippage %.3f (abs price) exceeded limit %.3f. Optimizing trade size...", slippagePct, limits.MaxSlippagePct)

		// Iteratively decrement sizing to find maximum executable size under the slippage boundary
		optimizedSize := adjustedSize
		for optimizedSize > 10 {
			optimizedSize -= 10
			tempCost := 0.0
			tempRemaining := optimizedSize
			tempFilled := 0

			for _, level := range levels {
				if tempRemaining <= 0 {
					break
				}
				fillAmount := level.Volume
				if fillAmount > tempRemaining {
					fillAmount = tempRemaining
				}

				tempCost += float64(fillAmount) * level.Price
				tempFilled += fillAmount
				tempRemaining -= fillAmount
			}

			tempAvgPrice := tempCost / float64(optimizedSize)
			var tempSlippage float64
			if action == "BUY" {
				tempSlippage = tempAvgPrice - bestPrice
			} else {
				tempSlippage = bestPrice - tempAvgPrice
			}

			if tempSlippage <= limits.MaxSlippagePct {
				adjustedSize = optimizedSize
				avgFillPrice = tempAvgPrice
				slippagePct = tempSlippage
				break
			}
		}
	}

	res.AdjustedSize = adjustedSize
	res.AvgFillPrice = math.Round(avgFillPrice*1000) / 1000
	res.SlippagePct = math.Round(slippagePct*10000) / 10000

	// 5. Build Iceberg Chunks
	// Split adjusted size into chunks of size 50 - 150 contracts
	chunks := []IcebergOrder{}
	chunkRemaining := adjustedSize
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	accumulatedDelay := 0

	for chunkRemaining > 0 {
		chunkSize := 50 + rng.Intn(101) // 50 to 150
		if chunkSize > chunkRemaining {
			chunkSize = chunkRemaining
		}

		// Calculate price index for this specific chunk
		chunkPrice := avgFillPrice
		// Provide slight mock variance on price levels per chunk
		variance := (rng.Float64() - 0.5) * 0.002 // +/- 0.1% variance
		chunkPrice = avgFillPrice * (1.0 + variance)

		chunks = append(chunks, IcebergOrder{
			ContractID: contractID,
			Action:     action,
			Size:       chunkSize,
			Price:      math.Round(chunkPrice*100) / 100,
			DelayMS:    accumulatedDelay,
			Timestamp:  time.Now().Add(time.Duration(accumulatedDelay) * time.Millisecond),
		})

		chunkRemaining -= chunkSize
		// Increase delay sequentially (50ms - 250ms gap)
		accumulatedDelay += 50 + rng.Intn(201)
	}

	res.IcebergChunks = chunks
	res.EvaluationTime = time.Since(start)
	return res, nil
}
