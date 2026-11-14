// Package ipkey says what counts as one client address for a limit.
//
// An IPv4 address is one client. An IPv6 address is not: a customer of
// any provider gets a /64 at the least, and can use every address in it.
// A limit per address would give such a client 2^64 times its share, so
// limits on connections and on failed logins count a /64 as one.
package ipkey

import "net/netip"

// Of returns the key under which an address is counted: the address for
// IPv4 (also when written as an IPv4-mapped IPv6 address), the /64 for
// IPv6, without a zone. Text that is no address is returned as it is.
func Of(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}
