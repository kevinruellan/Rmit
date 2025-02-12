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
	l        sync.Mutex
	ch       chan SignalData
}

// Broadcast wakes all goroutines that are waiting on s.
func (s *Signal) Broadcast(data interface{}) {
	s.l.Lock()
	defer s.l.Unlock()

	// Close and recreate the channel to ensure old waiters are cleaned up
	if s.ch != nil {
		close(s.ch)
	}
	s.ch = make(chan SignalData, 1)
}

// NewWaiter create a Waiter object for acquiring channel to wait for.
func (s *Signal) NewWaiter() Waiter {
	s.l.Lock()
	defer s.l.Unlock()

	if s.ch == nil {
		s.ch = make(chan SignalData, 1)
	}

	ref := s.ch
	return waiterFunc(func() <-chan SignalData {
		return ref
	})
}

type waiterFunc func() <-chan SignalData

func (w waiterFunc) C() <-chan SignalData {
	return w()
}
