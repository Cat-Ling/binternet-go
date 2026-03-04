package cache

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/peterbourgon/diskv"
)

type Cache interface {
	Get(key string) ([]byte, bool)
	Set(key string, value []byte, ttl time.Duration)
}

type LayeredCache struct {
	memoryCache *ristretto.Cache
	diskCache   *diskv.Diskv
	useMemory   bool
	useDisk     bool
}

func NewLayeredCache(useMemory bool, memoryLimit int64, useDisk bool, diskLimit int64) (*LayeredCache, error) {
	lc := &LayeredCache{
		useMemory: useMemory,
		useDisk:   useDisk,
	}

	if useMemory {
		config := &ristretto.Config{
			NumCounters: 1e7,                       // number of keys to track frequency of (10M).
			MaxCost:     memoryLimit * 1024 * 1024, // maximum cost of cache (MB).
			BufferItems: 64,                        // number of keys per Get buffer.
		}
		cache, err := ristretto.NewCache(config)
		if err != nil {
			return nil, fmt.Errorf("failed to create memory cache: %w", err)
		}
		lc.memoryCache = cache
	}

	if useDisk {
		// key is already an MD5 hash (hex string) passed from Set/Get
		// User requested at least level 5 caching to handle massive amounts of files.
		// MD5 is 32 chars hex. We take 5 pairs (10 chars) for 5 levels.
		advancedTransform := func(s string) []string {
			if len(s) < 10 {
				return []string{}
			}
			return []string{
				s[0:2],
				s[2:4],
				s[4:6],
				s[6:8],
				s[8:10],
			}
		}

		lc.diskCache = diskv.New(diskv.Options{
			BasePath:     "cache",
			Transform:    advancedTransform,
			CacheSizeMax: uint64(diskLimit * 1024 * 1024), // MB
		})
	}

	return lc, nil
}

func (c *LayeredCache) getDiskKey(key string) string {
	hash := md5.Sum([]byte(key))
	return hex.EncodeToString(hash[:])
}

func (c *LayeredCache) Get(key string) ([]byte, bool) {
	if c.useMemory {
		val, found := c.memoryCache.Get(key)
		if found {
			return val.([]byte), true
		}
	}

	if c.useDisk {
		// Disk keys must be filesystem safe
		diskKey := c.getDiskKey(key)
		val, err := c.diskCache.Read(diskKey)
		if err == nil {
			// Populate memory cache if found in disk
			if c.useMemory {
				c.memoryCache.Set(key, val, int64(len(val)))
			}
			return val, true
		}
	}

	return nil, false
}

func (c *LayeredCache) Set(key string, value []byte, ttl time.Duration) {
	if c.useMemory {
		c.memoryCache.SetWithTTL(key, value, int64(len(value)), ttl)
	}

	if c.useDisk {
		diskKey := c.getDiskKey(key)
		c.diskCache.Write(diskKey, value)
	}
}

// NoOpCache implementation for when caching is disabled
type NoOpCache struct{}

func (n *NoOpCache) Get(key string) ([]byte, bool)                   { return nil, false }
func (n *NoOpCache) Set(key string, value []byte, ttl time.Duration) {}
