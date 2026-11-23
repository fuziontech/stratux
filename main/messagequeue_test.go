package main

import (
	"sync"
	"testing"
	"time"
)

func ensureTestClock() {
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}
}

func TestMessageQueuePriorityAndFIFO(t *testing.T) {
	ensureTestClock()

	q := NewMessageQueue(10)
	q.Put(5, time.Minute, "low-1")
	q.Put(1, time.Minute, "high-1")
	q.Put(1, time.Minute, "high-2")
	q.Put(5, time.Minute, "low-2")

	expected := []struct {
		val  string
		prio int32
	}{
		{"high-1", 1},
		{"high-2", 1},
		{"low-1", 5},
		{"low-2", 5},
	}

	for _, exp := range expected {
		peekVal, peekPrio := q.PeekFirst()
		if peekVal != exp.val || peekPrio != exp.prio {
			t.Fatalf("PeekFirst got (%v, %d), want (%v, %d)", peekVal, peekPrio, exp.val, exp.prio)
		}
		gotVal, gotPrio := q.PopFirst()
		if gotVal != exp.val || gotPrio != exp.prio {
			t.Fatalf("PopFirst got (%v, %d), want (%v, %d)", gotVal, gotPrio, exp.val, exp.prio)
		}
	}

	if v, _ := q.PopFirst(); v != nil {
		t.Fatalf("expected empty queue, got %v", v)
	}
}

func TestMessageQueueExpirationAndClose(t *testing.T) {
	ensureTestClock()

	q := NewMessageQueue(10)
	q.Put(1, -time.Second, "expired")
	q.Put(2, time.Minute, "valid")

	v, prio := q.PopFirst()
	if v != "valid" || prio != 2 {
		t.Fatalf("expected valid item after skipping expired, got (%v, %d)", v, prio)
	}

	if q.IsClosed() {
		t.Fatalf("expected queue not closed")
	}
	q.Close()
	if !q.IsClosed() {
		t.Fatalf("expected queue closed")
	}

	// Put after Close must be ignored.
	q.Put(0, time.Minute, "ignored")
	if v, _ := q.PopFirst(); v != nil {
		t.Fatalf("expected nil after Close, got %v", v)
	}
}

func TestMessageQueueConcurrent(t *testing.T) {
	ensureTestClock()

	q := NewMessageQueue(128)
	var wg sync.WaitGroup

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				q.Put(int32(i%5), time.Minute, []byte{byte(id), byte(i)})
				_ = q.IsClosed()
			}
		}(w)
	}

	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				_, _ = q.PopFirst()
			}
		}()
	}

	time.Sleep(5 * time.Millisecond)
	q.Close()
	wg.Wait()
}
