package blockfacts

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func newTestCollector(t *testing.T, capacity int) *collector {
	t.Helper()
	active.Store(nil)
	t.Cleanup(func() { active.Store(nil) })
	if _, err := newCollector(json.RawMessage(`{"capacity":` + big.NewInt(int64(capacity)).String() + `}`)); err != nil {
		t.Fatal(err)
	}
	return active.Load()
}

func testBlock(number int64, nonces ...uint64) *types.Block {
	txs := make(types.Transactions, len(nonces))
	for i, n := range nonces {
		txs[i] = types.NewTx(&types.LegacyTx{Nonce: n, GasPrice: big.NewInt(1), Gas: 21000})
	}
	return types.NewBlockWithHeader(&types.Header{Number: big.NewInt(number)}).WithBody(types.Body{Transactions: txs})
}

func diff(s string) json.RawMessage { return json.RawMessage(`{"post":{},"pre":{"` + s + `":{}}}`) }

func TestSealKeepsOnlyBlockTransactionsInOrder(t *testing.T) {
	c := newTestCollector(t, 4)
	block := testBlock(1, 1, 2)
	txs := block.Transactions()
	c.pending[txs[1].Hash()] = diff("b")
	c.pending[txs[0].Hash()] = diff("a")
	c.pending[common.HexToHash("0xdead")] = diff("rolled-back")

	Seal(block)

	got := lookup(block.Hash())
	if len(got) != 2 || got[0].TxHash != txs[0].Hash() || got[1].TxHash != txs[1].Hash() {
		t.Fatalf("facts not aligned with block order: %+v", got)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending not cleared: %d left", len(c.pending))
	}
}

func TestSealSkipsBlockWithMissingTransaction(t *testing.T) {
	c := newTestCollector(t, 4)
	block := testBlock(1, 1, 2)
	c.pending[block.Transactions()[0].Hash()] = diff("a")

	Seal(block)

	if got := lookup(block.Hash()); got != nil {
		t.Fatalf("partial block served: %+v", got)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending not cleared: %d left", len(c.pending))
	}
}

func TestRingEvictsOldestAndKeepsResealedSlot(t *testing.T) {
	newTestCollector(t, 2)
	b1, b2, b3 := testBlock(1), testBlock(2), testBlock(3)

	Seal(b1)
	Seal(b2)
	Seal(b2) // a reorg rebuilt b2; it must not take a second slot
	Seal(b3)

	if lookup(b1.Hash()) != nil {
		t.Fatal("oldest block not evicted")
	}
	if lookup(b2.Hash()) == nil || lookup(b3.Hash()) == nil {
		t.Fatal("recent blocks missing")
	}
}

func TestSealWithoutCollectorIsNoop(t *testing.T) {
	active.Store(nil)
	Seal(testBlock(1, 1))
	if lookup(testBlock(1, 1).Hash()) != nil {
		t.Fatal("served without a collector")
	}
}
