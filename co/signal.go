// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package co

import (
	"sync"
	"time"
)

type TriggerInfo struct {
	Source string    // Who triggered it
	Time   time.Time // When it was triggered
}

// Waiter provides channel to wait for.
// Value read from channel indicates signal or broadcast. true for signal, otherwise broadcast.
type Waiter interface {
	C() <-chan TriggerInfo
}

// Signal a rendezvous point for goroutines waiting for or announcing the occurrence of an event.
// It's more friendly than sync.Cond, since it's channel base. That means you can do channel selection
// to wait for an event.
type Signal struct {
	l       sync.Mutex
	waiters []chan TriggerInfo // Track all active waiters
}

// func (s *Signal) init() {
// 	if s.ch == nil {
// 		s.ch = make(chan TriggerInfo, 1)
// 	}
// }

// Signal wakes one goroutine that is waiting on s.
func (s *Signal) Signal(source string) {
	s.l.Lock()
	defer s.l.Unlock()

	if len(s.waiters) > 0 {
		select {
		case s.waiters[0] <- TriggerInfo{Source: source, Time: time.Now()}:
		default:
		}
	}
}

// Broadcast wakes all goroutines that are waiting on s.
func (s *Signal) Broadcast(source string) {
	s.l.Lock()
	defer s.l.Unlock()

	triggerInfo := TriggerInfo{Source: source, Time: time.Now()}

	for _, ch := range s.waiters {
		select {
		case ch <- triggerInfo: // Send to each waiter
		default:
		}
	}
}

// NewWaiter create a Waiter object for acquiring channel to wait for.
func (s *Signal) NewWaiter() Waiter {
	s.l.Lock()
	defer s.l.Unlock()

	ch := make(chan TriggerInfo, 1) // Create a new buffered channel for each waiter
	s.waiters = append(s.waiters, ch)

	return waiterFunc(func() <-chan TriggerInfo {
		return ch
	})
}

type waiterFunc func() <-chan TriggerInfo

func (w waiterFunc) C() <-chan TriggerInfo {
	return w()
}
