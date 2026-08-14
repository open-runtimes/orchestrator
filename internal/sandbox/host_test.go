package sandbox

import (
	"strconv"
	"strings"
	"testing"
)

// The strongest available check on a wire format: everything the writer emits,
// the reader reads back. No cluster, no daemon, no proxy.
func TestAddressing_RoundTrip(t *testing.T) {
	t.Parallel()
	addr := Addressing{Domain: "sandboxes.example.com", Scheme: "https"}

	for _, port := range []int{0, 3000, 5173, 65535} {
		host := addr.Host("tok3n", port)
		token, gotPort, ok := addr.Resolve(host)
		if !ok {
			t.Fatalf("port %d: %q did not resolve", port, host)
		}
		if token != "tok3n" {
			t.Errorf("port %d: token: got %q", port, token)
		}
		want := ""
		if port != 0 {
			want = strconv.Itoa(port)
		}
		if gotPort != want {
			t.Errorf("port %d: want %q, got %q", port, want, gotPort)
		}
	}
}

func TestAddressing_ResolveRejectsWhatIsNotASandbox(t *testing.T) {
	t.Parallel()
	addr := Addressing{Domain: "localhost"}

	tests := []struct {
		name string
		host string
	}{
		// The one that matters: on the shared Docker listener both data planes
		// hear the same domain, so a deployment host must not read as a token —
		// trimming the prefix instead of requiring it swallowed these.
		{"a deployment host under the same domain", "myapp.localhost"},
		{"another domain entirely", "s-abc.example.com"},
		{"no domain at all", "s-abc"},
		{"the prefix and nothing else", "s-.localhost"},
		{"an empty host", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if token, _, ok := addr.Resolve(tt.host); ok {
				t.Errorf("%q resolved to token %q", tt.host, token)
			}
		})
	}
}

func TestAddressing_ResolveNormalizes(t *testing.T) {
	t.Parallel()
	addr := Addressing{Domain: "sandboxes.example.com"}

	// A Host header carries the port the client dialled, and DNS is
	// case-insensitive; neither changes which sandbox is addressed.
	for _, host := range []string{
		"s-abc.sandboxes.example.com",
		"s-abc.sandboxes.example.com:8081",
		"s-abc.SANDBOXES.EXAMPLE.COM",
	} {
		token, _, ok := addr.Resolve(host)
		if !ok || token != "abc" {
			t.Errorf("%q: got token %q, ok %v", host, token, ok)
		}
	}
}

func TestAddressing_HyphensAreOnlyPortsWhenTheyAre(t *testing.T) {
	t.Parallel()
	addr := Addressing{Domain: "localhost"}

	// A trailing word is part of the token, not a port.
	token, port, ok := addr.Resolve("s-abc-dev.localhost")
	if !ok || token != "abc-dev" || port != "" {
		t.Errorf("got token %q port %q ok %v", token, port, ok)
	}
	// Neither is a number outside the port range.
	token, port, ok = addr.Resolve("s-abc-99999.localhost")
	if !ok || token != "abc-99999" || port != "" {
		t.Errorf("got token %q port %q ok %v", token, port, ok)
	}
}

func TestAddressing_NoTokenNoAddress(t *testing.T) {
	t.Parallel()
	addr := Addressing{Domain: "localhost", Scheme: "http"}

	// Nothing is serving, so there is nothing to hand back.
	if got := addr.URL(""); got != "" {
		t.Errorf("URL: got %q", got)
	}
	if got := addr.URLs("", 3000, []int{5173}); got != nil {
		t.Errorf("URLs: got %v", got)
	}
}

func TestAddressing_URLsCoverEveryPort(t *testing.T) {
	t.Parallel()
	addr := Addressing{Domain: "localhost", Scheme: "http"}

	urls := addr.URLs("abc", 3000, []int{5173, 9229})
	if len(urls) != 3 {
		t.Fatalf("want the primary plus both extras, got %v", urls)
	}
	if urls["3000"] != addr.URL("abc") {
		t.Errorf("primary: got %q", urls["3000"])
	}
	for _, port := range []int{5173, 9229} {
		key := strconv.Itoa(port)
		if urls[key] != addr.PortURL("abc", port) {
			t.Errorf("port %d: got %q", port, urls[key])
		}
	}
}

func TestIsToken(t *testing.T) {
	t.Parallel()
	minted, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}

	// What the writer mints, the shared-listener tie-break must accept.
	if !IsToken(minted) {
		t.Errorf("minted token %q not recognised", minted)
	}
	// And what a deployment might reasonably declare, it must not — those hosts
	// belong to the neighbouring data plane.
	for _, s := range []string{"", "foo", "abc-dev", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.ToUpper(minted), strings.Repeat("g", 32)} {
		if IsToken(s) {
			t.Errorf("%q recognised as a token", s)
		}
	}
}
