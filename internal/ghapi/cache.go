package ghapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Cache struct {
	dir string
}

type cacheEntry struct {
	ETag       string     `json:"etag"`
	StatusCode int        `json:"status_code"`
	Header     httpHeader `json:"header"`
	Body       []byte     `json:"body"`
	FetchedAt  time.Time  `json:"fetched_at"`
}

type httpHeader map[string][]string

func NewCache(dir string) *Cache {
	if dir == "" {
		return nil
	}
	return &Cache{dir: dir}
}

func (c *Cache) Get(key string) (*cacheEntry, error) {
	if c == nil {
		return nil, errors.New("cache is nil")
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, err
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, err
	}
	if entry.ETag == "" || len(entry.Body) == 0 {
		return nil, errors.New("invalid cache entry")
	}
	return &entry, nil
}

func (c *Cache) Put(key string, entry cacheEntry) error {
	if c == nil || entry.ETag == "" {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tmp := c.path(key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path(key))
}

func (c *Cache) Delete(key string) error {
	if c == nil {
		return nil
	}
	err := os.Remove(c.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (c *Cache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".json")
}
