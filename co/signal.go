// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package co

import (
	"sync"
)

type SignalData struct {
	Data interface{}
}

// Waiter provides channel to wait for.
type Waiter interface {
	C() <-chan SignalData
}

// Signal a rendezvous point for goroutines waiting for or announcing the occurrence of an event.
// It's more friendly than sync.Cond, since it's channel base. That means you can do channel selection
// to wait for an event.
type Signal struct {
	l       sync.Mutex
	waiters []chan SignalData // Track all active waiters
}

// Broadcast wakes all goroutines that are waiting on s.
func (s *Signal) Broadcast(data interface{}) {
	s.l.Lock()
	defer s.l.Unlock()

	signalData := SignalData{Data: data}

	for _, ch := range s.waiters {
		select {
		case ch <- signalData: // Send to each waiter
		default:
		}
	}
}

// NewWaiter create a Waiter object for acquiring channel to wait for.
func (s *Signal) NewWaiter() Waiter {
	s.l.Lock()
	defer s.l.Unlock()

	ch := make(chan SignalData, 1) // Create a new buffered channel for each waiter
	s.waiters = append(s.waiters, ch)

	return waiterFunc(func() <-chan SignalData {
		return ch
	})
}

type waiterFunc func() <-chan SignalData

func (w waiterFunc) C() <-chan SignalData {
	return w()
}
