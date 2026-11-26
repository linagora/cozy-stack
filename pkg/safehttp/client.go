// Package safehttp can be used for making http requests when the hostname is
// not trusted (user inputs). It will avoid SSRF by ensuring that the IP
// address which will connect is not a private address, or loopback. It also
// checks that the port is 80 or 443, not anything else.
//
// Operators can configure trusted private networks (CIDRs) to allow safehttp
// callers to reach private addresses in closed-network deployments.
package safehttp

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"

	build "github.com/cozy/cozy-stack/pkg/config"
)

// trustedPrivateNetworks holds the parsed CIDRs that are allowed to bypass
// private-IP and port restrictions. Set once at startup via
// SetTrustedPrivateNetworks. Stored as an atomic.Value to avoid data races
// between config/test setup and concurrent outbound requests.
var trustedPrivateNetworks atomic.Value // holds []*net.IPNet

var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// These transition mechanisms cannot be checked as ordinary destination IPs.
var unsupportedTransitionPrefixes = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b:1::/48"), // Network-specific NAT64 layouts
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("2001::/32"),      // Teredo
}

// SetTrustedPrivateNetworks parses the given CIDR strings and stores them for
// use by safeControl. It should be called once at startup. If any CIDR is
// invalid, it returns an error and does not update the allowlist.
func SetTrustedPrivateNetworks(cidrs []string) error {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid trusted private network CIDR %q: %w", cidr, err)
		}
		nets = append(nets, ipnet)
	}
	trustedPrivateNetworks.Store(nets)
	return nil
}

func loadTrustedNetworks() []*net.IPNet {
	nets, _ := trustedPrivateNetworks.Load().([]*net.IPNet)
	return nets
}

var safeDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
	DualStack: true,
	Control:   safeControl,
}

var safeTransport = &http.Transport{
	// Default values for http.DefaultClient
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           safeDialer.DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,

	// As we are connecting to a new host each time, it is better to disable
	// keep-alive
	DisableKeepAlives: true,
}

// DefaultClient is an http client that can be used instead of
// http.DefaultClient to avoid SSRF. It has the same default configuration,
// except it disabled keep-alive, as it is probably not useful in such cases.
var DefaultClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: safeTransport,
}

var transportWithKeepAlive = &http.Transport{
	// Default values for http.DefaultClient
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           safeDialer.DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// ClientWithKeepAlive is an http client that can be used to avoid SSRF. And it
// has keep-alive (contrary to safehttp.DefaultClient). The typical use case is
// moving a Cozy.
var ClientWithKeepAlive = &http.Client{
	Transport: transportWithKeepAlive,
}

func safeControl(network string, address string, conn syscall.RawConn) error {
	if !(network == "tcp4" || network == "tcp6") {
		return fmt.Errorf("%s is not a safe network type", network)
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s is not a valid host/port pair: %s", address, err)
	}

	ipaddress := net.ParseIP(host)
	if ipaddress == nil {
		return fmt.Errorf("%s is not a valid IP address", host)
	}

	ipv6, _ := netip.AddrFromSlice(ipaddress)
	for _, prefix := range unsupportedTransitionPrefixes {
		if prefix.Contains(ipv6) {
			return fmt.Errorf("%s is not a safe transition IP address", host)
		}
	}
	if nat64Prefix.Contains(ipv6) {
		// Apply the same policy to the IPv4 destination behind NAT64.
		ipaddress = ipaddress[12:]
	}

	if ipaddress.IsUnspecified() || ipaddress.IsLinkLocalUnicast() || ipaddress.IsLinkLocalMulticast() {
		return fmt.Errorf("%s is not a valid IP address", host)
	}

	// Trusted private networks bypass private-IP, loopback, and port
	// restrictions only for private or loopback destinations. This is for
	// closed-network deployments where internal services run on private
	// addresses and non-standard ports.
	if (ipaddress.IsPrivate() || ipaddress.IsLoopback()) && isInTrustedNetwork(ipaddress) {
		return nil
	}

	if ipaddress.IsPrivate() {
		return fmt.Errorf("%s is not a public IP address", ipaddress)
	}

	// Allow loopback and custom ports for dev only (127.0.0.1 / localhost), as
	// it can be useful for accepting sharings on cozy.localhost:8080 for
	// example.
	if build.IsDevRelease() {
		return nil
	}

	if ipaddress.IsLoopback() {
		return fmt.Errorf("%s is not a public IP address", ipaddress)
	}

	if port != "80" && port != "443" {
		return fmt.Errorf("%s is not a safe port number", port)
	}

	return nil
}

// CheckHost resolves the host of a URL that will not be fetched with an HTTP
// client (a git remote for example) and applies to its addresses the same
// rules as the safe clients, except for the port.
func CheckHost(host string) error {
	if host == "" {
		return fmt.Errorf("missing host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		network := "tcp4"
		if ip.To4() == nil {
			network = "tcp6"
		}
		if err := safeControl(network, net.JoinHostPort(ip.String(), "443"), nil); err != nil {
			return err
		}
	}
	return nil
}

func isInTrustedNetwork(ip net.IP) bool {
	for _, ipnet := range loadTrustedNetworks() {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}
