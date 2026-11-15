package queue

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sort"
	"strings"

	"golang.org/x/net/idna"
)

// Resolver is the DNS a Worker needs. *net.Resolver has it; tests bring
// their own, so nothing here ever asks a real name server in a test.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// failure is why a destination could not be delivered to, when no server
// said so itself.
type failure struct {
	permanent bool
	status    string // RFC 3463
	msg       string
}

func (f *failure) Error() string { return f.msg }

func temporary(status, msg string) *failure { return &failure{false, status, msg} }
func permanent(status, msg string) *failure { return &failure{true, status, msg} }

// notFound reports a DNS answer that says the name, or the record asked
// for, does not exist. Go's resolver does not tell the two apart.
func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

// destination is where mail for a domain goes: hosts to try, in order.
type destination struct {
	hosts []string
	// implicit is true when the domain has no MX record and its own
	// address is used (RFC 5321 5.1).
	implicit bool
}

// asciiDomain returns the domain in lowercase ASCII, as DNS has it: an
// internationalized name as A-labels.
func asciiDomain(domain string) (string, bool) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if domain == "" || strings.HasPrefix(domain, "[") {
		return "", false
	}
	for i := 0; i < len(domain); i++ {
		if domain[i] >= 0x80 {
			d, err := idna.Lookup.ToASCII(domain)
			return d, err == nil && d != ""
		}
	}
	return domain, true
}

// lookupMX finds the hosts that take mail for a domain, by the rules of
// RFC 5321 section 5.1:
//
//   - The MX records, lowest preference number first; records with the same
//     preference in random order, to spread load.
//   - No MX record at all: the domain itself is the host, as if it had one
//     MX with preference 0 (the "implicit MX").
//   - One MX record with the target "." and preference 0 is a null MX (RFC
//     7505): the domain takes no mail, and nothing is tried.
//   - If we are one of the hosts ourselves, we and every host that is no
//     more preferred than us are dropped: handing the message to them would
//     send it in a circle. If nothing is left, the mail cannot be routed.
//
// A lookup that fails for another reason than "no such record" is a
// temporary failure.
func (w *Worker) lookupMX(ctx context.Context, domain string) (destination, *failure) {
	records, err := w.resolver().LookupMX(ctx, domain)
	// Go returns the valid records together with an error for the ones it
	// refused; what is usable is used.
	if err != nil && len(records) == 0 {
		if !notFound(err) {
			return destination{}, temporary("4.4.3", "looking up the mail servers of "+domain+" failed: "+dnsText(err))
		}
		return destination{hosts: []string{domain}, implicit: true}, nil
	}
	if len(records) == 0 {
		return destination{hosts: []string{domain}, implicit: true}, nil
	}
	type mx struct {
		host string
		pref uint16
	}
	var list []mx
	null := false
	for _, r := range records {
		host := strings.ToLower(strings.TrimSuffix(r.Host, "."))
		if host == "" {
			null = true // the target "."
			continue
		}
		list = append(list, mx{host, r.Pref})
	}
	if len(list) == 0 {
		if null {
			// RFC 7505 4.2 names the code.
			return destination{}, permanent("5.1.10", domain+" does not accept mail (null MX)")
		}
		return destination{hosts: []string{domain}, implicit: true}, nil
	}
	rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	sort.SliceStable(list, func(i, j int) bool { return list[i].pref < list[j].pref })

	own := -1
	for _, m := range list {
		if m.host == w.Hostname && (own < 0 || int(m.pref) < own) {
			own = int(m.pref)
		}
	}
	var hosts []string
	for _, m := range list {
		if own >= 0 && int(m.pref) >= own {
			continue
		}
		hosts = append(hosts, m.host)
	}
	if len(hosts) == 0 {
		return destination{}, permanent("5.4.6", "mail for "+domain+" loops back to this server")
	}
	return destination{hosts: hosts}, nil
}

// maxAddrsPerHost bounds how many addresses of one host are tried.
const maxAddrsPerHost = 4

// addresses resolves a host to the addresses to connect to, in the order
// the resolver gives them (it sorts by RFC 6724, so IPv6 comes first where
// that works). All of them are tried in turn: when one address family
// cannot be reached, the other is the fallback (RFC 5321 5.2).
func (w *Worker) addresses(ctx context.Context, host string) ([]net.IPAddr, *failure) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}
	addrs, err := w.resolver().LookupIPAddr(ctx, host)
	switch {
	case err != nil && notFound(err), err == nil && len(addrs) == 0:
		return nil, permanent("5.1.2", host+" has no address")
	case err != nil:
		return nil, temporary("4.4.3", "looking up "+host+" failed: "+dnsText(err))
	}
	if len(addrs) > maxAddrsPerHost {
		addrs = addrs[:maxAddrsPerHost]
	}
	return addrs, nil
}

// cgnat is the shared address space of RFC 6598: private in all but name.
var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// publicAddress reports whether an address that came out of DNS may be
// connected to: not loopback, not private (RFC 1918, RFC 4193, RFC 6598),
// not link-local (which is where cloud metadata services live), not
// unspecified, multicast or broadcast. An IPv4 address inside an IPv6 one
// (::ffff:a.b.c.d) is judged as the IPv4 address.
func publicAddress(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip == nil, ip.IsLoopback(), ip.IsPrivate(), ip.IsUnspecified(), ip.IsLinkLocalUnicast(),
		ip.IsMulticast(), ip.Equal(net.IPv4bcast), cgnat.Contains(ip):
		return false
	}
	return true
}

func dnsText(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		return de.Err
	}
	return err.Error()
}
