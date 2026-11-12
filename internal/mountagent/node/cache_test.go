package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// store is an object store that, like S3, answers a GET only when the URL's
// signature grants that object, and honours a one-byte Range.
type store struct {
	*httptest.Server
	mu        sync.Mutex
	objects   map[string]string // path -> content; the ETag follows the content
	downloads atomic.Int32      // full GETs served
	requests  atomic.Int32
	delay     time.Duration
}

func newStore(t *testing.T) *store {
	t.Helper()
	s := &store{objects: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.mu.Lock()
		body, ok := s.objects[r.URL.Path]
		s.mu.Unlock()
		if !ok || r.URL.Query().Get("sig") != "ok:"+r.URL.Path {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, len(body)*31+int(body[0])))
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, body[:1])
			return
		}
		s.downloads.Add(1)
		time.Sleep(s.delay)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *store) put(path, body string) {
	s.mu.Lock()
	s.objects[path] = body
	s.mu.Unlock()
}

// signed is a URL the store accepts for path; anything else is a URL it refuses.
func (s *store) signed(path string) string { return s.URL + path + "?sig=ok:" + path }

func newTestCache(t *testing.T, s *store, budget, maxObject int64) *Cache {
	t.Helper()
	c, err := NewCache(t.TempDir(), []string{s.URL}, budget, maxObject)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func read(t *testing.T, c *Cache, rawURL string) (string, bool) {
	t.Helper()
	f, hit, err := c.Open(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("open %s: %v", rawURL, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), hit
}

func TestCacheDownloadsOncePerNode(t *testing.T) {
	s := newStore(t)
	s.put("/builds/a.erofs", "image a")
	c := newTestCache(t, s, 1<<20, 1<<20)

	// Two URLs signed at different times name one entry.
	if got, hit := read(t, c, s.signed("/builds/a.erofs")); got != "image a" || hit {
		t.Fatalf("first open = %q, hit %v; want the image from a miss", got, hit)
	}
	if got, hit := read(t, c, s.signed("/builds/a.erofs")+"&X-Amz-Date=later"); got != "image a" || !hit {
		t.Fatalf("second open = %q, hit %v; want the image from a hit", got, hit)
	}
	if n := s.downloads.Load(); n != 1 {
		t.Fatalf("downloaded %d times, want 1", n)
	}

	// An agent restart keeps what was cached.
	again, err := NewCache(c.dir, []string{s.URL}, 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, hit := read(t, again, s.signed("/builds/a.erofs")); !hit {
		t.Fatal("a restarted agent lost its cache")
	}
}

// A pod that cannot read an object gets the store's refusal, whether or not
// another pod cached it, so it learns nothing about the cache.
func TestCacheServesOnlyWhatTheStoreGrants(t *testing.T) {
	s := newStore(t)
	s.put("/builds/a.erofs", "image a")
	c := newTestCache(t, s, 1<<20, 1<<20)
	read(t, c, s.signed("/builds/a.erofs"))

	for _, bad := range []string{
		s.URL + "/builds/a.erofs",                        // unsigned
		s.URL + "/builds/a.erofs?sig=forged",             // forged or expired
		s.URL + "/builds/a.erofs?sig=ok:/builds/b.erofs", // signed for another object
		s.URL + "/builds/b.erofs?sig=ok:/builds/b.erofs", // not there at all
	} {
		f, _, err := c.Open(context.Background(), bad)
		if err == nil {
			f.Close()
			t.Errorf("served %s", bad)
			continue
		}
		if !strings.Contains(err.Error(), "answered 403") {
			t.Errorf("%s: %v, want the store's 403", bad, err)
		}
		if strings.Contains(err.Error(), "sig=") {
			t.Errorf("error carries the URL's signature: %v", err)
		}
	}
}

func TestCacheMissesWhenTheObjectChanges(t *testing.T) {
	s := newStore(t)
	s.put("/builds/a.erofs", "image a")
	c := newTestCache(t, s, 1<<20, 1<<20)
	read(t, c, s.signed("/builds/a.erofs"))

	s.put("/builds/a.erofs", "image a, rebuilt")
	if got, hit := read(t, c, s.signed("/builds/a.erofs")); got != "image a, rebuilt" || hit {
		t.Fatalf("after a change: %q, hit %v; want the new image from a miss", got, hit)
	}
}

// The agent is on the host network: it fetches from the configured endpoint
// and nowhere else, whatever a pod names or the store answers.
func TestCacheFetchesOnlyFromItsEndpoints(t *testing.T) {
	s := newStore(t)
	s.put("/builds/a.erofs", "image a")
	other := newStore(t)
	other.put("/builds/a.erofs", "image a")
	c := newTestCache(t, s, 1<<20, 1<<20)

	host := strings.TrimPrefix(s.URL, "http://")
	for _, bad := range []string{
		other.signed("/builds/a.erofs"),                                     // another host
		"https://" + host + "/builds/a.erofs?sig=ok:/builds/a.erofs",        // another scheme
		"http://user:pw@" + host + "/builds/a.erofs?sig=ok:/builds/a.erofs", // credentials of its own
		"http:" + host + "/builds/a.erofs",                                  // opaque
	} {
		if _, _, err := c.Open(context.Background(), bad); !errors.Is(err, ErrNotCached) {
			t.Errorf("%s: %v, want a refusal", bad, err)
		}
	}
	if n := other.requests.Load() + s.requests.Load(); n != 0 {
		t.Fatalf("refused URLs reached a server %d times", n)
	}

	// A redirect is not followed, even from the configured store.
	redirect := httptest.NewServer(http.RedirectHandler(other.signed("/builds/a.erofs"), http.StatusFound))
	defer redirect.Close()
	c = newTestCache(t, s, 1<<20, 1<<20)
	c.endpoints[0].Host = strings.TrimPrefix(redirect.URL, "http://")
	if _, _, err := c.Open(context.Background(), redirect.URL+"/builds/a.erofs"); err == nil {
		t.Fatal("followed a redirect")
	}
	if n := other.requests.Load(); n != 0 {
		t.Fatalf("the redirect target was fetched %d times", n)
	}
}

func TestCacheSharesConcurrentMisses(t *testing.T) {
	s := newStore(t)
	s.put("/builds/a.erofs", "image a")
	s.delay = 200 * time.Millisecond
	c := newTestCache(t, s, 1<<20, 1<<20)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if got, _ := read(t, c, s.signed("/builds/a.erofs")); got != "image a" {
				t.Errorf("got %q", got)
			}
		})
	}
	wg.Wait()
	if n := s.downloads.Load(); n != 1 {
		t.Fatalf("10 concurrent misses downloaded %d times, want 1", n)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	s := newStore(t)
	for _, name := range []string{"a", "b", "c"} {
		s.put("/builds/"+name, strings.Repeat(name, 100))
	}
	c := newTestCache(t, s, 250, 1<<20) // room for two

	read(t, c, s.signed("/builds/a"))
	time.Sleep(10 * time.Millisecond)
	held, _, err := c.Open(context.Background(), s.signed("/builds/b")) // a pod mounts b
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	time.Sleep(10 * time.Millisecond)
	if _, hit := read(t, c, s.signed("/builds/a")); !hit { // a is now the more recent
		t.Fatal("a was not cached")
	}
	time.Sleep(10 * time.Millisecond)
	read(t, c, s.signed("/builds/c"))
	if _, hit := read(t, c, s.signed("/builds/c")); !hit {
		t.Fatal("c was evicted instead of the least recently used entry")
	}
	if _, hit := read(t, c, s.signed("/builds/b")); hit {
		t.Fatal("b, the least recently used, was kept")
	}
	// Eviction unlinked b, but a holder still reads it.
	if got, err := io.ReadAll(held); err != nil || string(got) != strings.Repeat("b", 100) {
		t.Fatalf("held file after eviction = %q, %v", got, err)
	}
}

func TestCacheSkipsLargeObjects(t *testing.T) {
	s := newStore(t)
	s.put("/builds/big", strings.Repeat("x", 2000))
	c := newTestCache(t, s, 1<<20, 1000)
	if _, _, err := c.Open(context.Background(), s.signed("/builds/big")); !errors.Is(err, ErrNotCached) {
		t.Fatalf("large object: %v, want a refusal", err)
	}
	if n := s.downloads.Load(); n != 0 {
		t.Fatalf("downloaded a large object %d times", n)
	}
}
