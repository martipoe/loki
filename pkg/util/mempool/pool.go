package mempool

import (
	"fmt"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	reasonSizeExceeded = "size-exceeded"
)

type slab struct {
	buffer      chan []byte
	size, count int
	once        sync.Once
	metrics     *metrics
	name        string
}

func newSlab(bufferSize, bufferCount int, m *metrics) *slab {
	name := humanize.Bytes(uint64(bufferSize))
	m.availableBuffersPerSlab.WithLabelValues(name).Set(0) // initialize metric with value 0

	return &slab{
		size:    bufferSize,
		count:   bufferCount,
		metrics: m,
		name:    name,
	}
}

func (s *slab) init() {
	s.buffer = make(chan []byte, s.count)
	for i := 0; i < s.count; i++ {
		buf := make([]byte, 0, s.size)
		s.buffer <- buf
	}
	s.metrics.availableBuffersPerSlab.WithLabelValues(s.name).Set(float64(s.count))
}

func (s *slab) get(size int) ([]byte, error) {
	s.metrics.accesses.WithLabelValues(s.name, opTypeGet).Inc()
	s.once.Do(s.init)

	waitStart := time.Now()
	// wait for available buffer on channel
	buf := <-s.buffer
	s.metrics.waitDuration.WithLabelValues(s.name).Observe(time.Since(waitStart).Seconds())

	return buf[:size], nil
}

func (s *slab) put(buf []byte) {
	s.metrics.accesses.WithLabelValues(s.name, opTypePut).Inc()
	if s.buffer == nil {
		panic("slab is not initialized")
	}

	// Note that memory is NOT zero'd on return, but since all allocations are of defined widths and we only ever then read a record of exactly that width into the allocation, it will always be overwritten before use and can't leak.
	s.buffer <- buf
}

// MemPool is an Allocator implementation that uses a fixed size memory pool
// that is split into multiple slabs of different buffer sizes.
// Buffers are re-cycled and need to be returned back to the pool, otherwise
// the pool runs out of available buffers.
type MemPool struct {
	slabs   []*slab
	metrics *metrics
}

func New(name string, buckets []Bucket, r prometheus.Registerer) *MemPool {
	a := &MemPool{
		slabs:   make([]*slab, 0, len(buckets)),
		metrics: newMetrics(r, name),
	}
	for _, b := range buckets {
		a.slabs = append(a.slabs, newSlab(int(b.Capacity), b.Size, a.metrics))
	}
	return a
}

// Get satisfies Allocator interface
// Allocating a buffer from an exhausted pool/slab, or allocating a buffer that
// exceeds the largest slab size will return an error.
func (a *MemPool) Get(size int) ([]byte, error) {
	for i := 0; i < len(a.slabs); i++ {
		if a.slabs[i].size < size {
			continue
		}
		return a.slabs[i].get(size)
	}
	a.metrics.errorsCounter.WithLabelValues("pool", reasonSizeExceeded).Inc()
	return nil, fmt.Errorf("no slab found for size: %d", size)
}

// Put satisfies Allocator interface
// Every buffer allocated with Get(size int) needs to be returned to the pool
// using Put(buffer []byte) so it can be re-cycled.
func (a *MemPool) Put(buffer []byte) bool {
	size := cap(buffer)
	for i := 0; i < len(a.slabs); i++ {
		if a.slabs[i].size < size {
			continue
		}
		a.slabs[i].put(buffer)
		return true
	}
	return false
}
