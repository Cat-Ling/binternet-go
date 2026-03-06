package cache

import (
	"fmt"
	"log"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/ristretto"
)

type Cache interface {
	Get(key string) ([]byte, bool)
	Set(key string, value []byte, ttl time.Duration)
	Close() error
}

// Default disk TTL for entries stored with ttl=0 (e.g., images)
const defaultDiskTTL = 6 * time.Hour

type LayeredCache struct {
	memoryCache *ristretto.Cache
	diskCache   *badger.DB
	useMemory   bool
	useDisk     bool
	diskLimitMB int64
}

func NewLayeredCache(useMemory bool, memoryLimit int64, useDisk bool, diskLimit int64) (*LayeredCache, error) {
	lc := &LayeredCache{
		useMemory:   useMemory,
		useDisk:     useDisk,
		diskLimitMB: diskLimit,
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
		opts := badger.DefaultOptions("cache/badger").
			WithLogger(nil). // Silence badger's internal logging
			WithNumVersionsToKeep(1).
			WithValueLogFileSize(64 << 20). // 64MB value log files
			WithNumMemtables(2).
			WithNumLevelZeroTables(2).
			WithNumLevelZeroTablesStall(4).
			WithBlockCacheSize(32 << 20) // 32MB block cache for reads

		db, err := badger.Open(opts)
		if err != nil {
			return nil, fmt.Errorf("failed to open disk cache (badger): %w", err)
		}
		lc.diskCache = db

		// Background GC goroutine — runs every 5 minutes to reclaim space from expired entries
		go lc.runGC()
	}

	return lc, nil
}

// runGC periodically runs BadgerDB's value log garbage collection
func (c *LayeredCache) runGC() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if c.diskCache == nil {
			return
		}
		// Run GC until there's nothing left to collect (< 50% reclaimable)
		for {
			err := c.diskCache.RunValueLogGC(0.5)
			if err != nil {
				break // No more GC needed
			}
		}

		// Check if we're over the disk limit and force-purge if needed
		c.enforceDiskLimit()
	}
}

// enforceDiskLimit checks current DB size and drops oldest expired entries
func (c *LayeredCache) enforceDiskLimit() {
	if c.diskCache == nil || c.diskLimitMB <= 0 {
		return
	}

	lsm, vlog := c.diskCache.Size()
	totalBytes := lsm + vlog
	limitBytes := c.diskLimitMB * 1024 * 1024

	if totalBytes > limitBytes {
		log.Printf("Disk cache over limit: %dMB / %dMB — running aggressive GC",
			totalBytes/(1024*1024), c.diskLimitMB)

		// Run aggressive GC (lower threshold = more aggressive)
		for i := 0; i < 10; i++ {
			if err := c.diskCache.RunValueLogGC(0.1); err != nil {
				break
			}
		}

		// Flatten the LSM tree to reclaim space
		c.diskCache.Flatten(4)
	}
}

func (c *LayeredCache) Get(key string) ([]byte, bool) {
	if c.useMemory {
		val, found := c.memoryCache.Get(key)
		if found {
			return val.([]byte), true
		}
	}

	if c.useDisk && c.diskCache != nil {
		var valCopy []byte
		err := c.diskCache.View(func(txn *badger.Txn) error {
			item, err := txn.Get([]byte(key))
			if err != nil {
				return err
			}
			valCopy, err = item.ValueCopy(nil)
			return err
		})
		if err == nil && valCopy != nil {
			// Promote to memory cache on disk hit
			if c.useMemory {
				c.memoryCache.Set(key, valCopy, int64(len(valCopy)))
			}
			return valCopy, true
		}
	}

	return nil, false
}

func (c *LayeredCache) Set(key string, value []byte, ttl time.Duration) {
	if c.useMemory {
		c.memoryCache.SetWithTTL(key, value, int64(len(value)), ttl)
	}

	if c.useDisk && c.diskCache != nil {
		// Async disk write
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)

		diskTTL := ttl
		if diskTTL == 0 {
			diskTTL = defaultDiskTTL
		}

		go func() {
			// Check size before writing — skip if way over limit
			if c.diskLimitMB > 0 {
				lsm, vlog := c.diskCache.Size()
				limitBytes := c.diskLimitMB * 1024 * 1024
				if lsm+vlog > limitBytes {
					return // Over limit, skip write — GC will reclaim space
				}
			}

			c.diskCache.Update(func(txn *badger.Txn) error {
				e := badger.NewEntry([]byte(key), valueCopy).WithTTL(diskTTL)
				return txn.SetEntry(e)
			})
		}()
	}
}

func (c *LayeredCache) Close() error {
	if c.diskCache != nil {
		return c.diskCache.Close()
	}
	return nil
}

// NoOpCache implementation for when caching is disabled
type NoOpCache struct{}

func (n *NoOpCache) Get(key string) ([]byte, bool)                   { return nil, false }
func (n *NoOpCache) Set(key string, value []byte, ttl time.Duration) {}
func (n *NoOpCache) Close() error                                    { return nil }
