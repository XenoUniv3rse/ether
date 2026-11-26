package clientnet

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// fakeDockerDNS answers A queries for names in table and nothing else, like
// Docker's embedded DNS answering a container name.
func fakeDockerDNS(t *testing.T, table map[string]netip.Addr) *net.Resolver {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			if len(q) < 12 {
				continue
			}
			// Question: labels from offset 12, then type and class.
			var labels []string
			i := 12
			for i < len(q) && q[i] != 0 {
				l := int(q[i])
				if l > 63 || i+1+l > len(q) {
					break
				}
				labels = append(labels, string(q[i+1:i+1+l]))
				i += 1 + l
			}
			qend := i + 5
			if qend > len(q) || q[i] != 0 {
				continue
			}
			qtype := binary.BigEndian.Uint16(q[i+1 : i+3])
			name := strings.Join(labels, ".")
			resp := append([]byte{}, q[:qend]...)
			resp[2], resp[3] = 0x81, 0x80 // response, recursion available, NOERROR
			resp[4], resp[5] = 0, 1
			resp[8], resp[9], resp[10], resp[11] = 0, 0, 0, 0
			addr, ok := table[name]
			if ok && qtype == 1 && addr.Is4() {
				resp[6], resp[7] = 0, 1
				a := addr.As4()
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, a[0], a[1], a[2], a[3])
			} else {
				resp[6], resp[7] = 0, 0
				if !ok {
					resp[3] = 0x83 // NXDOMAIN
				}
			}
			_, _ = conn.WriteTo(resp, from)
		}
	}()
	return embeddedDNSResolver(conn.LocalAddr().String())
}

func TestLookupProxyHostFallsBackToDockerDNS(t *testing.T) {
	addr := netip.MustParseAddr("172.18.0.2")
	resolver := fakeDockerDNS(t, map[string]netip.Addr{"npm-proxy-test.invalid": addr})
	system := func(context.Context) ([]netip.Addr, error) {
		return nil, context.DeadlineExceeded
	}
	docker := func(ctx context.Context) ([]netip.Addr, error) {
		return resolver.LookupNetIP(ctx, "ip", "npm-proxy-test.invalid.")
	}
	ctx := context.Background()
	if _, err := lookupProxyDNS(ctx, false, system, docker); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup outside Docker = %v, want the system error", err)
	}
	addrs, err := lookupProxyDNS(ctx, true, system, docker)
	if err != nil || len(addrs) != 1 || addrs[0] != addr {
		t.Fatalf("Docker lookup = %v, %v; want %s", addrs, err, addr)
	}
	_, err = lookupProxyDNS(ctx, true, system, func(ctx context.Context) ([]netip.Addr, error) {
		return resolver.LookupNetIP(ctx, "ip", "nobody-proxy-test.invalid.")
	})
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("Docker lookup = %v, want NXDOMAIN despite the system timeout", err)
	}
}
