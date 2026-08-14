package sandbox

import (
	"strconv"
	"strings"
)

// HostPrefix leads every sandbox hostname. It is what makes a sandbox host
// recognisable on a listener it shares with other workloads: without it, any
// single-label host under the domain would read as a capability token.
const HostPrefix = "s-"

// Addressing renders sandbox hostnames and reads them back. Both directions
// live here because they are one grammar, and a writer that drifts from its
// reader hands out URLs that resolve to nothing:
//
//	s-{token}.{domain}         the sandbox's primary port
//	s-{token}-{port}.{domain}  one of its declared extra ports
//
// The token and the port share ONE DNS label because a wildcard certificate
// covers exactly one (RFC 6125) — nesting the port as its own label would need
// a certificate per sandbox.
type Addressing struct {
	// Domain is the wildcard domain sandboxes are reached at.
	Domain string
	// Scheme is what callers are handed — https where a gateway terminates TLS.
	Scheme string
	// Port is the port that appears in the URL. Empty (or "80") renders a bare
	// host: the Kubernetes gateway fronts port 80, where the Docker data
	// listener is the edge itself and has to be dialled on its own port.
	Port string
}

// Host renders a sandbox's hostname. port 0 names its primary port. A sandbox
// with no token has no hostname — nothing is serving, so there is nothing to
// address.
func (a Addressing) Host(token string, port int) string {
	if token == "" {
		return ""
	}
	label := HostPrefix + token
	if port != 0 {
		label += "-" + strconv.Itoa(port)
	}
	return label + "." + a.Domain
}

// URL addresses a sandbox's primary port.
func (a Addressing) URL(token string) string { return a.addressed(a.Host(token, 0)) }

// PortURL addresses one of a sandbox's extra ports.
func (a Addressing) PortURL(token string, port int) string {
	return a.addressed(a.Host(token, port))
}

// URLs addresses every port a sandbox serves, keyed by port number, so a caller
// never has to assemble a hostname itself.
func (a Addressing) URLs(token string, primary int, ports []int) map[string]string {
	if token == "" {
		return nil
	}
	urls := make(map[string]string, len(ports)+1)
	urls[strconv.Itoa(primary)] = a.URL(token)
	for _, port := range ports {
		urls[strconv.Itoa(port)] = a.PortURL(token, port)
	}
	return urls
}

// Resolve reads a hostname back into the capability token it addresses and the
// port it named, if it named one. It accepts a Host header, port and all.
//
// ok is false for anything that is not a sandbox host under this domain. The
// prefix is REQUIRED rather than trimmed: on a listener shared with another
// data plane, trimming would turn every host under the domain into a token and
// swallow requests meant for its neighbour.
func (a Addressing) Resolve(hostport string) (token, port string, ok bool) {
	label, domain, cut := strings.Cut(stripPort(hostport), ".")
	if !cut || !strings.EqualFold(domain, a.Domain) {
		return "", "", false
	}
	rest, prefixed := strings.CutPrefix(label, HostPrefix)
	if !prefixed || rest == "" {
		return "", "", false
	}
	// A hyphen only names a port when what follows is one, so a token holding a
	// hyphen (or a trailing word) is not mistaken for an extra port.
	if base, suffix, cut := strings.Cut(rest, "-"); cut && isPort(suffix) {
		return base, suffix, true
	}
	return rest, "", true
}

// addressed wraps a hostname in the scheme and, where the edge is not fronted
// at port 80, the port a caller has to dial.
func (a Addressing) addressed(host string) string {
	if host == "" {
		return ""
	}
	addr := a.Scheme + "://" + host
	if a.Port != "" && a.Port != "80" {
		addr += ":" + a.Port
	}
	return addr
}

// IsToken reports whether a label has the shape mintToken emits: lowercase hex
// of the full token width. Resolve deliberately accepts any label — it is the
// grammar, not the credential check — but where a listener serves the sandbox
// wildcard alongside another data plane, a host that cannot be a token is
// better given to the neighbour than 404'd here.
func IsToken(s string) bool {
	if len(s) != tokenBytes*2 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// stripPort drops a :port suffix, leaving an IPv6 literal alone.
func stripPort(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i != -1 && !strings.Contains(hostport[i:], "]") {
		return hostport[:i]
	}
	return hostport
}

// isPort reports whether s is a plausible port number.
func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535
}
