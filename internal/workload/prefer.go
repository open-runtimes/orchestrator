package workload

import (
	"net/http"
	"strings"
)

// The async preference is one protocol rule with several enforcement points
// — the pools API, both activator edges, and the gateway's HTTPRoute rule —
// so it lives here once, with the rest of the data-plane contract.

// PreferAsync reports whether the request prefers an async response: the
// single respond-async token, matched case-insensitively (RFC 7240 tokens
// are case-insensitive). Combined forms ("respond-async, wait=10") are not
// recognized — by design, and uniformly across every enforcement point.
func PreferAsync(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Prefer"), "respond-async")
}

// PreferAsyncPattern is the same rule as a gateway HTTPRoute regex header
// match (RegularExpression is Extended — the same support tier as the
// per-backendRef RequestHeaderModifier the routing design already requires).
// prefer_test.go pins that this and PreferAsync accept the same values.
const PreferAsyncPattern = "(?i)^respond-async$"
