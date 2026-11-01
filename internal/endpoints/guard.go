package endpoints

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"
)

// guardedTransport dials only public addresses, apart from hosts the admin
// allows. A user's endpoint is fetched from inside the server's network:
// pointed at Vault or Postgres, its errors would read their answers back to
// the user. The check runs on the address actually dialled, after DNS, so a
// name cannot pass validation and later resolve inside.
func guardedTransport(allowPrivate bool, allowedHosts []string) http.RoundTripper {
	plain := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}

			if a, err := netip.ParseAddr(host); err != nil || !public(a) {
				return fmt.Errorf("%w: %s is inside the server's network", ErrInvalid, host)
			}

			return nil
		}}

	return &http.Transport{
		// A proxy would dial for us, past the check.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if allowPrivate || slices.Contains(allowedHosts, strings.ToLower(host)) {
				return plain.DialContext(ctx, network, addr)
			}

			return guarded.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}
