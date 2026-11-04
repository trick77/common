package webfetch

import (
	"context"
	"fmt"
	"net"
	"syscall"
)

// GuardedControl is a net.Dialer Control hook that rejects connections to
// non-public IP addresses. It runs after DNS resolution with the concrete IP
// the socket is about to connect to, so it also blocks DNS-rebinding to a
// private target.
//
// It is exported so a caller that makes its own requests gets the same guard:
// set it as the Control of the net.Dialer its transport dials through.
//
// This replaces the SSRF protection that the deployment previously got for
// free by isolating the fetch sidecar on its own Docker network: with fetch
// now running in-process on the app network, a model-chosen (or prompt-
// injected) URL could otherwise reach loopback, RFC1918 hosts, or the cloud
// metadata endpoint (169.254.169.254). Blocking is done here, in the dialer,
// because that is the only place the real destination IP is known.
func GuardedControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" && network != "tcp" {
		return fmt.Errorf("webfetch: refusing non-tcp network %q", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("webfetch: cannot parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("webfetch: refusing to dial unresolved host %q", host)
	}
	if !IsPublicIP(ip) {
		return fmt.Errorf("webfetch: refusing to connect to non-public address %s", ip)
	}
	return nil
}

// globalUnicastIPv6 is 2000::/3, the only IPv6 space IANA allocates for global
// unicast. An IPv6 address outside it is never public, so IsPublicIP rejects it
// before any other check. That alone covers loopback, link-local, multicast,
// ULA (fc00::/7) and every reserved block outside 2000::/3, among them:
// ::/96 (IPv4-compatible), ::ffff:0:0:0/96 (IPv4-translated, SIIT),
// 64:ff9b::/96 and 64:ff9b:1::/48 (NAT64), 100::/64 (discard),
// 100:0:0:1::/64 (dummy), 5f00::/16 (SRv6), fec0::/10 (site-local).
var globalUnicastIPv6 = mustParseCIDRs("2000::/3")[0]

// specialUseRanges are IANA special-use / non-globally-routable prefixes that
// IsPublicIP's other checks do not already cover. Membership in any of these
// makes an address non-public. This is a default-deny model: only
// globally-routable unicast addresses outside every special-use range are
// allowed. The IPv6 entries are the special-use blocks inside 2000::/3.
var specialUseRanges = mustParseCIDRs(
	// IPv4
	"0.0.0.0/8",       // "this host on this network"
	"100.64.0.0/10",   // carrier-grade NAT (RFC 6598)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1 (documentation)
	"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2 (documentation)
	"203.0.113.0/24",  // TEST-NET-3 (documentation)
	"240.0.0.0/4",     // reserved / future use (incl. 255.255.255.255)
	// IPv6
	"2001::/23",     // IETF protocol assignments (incl. Teredo, benchmarking, ORCHID)
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4
	"3fff::/20",     // documentation
)

// IsPublicIP reports whether ip is a globally-routable public unicast address
// the fetcher is allowed to reach. It is a strict allowlist: loopback, private
// (RFC1918 + ULA fc00::/7), link-local (incl. the 169.254.169.254 metadata
// endpoint), unspecified, broadcast, multicast, IPv6 outside 2000::/3, and the
// special-use ranges listed above are rejected — only public unicast passes.
// A nil or malformed ip is not public.
//
// It judges an IP, never a hostname: checking a resolved name and then dialing
// it is open to DNS rebinding, so a guard for outbound connections belongs in
// GuardedControl. The rejected set may grow in any release.
func IsPublicIP(ip net.IP) bool {
	// Normalize IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1) to its IPv4 form so the
	// checks below cannot be bypassed via the mapped representation.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if !globalUnicastIPv6.Contains(ip) {
		return false
	}
	// For IPv4: IsGlobalUnicast rejects unspecified, loopback, broadcast,
	// multicast, and link-local; IsPrivate rejects RFC1918. IPv6 reaching here
	// is already inside 2000::/3, where both are no-ops.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, r := range specialUseRanges {
		if r.Contains(ip) {
			return false
		}
	}
	return true
}

// mustParseCIDRs parses the given CIDRs at init time, panicking on a malformed
// entry (which would be a programming error in the constant list above).
func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("webfetch: invalid special-use CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// dialControl is the net.Dialer Control hook used for all outbound requests. It
// is a package variable (defaulting to the SSRF guard) solely so in-package
// tests can relax it to reach a loopback httptest server; production always
// uses GuardedControl.
var dialControl = GuardedControl

// dialContext is the shared transport's DialContext. It builds the dialer per
// call so dialControl is read at dial time: the transport is created once for
// the process, and the test hook above must still take effect after that.
func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Control: dialControl}
	return d.DialContext(ctx, network, addr)
}
