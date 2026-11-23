/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	Modifications (c) 2016 AvSquirrel (https://github.com/AvSquirrel)
	monotonic.go: Create monotonic clock using Go's monotonic time - necessary because of real time clock changes on RPi.
*/

package main

import (
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
)

// Timer (since start).

type monotonic struct {
	startTime   time.Time
	mu          sync.RWMutex
	realTimeSet bool
	realTimeRef time.Time
	realTimeAt  time.Time
}

func (m *monotonic) GetTime() time.Time {
	return (time.Time{}).Add(time.Since(m.startTime))
}

func (m *monotonic) GetMilliseconds() uint64 {
	ms := time.Since(m.startTime).Milliseconds()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

func (m *monotonic) GetRealTime() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.realTimeSet {
		return time.Time{}
	}
	return m.realTimeRef.Add(time.Since(m.realTimeAt))
}

func (m *monotonic) Since(t time.Time) time.Duration {
	return m.GetTime().Sub(t)
}

func (m *monotonic) HumanizeTime(t time.Time) string {
	return humanize.RelTime(t, m.GetTime(), "ago", "from now")
}

func (m *monotonic) Unix() int64 {
	return int64(m.Since(time.Time{}).Seconds())
}

func (m *monotonic) HasRealTimeReference() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.realTimeSet
}

func (m *monotonic) SetRealTimeReference(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.realTimeSet { // Only allow the real clock to be set once.
		m.realTimeRef = t
		m.realTimeAt = time.Now()
		m.realTimeSet = true
	}
}

func NewMonotonic() *monotonic {
	return &monotonic{
		startTime: time.Now(),
	}
}
