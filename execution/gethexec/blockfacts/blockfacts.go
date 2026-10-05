// Package blockfacts records each block's per-transaction state diff during the node's only
// execution of the block, and serves it over click_blockFacts.
//
// The diff is geth's prestateTracer in diff mode without code, the exact payload the indexer
// requests from debug_traceBlockByHash. Serving it from here spares the node a second execution.
package blockfacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/tracers"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native" // registers prestateTracer
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
)

// Name is the live tracer name for --execution.vmtrace.tracer-name.
const Name = "clickBlockFacts"

const defaultCapacity = 1024

// prestateConfig must match BlockDiff::tracer_options in the indexer.
var prestateConfig = json.RawMessage(`{"diffMode":true,"disableCode":true}`)

var (
	sealedCounter     = metrics.NewRegisteredCounter("click/blockfacts/sealed", nil)
	incompleteCounter = metrics.NewRegisteredCounter("click/blockfacts/incomplete", nil)
	txFailedCounter   = metrics.NewRegisteredCounter("click/blockfacts/tx_trace_failed", nil)
)

// active is the process's one collector. Seal and the API reach it here, because the live
// tracer directory hands the engine only the hooks.
var active atomic.Pointer[collector]

func init() {
	tracers.LiveDirectory.Register(Name, newCollector)
}

// TxFacts is one transaction's diff, in debug_traceBlockByHash's per-transaction shape.
type TxFacts struct {
	TxHash common.Hash     `json:"txHash"`
	Result json.RawMessage `json:"result"`
}

type config struct {
	// Capacity is how many recent blocks stay servable.
	Capacity int `json:"capacity"`
}

// collector builds each block's facts as the block executes.
//
// The hooks and Seal run on the block-producing goroutine under the engine's createBlockMutex,
// so tx state needs no lock. mu guards only the served blocks, which the RPC reads concurrently.
type collector struct {
	chainConfig *params.ChainConfig

	tx      *tracers.Tracer // prestate tracer of the transaction executing now
	txHash  common.Hash
	pending map[common.Hash]json.RawMessage // diffs of transactions that succeeded, by tx hash

	mu       sync.RWMutex
	blocks   map[common.Hash][]TxFacts
	ring     []common.Hash // block hashes in insertion order; the oldest is evicted first
	next     int
	capacity int
}

func newCollector(raw json.RawMessage) (*tracing.Hooks, error) {
	cfg := config{Capacity: defaultCapacity}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("blockfacts: parse config: %w", err)
	}
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("blockfacts: capacity must be positive, got %d", cfg.Capacity)
	}
	c := &collector{
		pending:  make(map[common.Hash]json.RawMessage),
		blocks:   make(map[common.Hash][]TxFacts, cfg.Capacity),
		ring:     make([]common.Hash, cfg.Capacity),
		capacity: cfg.Capacity,
	}
	if !active.CompareAndSwap(nil, c) {
		return nil, errors.New("blockfacts: a collector already exists in this process")
	}
	return &tracing.Hooks{
		OnBlockchainInit: c.onBlockchainInit,
		OnTxStart:        c.onTxStart,
		OnTxEnd:          c.onTxEnd,
		OnOpcode:         c.onOpcode,
		// Arbitrum reports ArbOS storage access outside the EVM's opcodes.
		CaptureArbitrumStorageGet: c.captureArbitrumStorageGet,
		CaptureArbitrumStorageSet: c.captureArbitrumStorageSet,
	}, nil
}

func (c *collector) onBlockchainInit(chainConfig *params.ChainConfig) {
	c.chainConfig = chainConfig
}

func (c *collector) onTxStart(env *tracing.VMContext, tx *types.Transaction, from common.Address) {
	c.txHash = tx.Hash()
	t, err := tracers.DefaultDirectory.New("prestateTracer", &tracers.Context{TxHash: c.txHash}, prestateConfig, c.chainConfig)
	if err != nil {
		log.Error("blockfacts: prestate tracer unavailable", "tx", c.txHash, "err", err)
		txFailedCounter.Inc(1)
		c.tx = nil
		return
	}
	c.tx = t
	if t.OnTxStart != nil {
		t.OnTxStart(env, tx, from)
	}
}

func (c *collector) onTxEnd(receipt *types.Receipt, err error) {
	t := c.tx
	c.tx = nil
	if t == nil {
		return
	}
	if t.OnTxEnd != nil {
		t.OnTxEnd(receipt, err)
	}
	// An error means the block producer dropped the transaction, so it has no diff to keep.
	if err != nil {
		return
	}
	result, rerr := t.GetResult()
	if rerr != nil {
		log.Error("blockfacts: prestate result unavailable", "tx", c.txHash, "err", rerr)
		txFailedCounter.Inc(1)
		return
	}
	c.pending[c.txHash] = result
}

func (c *collector) onOpcode(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, rData []byte, depth int, err error) {
	if t := c.tx; t != nil && t.OnOpcode != nil {
		t.OnOpcode(pc, op, gas, cost, scope, rData, depth, err)
	}
}

func (c *collector) captureArbitrumStorageGet(key common.Hash, depth int, before bool) {
	if t := c.tx; t != nil && t.CaptureArbitrumStorageGet != nil {
		t.CaptureArbitrumStorageGet(key, depth, before)
	}
}

func (c *collector) captureArbitrumStorageSet(key, value common.Hash, depth int, before bool) {
	if t := c.tx; t != nil && t.CaptureArbitrumStorageSet != nil {
		t.CaptureArbitrumStorageSet(key, value, depth, before)
	}
}

// Seal stores the facts of block, which has just executed, and does nothing when no collector
// is running.
//
// A transaction is kept only if it is in the final block, at its receipt position. That drops
// transactions the producer executed and then rolled back, whatever rule rolled them back. A
// block with any transaction missing is not stored, so the indexer falls back to
// debug_traceBlockByHash instead of reading a partial diff.
func Seal(block *types.Block) {
	c := active.Load()
	if c == nil {
		return
	}
	defer clear(c.pending)

	txs := block.Transactions()
	facts := make([]TxFacts, len(txs))
	for i, tx := range txs {
		diff, ok := c.pending[tx.Hash()]
		if !ok {
			log.Error("blockfacts: transaction has no diff, block not served", "block", block.Hash(), "number", block.Number(), "tx", tx.Hash(), "index", i)
			incompleteCounter.Inc(1)
			return
		}
		facts[i] = TxFacts{TxHash: tx.Hash(), Result: diff}
	}

	hash := block.Hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	// A reorg can rebuild a block already buffered; it keeps its ring slot.
	if _, ok := c.blocks[hash]; ok {
		c.blocks[hash] = facts
		sealedCounter.Inc(1)
		return
	}
	if evicted := c.ring[c.next]; evicted != (common.Hash{}) {
		delete(c.blocks, evicted)
	}
	c.ring[c.next] = hash
	c.next = (c.next + 1) % c.capacity
	c.blocks[hash] = facts
	sealedCounter.Inc(1)
}

// lookup returns the facts of the block with hash, or nil when it is not buffered.
func lookup(hash common.Hash) []TxFacts {
	c := active.Load()
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[hash]
}

// API serves the click namespace.
type API struct{}

// BlockFacts serves click_blockFacts: the per-transaction diffs of the block with hash, or null
// when the block is not buffered or this node runs no collector.
func (API) BlockFacts(hash common.Hash) []TxFacts {
	return lookup(hash)
}
