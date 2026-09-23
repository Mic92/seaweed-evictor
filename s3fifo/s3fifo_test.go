package s3fifo_test

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/Mic92/seaweed-evictor/s3fifo"
)

// prng is a splitmix64 generator; tests need reproducible input, not security.
type prng uint64

func (p *prng) next() uint64 {
	*p += 0x9e3779b97f4a7c15
	z := uint64(*p)
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb

	return z ^ z>>31
}

// intn returns a value in [0, n) for n up to 2^31.
func (p *prng) intn(n int) int { return int(p.next()>>33) % n }

// float returns a uniform value in (0, 1).
func (p *prng) float() float64 { return (float64(p.next()>>11) + 0.5) / (1 << 53) }

func TestOneHitWondersDoNotEvictHotObjects(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](100)

	// Hot object: inserted and read twice, so it is promoted to main.
	c.Insert("hot", 10)
	c.Access("hot")
	c.Access("hot")

	// A scan of objects that are never read again must not push it out.
	for i := range 50 {
		c.Insert(fmt.Sprintf("scan-%d", i), 10)
	}

	if !c.Contains("hot") {
		t.Fatal("hot object was evicted by a scan")
	}
	if c.Size() > 100 {
		t.Fatalf("size %d exceeds capacity", c.Size())
	}
}

func TestEvictionReturnsVictims(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](30)
	c.Insert("a", 10)
	c.Insert("b", 10)
	c.Insert("c", 10)

	got := c.Insert("d", 10)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("victims = %v, want [a]", got)
	}
	if c.Contains("a") {
		t.Fatal("a still tracked after eviction")
	}
}

func TestGhostHitGoesToMain(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](100)
	c.Insert("x", 10)
	// Push x out through the small queue; it stays within ghost capacity.
	for i := range 12 {
		c.Insert(fmt.Sprintf("f-%d", i), 10)
	}
	if c.Contains("x") {
		t.Fatal("x should have been evicted")
	}

	// Re-inserting a recently evicted key skips the small queue, so a
	// following scan cannot evict it.
	c.Insert("x", 10)
	for i := range 20 {
		c.Insert(fmt.Sprintf("g-%d", i), 10)
	}
	if !c.Contains("x") {
		t.Fatal("ghost-hit object was evicted by a scan")
	}
}

func TestPinnedEntriesAreNeverEvicted(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](30)
	c.Insert("dirty", 10)
	c.Pin("dirty")

	victims := make([]string, 0, 20)
	for i := range 20 {
		victims = append(victims, c.Insert(fmt.Sprintf("k-%d", i), 10)...)
	}
	for _, v := range victims {
		if v == "dirty" {
			t.Fatal("pinned entry was evicted")
		}
	}
	if !c.Contains("dirty") {
		t.Fatal("pinned entry missing")
	}

	c.Unpin("dirty")
	for i := range 20 {
		c.Insert(fmt.Sprintf("m-%d", i), 10)
	}
	if c.Contains("dirty") {
		t.Fatal("unpinned cold entry should eventually be evicted")
	}
}

func TestAllPinnedTerminates(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[int](20)
	for i := range 10 {
		c.Insert(i, 10)
		c.Pin(i)
	}
	// Over capacity but nothing evictable: must return, not spin.
	if got := c.Insert(100, 10); len(got) > 1 {
		t.Fatalf("unexpected victims %v", got)
	}
}

func TestRemoveAndReinsertUpdatesSize(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](100)
	c.Insert("a", 10)
	c.Insert("a", 30) // overwrite with a different size
	if c.Size() != 30 {
		t.Fatalf("size = %d, want 30", c.Size())
	}
	c.Remove("a")
	if c.Size() != 0 || c.Contains("a") {
		t.Fatal("remove did not clear entry")
	}
	c.Remove("missing") // no panic
}

func TestOversizedObject(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](10)
	got := c.Insert("big", 50)
	if len(got) != 1 || got[0] != "big" {
		t.Fatalf("victims = %v, want the oversized object itself", got)
	}
}

func TestInvariantsUnderRandomLoad(t *testing.T) {
	t.Parallel()

	const capacity = 1000
	c := s3fifo.New[int](capacity)
	rng := prng(1)
	live := map[int]int{}

	for range 20000 {
		k := int(-math.Log(rng.float()) * 50)
		switch rng.intn(4) {
		case 0:
			c.Access(k)
		case 1:
			c.Remove(k)
			delete(live, k)
		default:
			sz := 1 + rng.intn(40)
			for _, v := range c.Insert(k, sz) {
				delete(live, v)
			}
			if c.Contains(k) {
				live[k] = sz
			}
		}
		if c.Size() > capacity {
			t.Fatalf("size %d over capacity", c.Size())
		}
	}

	sum := 0
	for k, sz := range live {
		if !c.Contains(k) {
			t.Fatalf("live key %d not tracked", k)
		}
		sum += sz
	}
	if sum != c.Size() {
		t.Fatalf("tracked size %d != model %d", c.Size(), sum)
	}
}

func TestHitRatioBeatsFIFOOnZipf(t *testing.T) {
	t.Parallel()

	// Zipf(1.1) over 10000 keys by inverse CDF.
	const keys = 10000

	cdf := make([]float64, keys)
	sum := 0.0

	for i := range keys {
		sum += math.Pow(float64(i+1), -1.1)
		cdf[i] = sum
	}

	rng := prng(2)
	c := s3fifo.New[int](500)
	hits, total := 0, 100000

	for range total {
		k, _ := slices.BinarySearch(cdf, rng.float()*sum)
		if c.Contains(k) {
			c.Access(k)

			hits++
		} else {
			c.Insert(k, 1)
		}
	}

	if ratio := float64(hits) / float64(total); ratio < 0.5 {
		t.Fatalf("hit ratio %.2f too low for zipf workload", ratio)
	}
}

func TestEvictAfterUnpin(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](20)
	for _, k := range []string{"a", "b", "c"} {
		c.InsertPinned(k, 10)
	}

	if got := c.Evict(); len(got) != 0 {
		t.Fatalf("evicted pinned entries: %v", got)
	}

	c.Unpin("a")

	if got := c.Evict(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("victims = %v, want [a]", got)
	}
}

func TestInsertPinnedSurvivesItsOwnInsert(t *testing.T) {
	t.Parallel()

	c := s3fifo.New[string](20)
	c.Insert("a", 10)
	c.Insert("b", 10)

	got := c.InsertPinned("c", 10)
	if slices.Contains(got, "c") || !c.Contains("c") {
		t.Fatalf("pinned newcomer evicted: %v", got)
	}
}
