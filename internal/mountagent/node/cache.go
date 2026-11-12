package node

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cache keeps images on the node's disk so that pods on one node download an
// image once (docs/design/node-artifact-cache.md). It is shared by every pod on
// the node, across tenants, so it trusts nothing a pod names:
//
//   - it fetches only from the object store endpoints it was configured with,
//     and follows no redirects, because it runs on the host network;
//   - it serves an entry, hit or miss, only after the object store has accepted
//     the pod's own pre-signed URL for that object, so a pod that cannot read an
//     object cannot read or even detect a cached copy of it;
//   - an entry is keyed by the object and its ETag, so a replaced object is a
//     miss.
type Cache struct {
	dir       string
	endpoints []*url.URL
	budget    int64 // bytes kept on disk; least recently used go first
	maxObject int64 // larger objects are not cached
	client    *http.Client

	mu       sync.Mutex
	inflight map[string]*download
}

type download struct {
	done chan struct{}
	err  error
}

// ErrNotCached is a refusal to cache an object. The caller downloads it itself.
var ErrNotCached = errors.New("not cached")

// downloadTimeout bounds one download. It is detached from the request that
// started it, since other pods may be waiting on the same one.
const downloadTimeout = 5 * time.Minute

// NewCache keeps up to budget bytes of images in dir, fetched from endpoints
// (scheme and host, e.g. https://fra1.digitaloceanspaces.com). Downloads left
// half-written by an earlier process are removed; finished entries are kept.
func NewCache(dir string, endpoints []string, budget, maxObject int64) (*Cache, error) {
	c := &Cache{
		dir: dir, budget: budget, maxObject: maxObject, inflight: map[string]*download{},
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	for _, e := range endpoints {
		u, err := url.Parse(strings.TrimSpace(e))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("cache endpoint %q must be http(s)://host[:port]", e)
		}
		c.endpoints = append(c.endpoints, u)
	}
	if len(c.endpoints) == 0 {
		return nil, errors.New("the cache needs at least one endpoint")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	partials, err := filepath.Glob(filepath.Join(dir, "*.partial"))
	if err != nil {
		return nil, err
	}
	for _, p := range partials {
		_ = os.Remove(p)
	}
	return c, nil
}

// Open returns the cached image for rawURL, a pre-signed GET, downloading it
// first on a miss. The file is open, so an eviction that unlinks it afterwards
// does not take it from the caller. Concurrent misses for one entry share one
// download.
func (c *Cache) Open(ctx context.Context, rawURL string) (img *os.File, hit bool, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || !c.fetchable(u) {
		return nil, false, fmt.Errorf("%w: not a configured object store endpoint", ErrNotCached)
	}
	etag, size, err := c.probe(ctx, u)
	if err != nil {
		return nil, false, err
	}
	if size > c.maxObject {
		return nil, false, fmt.Errorf("%w: %d bytes is over the %d byte limit", ErrNotCached, size, c.maxObject)
	}
	sum := sha256.Sum256([]byte(u.Scheme + "://" + u.Host + u.EscapedPath() + "\x00" + etag))
	path := filepath.Join(c.dir, hex.EncodeToString(sum[:]))

	if img, err := os.Open(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now) // recency for eviction
		return img, true, nil
	}
	if err := c.fill(u, etag, path); err != nil {
		return nil, false, err
	}
	img, err = os.Open(path)
	return img, false, err
}

// fetchable reports whether u is on a configured endpoint: the same scheme and
// exactly the same host and port, with no credentials of its own.
func (c *Cache) fetchable(u *url.URL) bool {
	return u.User == nil && u.Opaque == "" && slices.ContainsFunc(c.endpoints, func(e *url.URL) bool {
		return u.Scheme == e.Scheme && strings.EqualFold(u.Host, e.Host)
	})
}

// probe asks the object store for one byte of the object with the pod's URL.
// It is the authorization check (the store refuses a URL that does not grant a
// GET of this object) and returns the object's ETag and size.
func (c *Cache) probe(ctx context.Context, u *url.URL) (etag string, size int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	res, err := c.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("probe %s: %w", u.Path, unwrapURL(err))
	}
	res.Body.Close()
	etag = res.Header.Get("ETag")
	switch res.StatusCode {
	case http.StatusPartialContent:
		_, total, _ := strings.Cut(res.Header.Get("Content-Range"), "/")
		size, err = strconv.ParseInt(total, 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("probe %s: no object size in Content-Range %q", u.Path, res.Header.Get("Content-Range"))
		}
	case http.StatusOK:
		size = res.ContentLength
	default:
		return "", 0, fmt.Errorf("probe %s: object store answered %d", u.Path, res.StatusCode)
	}
	if etag == "" || size < 0 {
		return "", 0, fmt.Errorf("%w: object store sent no ETag or size", ErrNotCached)
	}
	return etag, size, nil
}

// fill downloads the object into path, once however many callers ask at once.
func (c *Cache) fill(u *url.URL, etag, path string) error {
	c.mu.Lock()
	d, waiting := c.inflight[path]
	if !waiting {
		d = &download{done: make(chan struct{})}
		c.inflight[path] = d
	}
	c.mu.Unlock()
	if waiting {
		<-d.done
		return d.err
	}

	d.err = c.download(u, etag, path)
	c.mu.Lock()
	delete(c.inflight, path)
	c.mu.Unlock()
	close(d.done)
	if d.err == nil {
		c.evict()
	}
	return d.err
}

func (c *Cache) download(u *url.URL, etag, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", u.Path, unwrapURL(err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: object store answered %d", u.Path, res.StatusCode)
	}
	// The object changed since the probe: the bytes are not what the key names.
	if res.Header.Get("ETag") != etag {
		return fmt.Errorf("%w: %s changed while it was fetched", ErrNotCached, u.Path)
	}

	tmp := path + "." + rand.Text() + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op after the rename
	n, err := io.Copy(f, io.LimitReader(res.Body, c.maxObject+1))
	if err == nil && n > c.maxObject {
		err = fmt.Errorf("%w: %s is over the %d byte limit", ErrNotCached, u.Path, c.maxObject)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", u.Path, unwrapURL(err))
	}
	slog.Info("Cached image", "object", u.Path, "bytes", n, "duration", time.Since(start))
	return nil
}

// evict removes the least recently used entries until the cache fits its
// budget. A file a pod still has loop-mounted keeps its inode until the mount
// goes, so eviction never breaks a running pod.
func (c *Cache) evict() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		slog.Warn("Cache eviction failed", "error", err)
		return
	}
	var files []fs.FileInfo
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		files = append(files, info)
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b fs.FileInfo) int { return cmp.Compare(a.ModTime().UnixNano(), b.ModTime().UnixNano()) })
	for _, f := range files {
		if total <= c.budget {
			return
		}
		if err := os.Remove(filepath.Join(c.dir, f.Name())); err == nil || errors.Is(err, os.ErrNotExist) {
			total -= f.Size()
		}
	}
}

// unwrapURL drops the URL net/http puts in its errors: it is pre-signed, and
// the query is a credential that must not reach the pod's logs or the agent's.
func unwrapURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}
