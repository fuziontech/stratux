package main

import (
	"sync"
	"testing"
	"time"
)

func TestMonotonicBasic(t *testing.T) {
	m := NewMonotonic()

	t1 := m.GetTime()
	ms1 := m.GetMilliseconds()
	time.Sleep(15 * time.Millisecond)
	t2 := m.GetTime()
	ms2 := m.GetMilliseconds()

	if !t2.After(t1) {
		t.Fatalf("expected monotonic time to advance: t1=%v t2=%v", t1, t2)
	}
	if ms2 < ms1 {
		t.Fatalf("expected monotonic milliseconds to be non-decreasing: ms1=%d ms2=%d", ms1, ms2)
	}
	if d := m.Since(t1); d <= 0 {
		t.Fatalf("expected positive Since(t1), got %v", d)
	}
	if m.Unix() < 0 {
		t.Fatalf("expected non-negative Unix(), got %d", m.Unix())
	}
	if s := m.HumanizeTime(t1); s == "" {
		t.Fatalf("expected non-empty HumanizeTime output")
	}
}

func TestMonotonicRealTimeReference(t *testing.T) {
	m := NewMonotonic()

	if m.HasRealTimeReference() {
		t.Fatalf("expected HasRealTimeReference() to be false initially")
	}
	if !m.GetRealTime().IsZero() {
		t.Fatalf("expected zero GetRealTime() before SetRealTimeReference")
	}

	ref1 := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	ref2 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	m.SetRealTimeReference(ref1)
	m.SetRealTimeReference(ref2) // Must be ignored (only set once).

	if !m.HasRealTimeReference() {
		t.Fatalf("expected HasRealTimeReference() to be true after set")
	}

	time.Sleep(10 * time.Millisecond)
	rt := m.GetRealTime()
	if rt.Before(ref1) || rt.After(ref1.Add(5*time.Second)) {
		t.Fatalf("unexpected GetRealTime()=%v for ref1=%v", rt, ref1)
	}
}

func TestMonotonicConcurrent(t *testing.T) {
	m := NewMonotonic()
	ref := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = m.GetTime()
				_ = m.GetMilliseconds()
				_ = m.GetRealTime()
				_ = m.HasRealTimeReference()
				_ = m.Unix()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			m.SetRealTimeReference(ref.Add(time.Duration(j) * time.Second))
		}
	}()
	wg.Wait()
}
