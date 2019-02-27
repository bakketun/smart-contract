package data

import (
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/tokenized/smart-contract/pkg/wire"
)

// MemPool is used for managing announced transactions that haven't confirmed yet.
// The mempool is non-persistent and is mainly used to prevent duplicate tx requests.
type MemPool struct {
	txs      map[chainhash.Hash]memPoolTx         // Lookup of block height by hash.
	inputs   map[chainhash.Hash][]*chainhash.Hash // Lookup by hash of outpoint. Used to find conflicting inputs.
	requests map[chainhash.Hash]time.Time         // Transactions that have been requested
	mutex    sync.Mutex
}

// NewMemPool returns a new MemPool.
func NewMemPool() *MemPool {
	result := MemPool{
		txs:      make(map[chainhash.Hash]memPoolTx),
		inputs:   make(map[chainhash.Hash][]*chainhash.Hash),
		requests: make(map[chainhash.Hash]time.Time),
	}
	return &result
}

// Adds an active request for a tx.
// This is to prevent duplicate requests and receiving the same tx from multiple peers.
// Returns:
//   bool - True if we already have the tx
//   bool - True if the tx should be requested
func (memPool *MemPool) AddRequest(txid *chainhash.Hash) (bool, bool) {
	memPool.mutex.Lock()
	defer memPool.mutex.Unlock()

	_, exists := memPool.txs[*txid]
	if exists {
		return true, false // Already in the mempool
	}

	now := time.Now()
	requestTime, requested := memPool.requests[*txid]
	if !requested || now.Sub(requestTime).Seconds() > 3 {
		// Tx has not been requested yet or the previous request is old
		memPool.requests[*txid] = now
		return false, true
	}

	return false, false // Another request is still active
}

// Adds a timestamped tx hash to the mempool
// Returns:
//   []*chainhash.Hash - list of conflicting transactions (not including this tx) if there are
//     conflicts with inputs (double spends).
//   bool - true if the tx isn't already in the mempool and was added
func (memPool *MemPool) AddTransaction(tx *wire.MsgTx) ([]*chainhash.Hash, bool) {
	memPool.mutex.Lock()
	defer memPool.mutex.Unlock()

	result := make([]*chainhash.Hash, 0)
	hash := tx.TxHash()

	_, exists := memPool.txs[hash]
	if exists {
		return result, false // Already in the mempool
	}

	// Add tx
	newTx := newMemPoolTx(time.Now(), tx)
	memPool.txs[hash] = newTx

	// Add inputs while checking for conflicts
	for _, outpoint := range newTx.outPoints {
		outpointHash := outpoint.OutpointHash()
		list, exists := memPool.inputs[outpointHash]
		if exists {
			// Append conflicting
			// It is possible tx conflict on more than one input and we don't want duplicates in
			//   the result list.
			appendIfNotContained(result, list)
			list = append(list, &hash)
		} else {
			// Create new list with only this tx hash
			list := make([]*chainhash.Hash, 1)
			list[0] = &hash
			memPool.inputs[outpointHash] = list
		}
	}

	return result, true
}

// Appends the items in add to list if they are not already in list
func appendIfNotContained(list []*chainhash.Hash, add []*chainhash.Hash) {
	for _, addHash := range add {
		found := false
		for _, hash := range list {
			if *hash == *addHash {
				found = true
				break
			}
		}

		if !found {
			list = append(list, addHash)
		}
	}
}

// Removes a tx hash from the mempool
// Returns true if the tx was in the mempool
func (memPool *MemPool) RemoveTransaction(hash *chainhash.Hash) bool {
	memPool.mutex.Lock()
	defer memPool.mutex.Unlock()

	tx, exists := memPool.txs[*hash]
	if exists {
		// Remove outpoints
		for _, outpoint := range tx.outPoints {
			outpointHash := outpoint.OutpointHash()
			otherHashes, exists := memPool.inputs[outpointHash]
			if exists { // It should always exist
				if len(otherHashes) > 1 {
					// Remove this outpoint hash from the list
					for i, otherHash := range otherHashes {
						if *otherHash == outpointHash {
							otherHashes = append(otherHashes[:i], otherHashes[i+1:]...)
							break
						}
					}
				} else {
					delete(memPool.inputs, outpointHash)
				}
			}
		}

		// Remove tx
		delete(memPool.txs, *hash)
	}
	return exists
}

// Returns true if the transaction is in the mempool
func (memPool *MemPool) TransactionExists(hash *chainhash.Hash) bool {
	memPool.mutex.Lock()
	defer memPool.mutex.Unlock()

	_, exists := memPool.txs[*hash]
	return exists
}

// Returns txids of any transactions from the mempool with inputs that conflict with the specified
//   transaction.
// Also removes them from the mempool.
func (memPool *MemPool) Conflicting(tx *wire.MsgTx) []*chainhash.Hash {
	memPool.mutex.Lock()
	defer memPool.mutex.Unlock()

	result := make([]*chainhash.Hash, 0, 1)
	// Check for conflicting inputs
	for _, input := range tx.TxIn {
		if list, exists := memPool.inputs[input.PreviousOutPoint.OutpointHash()]; exists {
			for _, hash := range list {
				result = append(result, hash)
				memPool.RemoveTransaction(hash)
			}
		}
	}
	return result
}

type memPoolTx struct {
	time      time.Time
	outPoints []wire.OutPoint
}

func newMemPoolTx(time time.Time, tx *wire.MsgTx) memPoolTx {
	result := memPoolTx{
		time:      time,
		outPoints: make([]wire.OutPoint, 0, len(tx.TxIn)),
	}

	for _, input := range tx.TxIn {
		result.outPoints = append(result.outPoints, input.PreviousOutPoint)
	}

	return result
}
