package s3fifo_test

import (
	"strconv"
	"testing"

	"github.com/Mic92/seaweed-evictor/s3fifo"
)

// keys are pre-built so the benchmark measures the cache, not strconv.
func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "/buckets/cache/objects/" + strconv.Itoa(i) + ".bin"
	}

	return keys
}

// BenchmarkInsertEvict is the steady state of a full cache: every insert
// evicts about one entry.
func BenchmarkInsertEvict(b *testing.B) {
	const objects = 1 << 20

	keys := benchKeys(objects)
	c := s3fifo.New[string](objects / 4 * 1000)

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		c.Insert(keys[i%objects], 1000)
	}
}

// BenchmarkAccess is a read hit as reported by the audit log.
func BenchmarkAccess(b *testing.B) {
	const objects = 1 << 16

	keys := benchKeys(objects)
	c := s3fifo.New[string](objects * 1000)

	for _, k := range keys {
		c.Insert(k, 1000)
	}

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		c.Access(keys[i%objects])
	}
}

// BenchmarkMemory reports the heap cost per tracked object at one million
// entries.
func BenchmarkMemory(b *testing.B) {
	const objects = 1 << 20

	keys := benchKeys(objects)

	b.ReportAllocs()

	for b.Loop() {
		c := s3fifo.New[string](objects * 1000)
		for _, k := range keys {
			c.Insert(k, 1000)
		}
	}
}
