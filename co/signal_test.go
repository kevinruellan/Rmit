// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package co_test

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vechain/thor/v2/co"
	"github.com/vechain/thor/v2/genesis"
	"github.com/vechain/thor/v2/test/testchain"
	"github.com/vechain/thor/v2/thor"
	"github.com/vechain/thor/v2/tx"
)

func TestSignalBroadcastBeforeWait(t *testing.T) {
	const payload = "payload"
	var sig co.Signal
	sig.Broadcast(payload)

	var ws []co.Waiter
	for i := 0; i < 10; i++ {
		ws = append(ws, sig.NewWaiter())
	}

	var noWaiters int
	for _, w := range ws {
		select {
		case <-w.C():
		default:
			noWaiters++
		}
	}
	assert.Equal(t, 10, noWaiters)
}

func TestSignalBroadcastAfterWait(t *testing.T) {
	var sig co.Signal

	var ws []co.Waiter
	const numberOfWaiters = 10
	for i := 0; i < numberOfWaiters; i++ {
		ws = append(ws, sig.NewWaiter())
	}

	const payload = "payload"
	sig.Broadcast(payload)

	validatePayloadForWaiters(t, numberOfWaiters, ws)
}

func TestSignalBroadcastConsecutiveValues(t *testing.T) {
	var sig co.Signal

	var ws []co.Waiter
	const numberOfWaiters = 10
	for i := 0; i < numberOfWaiters; i++ {
		ws = append(ws, sig.NewWaiter())
	}

	// We now broadcast 10 consecutive values
	// simulating block numbers
	for i := 0; i < numberOfWaiters; i++ {
		sig.Broadcast(i)

		validatePayloadForWaiters(t, numberOfWaiters, ws)
	}
}

func validatePayloadForWaiters(t *testing.T, numberOfWaiters int, ws []co.Waiter) {
	var signalWaiters int
	payloads := make([]interface{}, 0, numberOfWaiters)
	for _, w := range ws {
		select {
		case signalData := <-w.C():
			signalWaiters++
			payloads = append(payloads, signalData.Data)
		default:
		}
	}

	assert.Equal(t, numberOfWaiters, signalWaiters)
	for i, payload := range payloads {
		assert.Equal(t, payload, payloads[i])
	}
}

func TestIntegration(t *testing.T) {
	numberOfBlocks := 10
	thorChain, err := testchain.NewIntegrationTestChain()
	assert.NoError(t, err)

	// This is meant to get the best block signal
	done := make(chan struct{})
	blockNumbers := make([]uint32, 0, numberOfBlocks)
	go func() {
		ticker := thorChain.Repo().NewTicker()
		for {
			select {
			case signalData := <-ticker.C():
				blockID, ok := signalData.Data.(thor.Bytes32)
				if ok {
					summary, err := thorChain.Repo().GetBlockSummary(blockID)
					assert.NoError(t, err)
					blockNumbers = append(blockNumbers, summary.Header.Number())
				}
			case <-done:
				return
			}
		}
	}()

	// Build and mint transactions
	addr := thor.BytesToAddress([]byte("to"))
	cla := tx.NewClause(&addr).WithValue(big.NewInt(10000))

	var testTx *tx.Transaction

	for i := 0; i < numberOfBlocks-1; i++ {
		testTx = new(tx.Builder).
			ChainTag(thorChain.Repo().ChainTag()).
			Expiration(10).
			Gas(21000).
			Nonce(uint64(i)).
			Clause(cla).
			BlockRef(tx.NewBlockRef(uint32(i))).
			Build()
		testTx = tx.MustSign(testTx, genesis.DevAccounts()[0].PrivateKey)
		assert.NoError(t, thorChain.MintTransactions(genesis.DevAccounts()[0], testTx))
	}

	// Validation
	close(done)
	allBlocks, err := thorChain.GetAllBlocks()
	assert.NoError(t, err)
	assert.Len(t, allBlocks, numberOfBlocks)
	// Every block except for the genesis block
	assert.Equal(t, numberOfBlocks-1, len(blockNumbers))
	// Might not be in order
	for i := 1; i <= numberOfBlocks-1; i++ {
		assert.Contains(t, blockNumbers, uint32(i))
	}
}
