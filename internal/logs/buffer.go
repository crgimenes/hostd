package logs

import (
	"strings"
	"sync"
	"time"
)

// Buffer holds the most recent records and hands them to readers.
//
// It is bounded on purpose: a log that grows until the machine runs out of
// memory is a bug, not technical debt. The durable store with retention
// arrives later; the ceiling exists from the first day.
type Buffer struct {
	mu       sync.Mutex
	records  []Record
	capacity int
	next     uint64
	// dropped counts records that fell out of the buffer, so a reader can
	// tell "nothing happened" from "I could not keep it all".
	dropped uint64

	watchers map[int]chan Record
	nextID   int
}

// NewBuffer returns a buffer holding at most capacity records.
func NewBuffer(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{
		records:  make([]Record, 0, capacity),
		capacity: capacity,
		watchers: make(map[int]chan Record),
	}
}

// Append stores a record, stamping it with the next sequence number, and
// returns the stored copy.
func (b *Buffer) Append(r Record) Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	r.Seq = b.next
	if r.Time.IsZero() {
		// A record with no time is unreadable later. Filling it here means
		// every caller gets one without having to remember.
		r.Time = time.Now()
	}
	if len(b.records) == b.capacity {
		copy(b.records, b.records[1:])
		b.records = b.records[:b.capacity-1]
		b.dropped++
	}
	b.records = append(b.records, r)
	// Delivery happens under the same lock that unregisters a watcher, which
	// is what makes closing a watcher channel safe. It cannot stall a writer
	// because the send never blocks: a watcher that cannot keep up loses
	// lines rather than holding up the service producing them.
	for _, ch := range b.watchers {
		select {
		case ch <- r:
		default:
		}
	}
	return r
}

// Query describes which records a reader wants.
type Query struct {
	Service string
	Stream  string
	Match   string
	// Limit is the maximum number of records returned, most recent last.
	Limit int
	// Since returns only records with a sequence above this value, which is
	// how a follower resumes without repeating what it already printed.
	Since uint64
}

// Matches reports whether a record satisfies the query. A follower receives
// everything appended, so it filters with the same rule a search uses.
func (q Query) Matches(r Record) bool {
	if q.Service != "" && r.Service != q.Service {
		return false
	}
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if q.Since > 0 && r.Seq <= q.Since {
		return false
	}
	if q.Match != "" && !strings.Contains(strings.ToLower(r.Text), strings.ToLower(q.Match)) {
		return false
	}
	return true
}

// Search returns the records matching q, oldest first.
func (b *Buffer) Search(q Query) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := q.Limit
	if limit <= 0 || limit > b.capacity {
		limit = b.capacity
	}
	out := make([]Record, 0, min(limit, len(b.records)))
	// Walk backwards so a limit keeps the most recent records, then restore
	// chronological order for the reader.
	for i := len(b.records) - 1; i >= 0 && len(out) < limit; i-- {
		if q.Matches(b.records[i]) {
			out = append(out, b.records[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Dropped reports how many records fell out of the buffer.
func (b *Buffer) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Len reports how many records are held.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.records)
}

// Watch delivers records appended from now on. The returned function stops the
// delivery and must be called, or the buffer keeps feeding a channel nobody
// reads.
func (b *Buffer) Watch(bufferSize int) (<-chan Record, func()) {
	if bufferSize < 1 {
		bufferSize = 1
	}
	ch := make(chan Record, bufferSize)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.watchers[id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.watchers, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}
