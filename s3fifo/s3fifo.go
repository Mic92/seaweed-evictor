// Package s3fifo implements the S3-FIFO eviction policy (Yang et al., SOSP'23)
// weighted by object size. It only tracks keys; the caller owns the data and
// deletes whatever Insert reports as evicted.
//
// Entries can be pinned. Pinned entries are never evicted, which lets a
// write-back cache protect objects that have not reached the upstream yet.
package s3fifo

const (
	maxFreq       = 3
	smallFraction = 10 // percent of capacity reserved for the small queue
)

type entry[K comparable] struct {
	key        K
	size       int
	freq       int
	pinned     bool
	q          *queue[K]
	prev, next *entry[K]
}

// queue is an intrusive doubly linked list: head is newest, tail is oldest.
type queue[K comparable] struct {
	head, tail *entry[K]
	bytes      int
	n          int
}

func (q *queue[K]) pushFront(e *entry[K]) {
	e.q, e.prev, e.next = q, nil, q.head
	if q.head != nil {
		q.head.prev = e
	} else {
		q.tail = e
	}

	q.head = e
	q.bytes += e.size
	q.n++
}

func (q *queue[K]) remove(e *entry[K]) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		q.head = e.next
	}

	if e.next != nil {
		e.next.prev = e.prev
	} else {
		q.tail = e.prev
	}

	e.q, e.prev, e.next = nil, nil, nil
	q.bytes -= e.size
	q.n--
}

func (q *queue[K]) rotate(e *entry[K]) {
	q.remove(e)
	q.pushFront(e)
}

// Cache is not safe for concurrent use.
type Cache[K comparable] struct {
	capacity int
	small    queue[K]
	main     queue[K]
	ghost    queue[K] // keys only, sizes count against the ghost budget
	// entries holds live and ghost entries; an entry's queue tells which.
	// One map keeps an insert to a single lookup and lets an eviction turn an
	// entry into a ghost without touching the map.
	entries map[K]*entry[K]
}

// New returns a cache that holds at most capacity units (e.g. bytes).
func New[K comparable](capacity int) *Cache[K] {
	return &Cache[K]{
		capacity: capacity,
		entries:  map[K]*entry[K]{},
	}
}

// Size returns the total size of tracked entries.
func (c *Cache[K]) Size() int { return c.small.bytes + c.main.bytes }

// Contains reports whether k is tracked, without counting as a hit.
func (c *Cache[K]) Contains(k K) bool {
	e, ok := c.entries[k]

	return ok && e.q != &c.ghost
}

// Access records a hit.
func (c *Cache[K]) Access(k K) {
	if e, ok := c.entries[k]; ok && e.q != &c.ghost && e.freq < maxFreq {
		e.freq++
	}
}

// Pin protects k from eviction until Unpin.
func (c *Cache[K]) Pin(k K) {
	if e, ok := c.entries[k]; ok && e.q != &c.ghost {
		e.pinned = true
	}
}

// Unpin makes k evictable again.
func (c *Cache[K]) Unpin(k K) {
	if e, ok := c.entries[k]; ok && e.q != &c.ghost {
		e.pinned = false
	}
}

// Remove drops a key without adding it to the ghost queue.
func (c *Cache[K]) Remove(k K) {
	if e, ok := c.entries[k]; ok {
		e.q.remove(e)
		delete(c.entries, k)
	}
}

// Insert adds or resizes a key and returns the keys that must be evicted to
// stay within capacity. The returned keys may include k itself when it is
// larger than the whole cache.
func (c *Cache[K]) Insert(k K, size int) []K {
	return c.put(k, size, false)
}

// InsertPinned is Insert for an entry that must not be evicted, not even by
// its own insertion. Callers use it for data that has not been persisted yet.
func (c *Cache[K]) InsertPinned(k K, size int) []K {
	return c.put(k, size, true)
}

// Evict returns victims until the cache fits its capacity again. Call it after
// Unpin, because unpinning is what makes an over-capacity cache evictable.
func (c *Cache[K]) Evict() []K {
	var victims []K

	for c.Size() > c.capacity {
		e := c.evictOne()
		if e == nil {
			break // everything left is pinned
		}

		victims = append(victims, e.key)
	}

	return victims
}

func (c *Cache[K]) put(k K, size int, pin bool) []K {
	if size > c.capacity {
		c.Remove(k)

		return []K{k}
	}

	e, ok := c.entries[k]

	switch {
	case !ok:
		e = &entry[K]{key: k, size: size, pinned: pin}
		c.entries[k] = e
		c.small.pushFront(e)
	case e.q == &c.ghost:
		// Evicted recently: skip the small queue.
		c.ghost.remove(e)
		e.size, e.pinned = size, pin
		c.main.pushFront(e)
	default:
		e.q.bytes += size - e.size
		e.size = size
		e.pinned = e.pinned || pin
	}

	return c.Evict()
}

func (c *Cache[K]) evictOne() *entry[K] {
	if c.small.bytes*100 >= c.capacity*smallFraction {
		if e := c.evictSmall(); e != nil {
			return e
		}

		return c.evictMain()
	}

	if e := c.evictMain(); e != nil {
		return e
	}

	return c.evictSmall()
}

// evictSmall frees the oldest cold entry. Entries hit more than once move to
// main instead. Pinned entries stay in their queue and rotate to the front.
func (c *Cache[K]) evictSmall() *entry[K] {
	for range c.small.n {
		e := c.small.tail

		switch {
		case e.pinned:
			c.small.rotate(e)
		case e.freq > 1:
			c.small.remove(e)
			e.freq = 0
			c.main.pushFront(e)
		default:
			c.small.remove(e)
			c.addGhost(e)

			return e
		}
	}

	return nil
}

func (c *Cache[K]) evictMain() *entry[K] {
	for range c.main.n * (maxFreq + 1) {
		e := c.main.tail

		switch {
		case e.pinned:
			c.main.rotate(e)
		case e.freq > 0:
			e.freq--
			c.main.rotate(e)
		default:
			c.main.remove(e)
			delete(c.entries, e.key)

			return e
		}
	}

	return nil
}

// addGhost remembers an evicted key. It reuses the entry, which is detached.
func (c *Cache[K]) addGhost(e *entry[K]) {
	e.freq, e.pinned = 0, false
	c.ghost.pushFront(e)

	limit := c.capacity - c.capacity*smallFraction/100
	for c.ghost.bytes > limit {
		old := c.ghost.tail
		c.ghost.remove(old)
		delete(c.entries, old.key)
	}
}
