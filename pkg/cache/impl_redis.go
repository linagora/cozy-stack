package cache

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis implementation of the cache client.
type Redis struct {
	client redis.UniversalClient
}

// NewRedis instantiate a new Redis Cache Client.
func NewRedis(client redis.UniversalClient) *Redis {
	return &Redis{client}
}

// CheckStatus checks that the cache is ready, or returns an error.
func (c *Redis) CheckStatus(ctx context.Context) (time.Duration, error) {
	before := time.Now()
	if err := c.client.Ping(ctx).Err(); err != nil {
		return 0, err
	}
	return time.Since(before), nil
}

// Get fetch the cached asset at the given key, and returns true only if the
// asset was found.
func (c *Redis) Get(key string) ([]byte, bool) {
	cmd := c.client.Get(context.TODO(), key)
	b, err := cmd.Bytes()
	if err != nil {
		return nil, false
	}

	return b, true
}

// MultiGet can be used to fetch several keys at once.
func (c *Redis) MultiGet(keys []string) [][]byte {
	results := make([][]byte, len(keys))

	cmd := c.client.MGet(context.TODO(), keys...)

	for i, val := range cmd.Val() {
		if buf, ok := val.(string); ok {
			results[i] = []byte(buf)
		}
	}

	return results
}

// Keys returns the list of keys with the given prefix.
//
// Note: it can be slow and should be used carefully.
func (c *Redis) Keys(prefix string) []string {
	cmd := c.client.Keys(context.TODO(), prefix+"*")

	return cmd.Val()
}

// Clear removes a key from the cache
func (c *Redis) Clear(key string) {
	c.client.Del(context.TODO(), key)
}

// Set stores an asset to the given key.
func (c *Redis) Set(key string, data []byte, expiration time.Duration) {
	c.client.Set(context.TODO(), key, data, expiration)
}

// SetNX stores the data in the cache only if the key doesn't exist yet.
func (c *Redis) SetNX(key string, data []byte, expiration time.Duration) {
	c.client.SetNX(context.TODO(), key, data, expiration)
}

// GetCompressed works like Get but expect a compressed asset that is
// uncompressed.
func (c *Redis) GetCompressed(key string) (io.Reader, bool) {
	r, ok := c.Get(key)
	if !ok {
		return nil, false
	}

	gr, err := gzip.NewReader(bytes.NewReader(r))
	if err != nil {
		return nil, false
	}

	return gr, true
}

// SetCompressed works like Set but compress the asset data before storing it.
func (c *Redis) SetCompressed(key string, data []byte, expiration time.Duration) {
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
func (c *Redis) RefreshTTL(key string, expiration time.Duration) {
	c.client.Expire(context.TODO(), key, expiration)
}
