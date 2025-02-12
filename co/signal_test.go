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

	done := make(chan struct{})
	payloads := make([]string, 0, numberOfBlocks)
	go func() {
		ticker := thorChain.Repo().NewTicker()
		for {
			select {
			case signalData := <-ticker.C():
				_, ok := signalData.Data.(thor.Bytes32)
				if ok {
					payloads = append(payloads, signalData.Data.(thor.Bytes32).String())
				}
			case <-done:
				return
			}
		}
	}()

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

	close(done)

	allBlocks, err := thorChain.GetAllBlocks()
	assert.NoError(t, err)
	assert.Len(t, allBlocks, numberOfBlocks)
	// Every block except for the genesis block
	assert.Equal(t, numberOfBlocks-1, len(payloads))
	expectedPayloads := []string{
		"0x00000001d5e941d4a4576487992c2276d12dff6e30797636dedac986793bb1c8",
		"0x00000002fc9e017e8139224f446237e7598de09c6e00bf7062dbf78217e2c5c9",
		"0x00000003258c00ab61649bf9537c10b2ce2a8339b34b14b627e72a7d11fd7895",
		"0x000000044e1dab4195c3986712bc8bef83d6352081f3374c8d63cc14b37ddadd",
		"0x00000005af187489c3f98fc0e282b8c56219c01e8bfc52fd073115a25049576f",
		"0x000000062261d57eb11eef930a5d366879881b9aa5f284cdf72bcc92708741d6",
		"0x0000000738bf86373d7f62959a779ee83eea03c6765c1021dfb7d391fbb92b70",
		"0x00000008525c292da882aebd89fa6912afef6523e9237d1203cb7fe2009a84b3",
		"0x00000009cbed4e85692f9cd0373eb1f3f4e76289096806f44f347a42a36f2bb5",
	}
	assert.Equal(t, expectedPayloads, payloads)
}
