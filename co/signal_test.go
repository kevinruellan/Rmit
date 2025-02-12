// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package co_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vechain/thor/v2/co"
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
