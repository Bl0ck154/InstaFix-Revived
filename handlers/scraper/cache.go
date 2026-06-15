package handlers

import (
	"os"

	"github.com/cespare/xxhash/v2"
	"github.com/elastic/go-freelru"
	bolt "go.etcd.io/bbolt"
)

var DB *bolt.DB
var LRU *freelru.SyncedLRU[string, bool]

// HasCachedData reports whether a post already has cached metadata. It is used
// by the lightweight request protection layer to avoid treating cheap cached
// requests the same as expensive cache misses.
func HasCachedData(postID string) bool {
	if DB == nil || postID == "" {
		return false
	}
	found := false
	_ = DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		if b == nil {
			return nil
		}
		found = b.Get([]byte(postID)) != nil
		return nil
	})
	return found
}

func hashStringXXHASH(s string) uint32 {
	return uint32(xxhash.Sum64String(s))
}

func InitDB() error {
	db, err := bolt.Open("cache.db", 0600, nil)
	if err != nil {
		return err
	}

	// Create buckets
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte("data")); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists([]byte("ttl"))
		return err
	})
	if err != nil {
		db.Close()
		return err
	}

	DB = db
	return nil
}

func InitLRU(maxEntries int) {
	// Initialize LRU for grid caching
	lru, err := freelru.NewSynced[string, bool](uint32(maxEntries), hashStringXXHASH)
	if err != nil {
		panic(err)
	}

	lru.SetOnEvict(func(key string, value bool) {
		os.Remove(key)
	})

	// Fill LRU with existing files
	dir, err := os.ReadDir("static")
	if err != nil {
		panic(err)
	}
	for _, d := range dir {
		if !d.IsDir() {
			lru.Add("static/"+d.Name(), true)
		}
	}

	LRU = lru
}
