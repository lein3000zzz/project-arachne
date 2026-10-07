// Package urlnorm is the single definition of URL identity. Every URL that becomes
// a task, a graph node or an index key goes through Canonical, so two spellings of
// one address can never become two pages.
package urlnorm

import (
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// Version must be bumped with any change to what Canonical returns: stored URLs
// are re-canonicalized on the next start when it differs.
const Version = 3

// Query parameters that only track where a click came from. Removing them never
// changes what the server returns.
var trackingParams = map[string]struct{}{
	"gclid": {}, "dclid": {}, "fbclid": {}, "msclkid": {}, "yclid": {}, "ysclid": {},
	"mc_cid": {}, "mc_eid": {}, "_openstat": {}, "igshid": {},
}

var hostProfile = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.BidiRule())

// Canonical resolves raw against base (which may be nil for absolute URLs) and
// returns its canonical form, or false for anything that is not a fetchable
// http(s) URL.
//
// Only syntactic equivalences are applied. Whether /a and /a/, or http and https,
// are the same resource is for the server to say - see the crawler's redirect
// handling.
func Canonical(raw string, base *url.URL) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", false
	}

	// A bare quote is never part of a URL - RFC 3986 has it percent-encoded - so in
	// scraped text it is where the URL ended.
	if i := strings.IndexByte(raw, '"'); i >= 0 {
		raw = raw[:i]
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	if !u.IsAbs() {
		if base == nil {
			return "", false
		}

		u = base.ResolveReference(u)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}

	host, ok := canonicalHost(u)
	if !ok {
		return "", false
	}

	u.Host = host
	u.User = nil

	// An empty reference resolves nothing but still removes dot segments, which
	// url.Parse leaves in place on absolute URLs.
	u = u.ResolveReference(&url.URL{})

	if u.Path == "" {
		u.Path = "/"
		u.RawPath = ""
	}

	u.RawQuery = stripTracking(u.RawQuery)
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""

	return u.String(), true
}

// FromUserInput accepts what a person types: a bare "example.com" means https.
func FromUserInput(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") && !strings.HasPrefix(raw, "//") {
		raw = "https://" + raw
	}

	return Canonical(raw, nil)
}

func canonicalHost(u *url.URL) (string, bool) {
	hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if hostname == "" {
		return "", false
	}

	if !strings.Contains(hostname, ":") {
		ascii, err := hostProfile.ToASCII(hostname)
		if err != nil {
			return "", false
		}

		hostname = ascii
	} else {
		hostname = "[" + hostname + "]"
	}

	port := u.Port()
	if port == "" || u.Scheme == "http" && port == "80" || u.Scheme == "https" && port == "443" {
		return hostname, true
	}

	return hostname + ":" + port, true
}

// stripTracking keeps the remaining parameters in their original order and
// encoding: reordering or re-encoding them can change what some servers return.
func stripTracking(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}

	kept := make([]string, 0)

	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}

		key := pair
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key = pair[:i]
		}

		key = strings.ToLower(key)
		if _, tracking := trackingParams[key]; tracking || strings.HasPrefix(key, "utm_") {
			continue
		}

		kept = append(kept, pair)
	}

	return strings.Join(kept, "&")
}
