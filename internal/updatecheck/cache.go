package updatecheck

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// CacheFile is the cache's name at the root of GHOSTBRANCH_HOME.
const CacheFile = "update-check.json"

// cacheFormat versions the file itself. A cache is not a record: a newer
// format is ignored and rewritten, never UPGRADE_NEEDED.
const cacheFormat = 1

// Cache is update-check.json. Concurrent commands may both write it; the
// last write wins, which is harmless.
type Cache struct {
	Format           int            `json:"format"`
	CheckedAt        time.Time      `json:"checked_at"`
	Latest           *CachedRelease `json:"latest"`
	Advisories       []Advisory     `json:"advisories"`
	RemindedPersonAt time.Time      `json:"reminded_person_at"`
	RemindedAgentAt  time.Time      `json:"reminded_agent_at"`
	// BinaryVersion is the version the advisories were matched against.
	BinaryVersion string `json:"binary_version"`
	// LastError is why the last look failed ("" when it didn't).
	LastError string `json:"last_error"`
}

// CachedRelease is the newest release seen, without the v.
type CachedRelease struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// Load reads the cache; anything missing, damaged or of another format reads
// as an empty cache.
func Load(path string) Cache {
	b, err := os.ReadFile(path)
	if err != nil {
		return Cache{}
	}
	var c Cache
	if json.Unmarshal(b, &c) != nil || c.Format != cacheFormat {
		return Cache{}
	}
	return c
}

// Save writes c atomically (a temp file, then a rename), mode 0600 on macOS
// and Linux, like plans.
func Save(path string, c Cache) error {
	c.Format = cacheFormat
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "update-check.*.tmp") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Merge records a look made by binary. A failed look keeps what was known
// and records why, so a broken network is asked again only at the next
// interval (R7). Advisories matched for another binary are dropped.
func (c *Cache) Merge(r Result, binary Version) {
	if c.BinaryVersion != binary.String() {
		c.Advisories = nil
	}
	c.CheckedAt, c.BinaryVersion = r.At, binary.String()
	if r.Latest != nil {
		c.Latest = &CachedRelease{Version: r.Latest.Version.String(), URL: r.Latest.URL}
	}
	switch {
	case r.Err == nil:
		c.LastError, c.Advisories = "", append([]Advisory{}, r.Advisories...)
	case errors.Is(r.Err, ErrNoRelease):
		c.LastError, c.Latest, c.Advisories = "", nil, nil
	default:
		c.LastError = clean(r.Err.Error())
	}
}

// Reminded records that aud was just reminded.
func (c *Cache) Reminded(aud Audience, now time.Time) {
	if aud == Agent {
		c.RemindedAgentAt = now
		return
	}
	c.RemindedPersonAt = now
}
