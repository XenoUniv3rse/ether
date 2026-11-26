package clientnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const dockerDNSBudget = 500 * time.Millisecond

// A container sharing a VPN container's network may have a resolv.conf
// pointing at the VPN resolver even while Docker's embedded DNS is available.
var dockerResolver = embeddedDNSResolver("127.0.0.11:53")

func embeddedDNSResolver(endpoint string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, endpoint)
		},
	}
}

var inDockerNetwork = sync.OnceValue(func() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return false
	}
	// Docker also creates /.dockerenv in host mode. Linux exposes
	// netdev_max_backlog only in the initial network namespace.
	// Require a visible network sysctl tree so an absent or unreadable procfs doesn't enable fallback.
	const netCore = "/proc/sys/net/core/"
	if _, err := os.Stat(netCore + "somaxconn"); err != nil {
		return false
	}
	_, err := os.Stat(netCore + "netdev_max_backlog")
	return errors.Is(err, os.ErrNotExist)
})

// LookupProxyHost resolves a proxy host, falling back to Docker's embedded DNS
// within a Docker network namespace.
func LookupProxyHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return lookupProxyDNS(ctx, inDockerNetwork(),
		func(ctx context.Context) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		func(ctx context.Context) ([]netip.Addr, error) {
			// Search domains from resolv.conf belong to the system resolver.
			return dockerResolver.LookupNetIP(ctx, "ip", strings.TrimSuffix(host, ".")+".")
		})
}

// LookupProxyAddr reverse-resolves a proxy address using the same policy.
func LookupProxyAddr(ctx context.Context, addr string) ([]string, error) {
	return lookupProxyDNS(ctx, inDockerNetwork(),
		func(ctx context.Context) ([]string, error) {
			return net.DefaultResolver.LookupAddr(ctx, addr)
		},
		func(ctx context.Context) ([]string, error) {
			return dockerResolver.LookupAddr(ctx, addr)
		})
}

func lookupProxyDNS[T any](ctx context.Context, useDocker bool, system, docker func(context.Context) ([]T, error)) ([]T, error) {
	if !useDocker {
		return system(ctx)
	}

	// Reserve part of the caller's budget for Docker even if the system
	// resolver stalls. The Docker deadline bounds the whole exchange.
	ctx, cancel := context.WithTimeout(ctx, proxyLookupBudget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	budget := min(dockerDNSBudget, time.Until(deadline)/2)
	systemCtx, cancelSystem := context.WithDeadline(ctx, deadline.Add(-budget))
	results, err := system(systemCtx)
	cancelSystem()
	if (err == nil && len(results) > 0) || ctx.Err() != nil {
		return results, err
	}

	dockerCtx, cancelDocker := context.WithTimeout(ctx, budget)
	defer cancelDocker()
	fallback, fallbackErr := docker(dockerCtx)
	if fallbackErr == nil && len(fallback) > 0 && dockerCtx.Err() == nil {
		return fallback, nil
	}
	// A negative answer revokes cached trust even if the other resolver
	// timed out. A timeout alone permits the cache's bounded stale window.
	var dnsErr *net.DNSError
	fallbackNotFound := errors.As(fallbackErr, &dnsErr) && dnsErr.IsNotFound
	if fallbackNotFound || proxyDNSTimedOut(err) {
		return nil, fallbackErr
	}
	return results, err
}
