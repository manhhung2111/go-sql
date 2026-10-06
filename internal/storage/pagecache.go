package storage

import (
	"container/list"
	"fmt"
	"sync"
)

// cachedPageFile is a write-through LRU cache in front of a PageFile.
//
// Cached buffers are immutable: a write installs a new buffer rather than
// changing the cached one, and nothing recycles a buffer, so a reader that
// already holds one keeps seeing the bytes it read even after the page is
// replaced or evicted. The mutex covers only the map and the list.
type cachedPageFile struct {
	inner    PageFile
	capacity int

	mu     sync.Mutex
	frames map[int32]*list.Element
	lru    *list.List // front is the most recently used
}

type frame struct {
	page int32
	buf  []byte
}

// NewCachedPageFile wraps inner with a write-through LRU cache of capacity
// pages. Like the file it wraps, reads may run concurrently with one another
// and writes need exclusive access from the owner.
func NewCachedPageFile(inner PageFile, capacity int) (PageFile, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("page cache capacity must be at least 1, got %d", capacity)
	}
	return &cachedPageFile{
		inner:    inner,
		capacity: capacity,
		frames:   make(map[int32]*list.Element),
		lru:      list.New(),
	}, nil
}

func (c *cachedPageFile) ReadPage(n int32) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.frames[n]; ok {
		c.lru.MoveToFront(el)
		buf := el.Value.(*frame).buf
		c.mu.Unlock()
		return buf, nil
	}
	c.mu.Unlock()

	// The read happens outside the lock so a slow read does not block hits.
	buf, err := c.inner.ReadPage(n)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.frames[n]; ok { // another reader installed it first
		c.lru.MoveToFront(el)
		return el.Value.(*frame).buf, nil
	}
	c.install(n, buf)
	return buf, nil
}

func (c *cachedPageFile) WritePage(n int32, buf []byte) error {
	if len(buf) != maxPageSize {
		return fmt.Errorf("page is %d bytes, want %d", len(buf), maxPageSize)
	}

	// Stamp a private copy: it is what lands in the cache, so the caller's
	// buffer is never modified and never aliased by a cached frame.
	stamped := append([]byte(nil), buf...)
	stampPage(stamped, n)

	// Disk first. On failure the cache must not keep anything the disk lacks,
	// so the page's entry is dropped and the next read reloads what is there.
	if err := c.inner.WritePage(n, stamped); err != nil {
		c.mu.Lock()
		c.drop(n)
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	c.install(n, stamped)
	c.mu.Unlock()
	return nil
}

// AllocatePage appends a blank page. Nothing is cached until it is first read
// or written.
func (c *cachedPageFile) AllocatePage() (int32, error) { return c.inner.AllocatePage() }

func (c *cachedPageFile) NumPages() int32 { return c.inner.NumPages() }

func (c *cachedPageFile) Sync() error { return c.inner.Sync() }

func (c *cachedPageFile) Close() error {
	c.mu.Lock()
	c.frames = make(map[int32]*list.Element)
	c.lru.Init()
	c.mu.Unlock()
	return c.inner.Close()
}

// install makes buf page n's frame and the most recently used, replacing any
// existing frame with a new one (the old frame's buffer is left untouched), then
// evicts from the back while over capacity. Callers hold c.mu.
func (c *cachedPageFile) install(n int32, buf []byte) {
	if el, ok := c.frames[n]; ok {
		el.Value = &frame{page: n, buf: buf}
		c.lru.MoveToFront(el)
		return
	}
	c.frames[n] = c.lru.PushFront(&frame{page: n, buf: buf})
	for c.lru.Len() > c.capacity {
		oldest := c.lru.Back()
		delete(c.frames, oldest.Value.(*frame).page)
		c.lru.Remove(oldest)
	}
}

// drop forgets page n. Callers hold c.mu.
func (c *cachedPageFile) drop(n int32) {
	if el, ok := c.frames[n]; ok {
		c.lru.Remove(el)
		delete(c.frames, n)
	}
}
