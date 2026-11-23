/*
	Copyright (c) 2021 Adrian Batzill
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	messagequeue.go: Prioritizing queue for slow network connections
*/

package main

import (
	"sort"
	"sync"
	"time"
)

type QueueEntry struct {
	priority   int32
	outdatedAt time.Time
	data       any
}

type MessageQueue struct {
	maxSize       int
	entries       []QueueEntry
	prefixDropped int
	DataAvailable chan bool
	Closed        bool
	mutex         sync.Mutex
}

func NewMessageQueue(maxSize int) *MessageQueue {
	return &MessageQueue{
		maxSize:       maxSize,
		entries:       make([]QueueEntry, 0),
		DataAvailable: make(chan bool, 1),
	}
}

func (queue *MessageQueue) IsClosed() bool {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	return queue.Closed
}

func (queue *MessageQueue) Put(prio int32, maxAge time.Duration, data any) {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	if queue.Closed {
		return
	}

	timeout := stratuxClock.GetTime().Add(maxAge)
	entry := QueueEntry{prio, timeout, data}

	if len(queue.entries) == 0 {
		queue.entries = make([]QueueEntry, 1)
		queue.entries[0] = entry
		queue.prefixDropped = 0
	} else {
		index := queue.findInsertPosition(prio)

		if index == len(queue.entries) {
			queue.entries = append(queue.entries, entry)
		} else {
			queue.entries = append(queue.entries, QueueEntry{})
			copy(queue.entries[index+1:], queue.entries[index:])
			queue.entries[index] = entry
		}
	}

	// Allow 10% over-use before we prune, so the pruning is done in batches to save CPU
	if queue.maxSize > 0 && float32(len(queue.entries)) > float32(queue.maxSize)*1.1 {
		queue.prune()
	}
	if len(queue.entries) != 0 {
		queue.notifyData()
	}
}

func (queue *MessageQueue) PeekFirst() (any, int32) {
	return queue.getFirst(false)
}

func (queue *MessageQueue) PopFirst() (any, int32) {
	return queue.getFirst(true)
}

func (queue *MessageQueue) getFirst(remove bool) (any, int32) {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()

	index := queue.getFirstUsableIndex()
	if index < 0 {
		return nil, 0 // nothing in queue
	}

	// Found one. Zero skipped/popped slots so payloads are eligible for GC.
	entry := queue.entries[index]
	dropCount := index
	if remove {
		queue.entries[index] = QueueEntry{}
		dropCount = index + 1
	}
	queue.entries = queue.entries[dropCount:]
	queue.prefixDropped += dropCount

	if len(queue.entries) == 0 {
		queue.entries = nil
		queue.prefixDropped = 0
	} else if queue.prefixDropped >= 64 && queue.prefixDropped >= len(queue.entries) {
		compacted := make([]QueueEntry, len(queue.entries))
		copy(compacted, queue.entries)
		queue.entries = compacted
		queue.prefixDropped = 0
	}

	return entry.data, entry.priority
}

// Returns the first entry that's not outdated
func (queue *MessageQueue) getFirstUsableIndex() int {
	now := stratuxClock.GetTime()
	for i, data := range queue.entries {
		if data.outdatedAt.Before(now) {
			// Entry already timed out; zero slot to release payload reference.
			queue.entries[i] = QueueEntry{}
			continue
		}
		// found one
		return i
	}

	// Nothing current in queue
	if len(queue.entries) > 0 {
		queue.entries = nil
		queue.prefixDropped = 0
	}

	return -1
}

func (queue *MessageQueue) GetQueueDump(pruneFirst bool) []any {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()

	if pruneFirst {
		queue.prune()
	}
	data := make([]any, len(queue.entries))
	for i, d := range queue.entries {
		data[i] = d.data
	}
	return data
}

// Removes elements from the queue so it fits its maxSize
// - All outdated elements are discarded
// - Low priority elements are discarded, starting with the oldest ones
func (queue *MessageQueue) prune() {
	// Group into priority categories, so we can then strip the beginning of each category if needed (remove oldest messages)
	newEntries := make([][]QueueEntry, 0)
	totalUsable := 0
	prevPrio := int32(999999999)
	now := stratuxClock.GetTime()
	for _, entry := range queue.entries {
		if entry.outdatedAt.Before(now) {
			continue // outdated, remove completely
		}
		totalUsable++
		if len(newEntries) == 0 || entry.priority != prevPrio {
			// new prio-category
			newEntries = append(newEntries, make([]QueueEntry, 0))
		}
		newEntries[len(newEntries)-1] = append(newEntries[len(newEntries)-1], entry)
		prevPrio = entry.priority
	}
	toBeRemoved := totalUsable - queue.maxSize
	if toBeRemoved > 0 {
		for i := len(newEntries) - 1; i >= 0; i-- {
			// From lowest to highest prio, remove the oldest messages of each category until we have few enough in total
			if len(newEntries[i]) >= toBeRemoved {
				// can remove enough in this category
				newEntries[i] = newEntries[i][toBeRemoved:]
				break
			} else {
				// remove this category, then proceed with next higher prio one
				toBeRemoved -= len(newEntries[i])
				newEntries[i] = nil
			}
		}
	}

	// finally, copy everything back to our queue
	kept := 0
	for _, category := range newEntries {
		kept += len(category)
	}
	queue.entries = make([]QueueEntry, 0, kept)
	for _, category := range newEntries {
		if category != nil {
			queue.entries = append(queue.entries, category...)
		}
	}
	queue.prefixDropped = 0
}

func (queue *MessageQueue) findInsertPosition(priority int32) int {
	index := sort.Search(len(queue.entries), func(i int) bool {
		// > instead of >= so we get to the first entry that is larger - in order to keep insertion order
		// for equal-priority messages
		return queue.entries[i].priority > priority
	})

	return index
}

func (queue *MessageQueue) notifyData() {
	select {
	case queue.DataAvailable <- true:
	default:
	}
}

func (queue *MessageQueue) Close() {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	if queue.Closed {
		return
	}
	queue.Closed = true
	queue.entries = nil
	queue.prefixDropped = 0
	queue.notifyData()
}
