package cache

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// InMemory implementation of the Cache client.
type InMemory struct {
	m *sync.Map
}

// NewInMemory instantiates a new in-memory Cache Client.
func NewInMemory() *InMemory {
	return &InMemory{m: new(sync.Map)}
}

// CheckStatus checks that the cache is ready, or returns an error.
func (c *InMemory) CheckStatus(ctx context.Context) (time.Duration, error) {
	return 0, nil
}

// Get fetch the cached asset at the given key, and returns true only if the
// asset was found.
func (c *InMemory) Get(key string) ([]byte, bool) {
	value, ok := c.m.Load(key)
	if !ok {
		return nil, false
	}

	entry := value.(cacheEntry)
	if time.Now().After(entry.expiredAt) {
		// The value is expired. Clean it and return not found
		c.Clear(key)
		return nil, false
	}

	return entry.payload, true
}

// MultiGet can be used to fetch several keys at once.
func (c *InMemory) MultiGet(keys []string) [][]byte {
	results := make([][]byte, len(keys))

	for i, key := range keys {
		results[i], _ = c.Get(key)
	}

	return results
}

// Keys returns the list of keys with the given prefix.
func (c *InMemory) Keys(prefix string) []string {
	results := make([]string, 0)

	c.m.Range(func(key, _ interface{}) bool {
		k := key.(string)
		if strings.HasPrefix(k, prefix) {
			results = append(results, k)
		}

		return true
	})

	return results
}

// Clear removes a key from the cache
func (c *InMemory) Clear(key string) {
	c.m.Delete(key)
}

// Set stores an asset to the given key.
func (c *InMemory) Set(key string, data []byte, expiration time.Duration) {
	c.m.Store(key, cacheEntry{
		payload:   data,
		expiredAt: time.Now().Add(expiration),
	})
}

// SetNX stores the data in the cache only if the key doesn't exist yet.
func (c *InMemory) SetNX(key string, data []byte, expiration time.Duration) {
	c.m.LoadOrStore(key, cacheEntry{
		payload:   data,
		expiredAt: time.Now().Add(expiration),
	})
}

// GetCompressed works like Get but expect a compressed asset that is
// uncompressed.
func (c *InMemory) GetCompressed(key string) (io.Reader, bool) {
	if r, ok := c.Get(key); ok {
		if gr, err := gzip.NewReader(bytes.NewReader(r)); err == nil {
			return gr, true
		}
	}
	return nil, false
}

// SetCompressed works like Set but compress the asset data before storing it.
func (c *InMemory) SetCompressed(key string, data []byte, expiration time.Duration) {
	dataCompressed := new(bytes.Buffer)
	gw := gzip.NewWriter(dataCompressed)
	if _, err := gw.Write(data); err != nil {
		return
	}
	// Close writes the end of the stream and its trailer.
	if err := gw.Close(); err != nil {
		return
	}
	c.Set(key, dataCompressed.Bytes(), expiration)
}

// RefreshTTL can be used to update the TTL of an existing entry in the cache.
func (c *InMemory) RefreshTTL(key string, expiration time.Duration) {
	value, ok := c.m.Load(key)
	if !ok {
		return
	}

	entry := value.(cacheEntry)
	entry.expiredAt = time.Now().Add(expiration)
	c.m.Store(key, entry)
}
