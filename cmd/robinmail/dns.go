package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// dkimSelector is the selector contrib/rspamd/local.d/dkim_signing.conf
// signs with.
const dkimSelector = "robinmail"

// dkimPublicKey reads the private key Rspamd signs with and returns the
// "p=" value of the DNS record: the public key as base64 of its
// SubjectPublicKeyInfo (RFC 6376 3.6.1). A file that does not exist is
// made first: RSA with 2048 bits (RFC 8301 3.2), PKCS#8 in PEM.
func dkimPublicKey(path string) (pub string, created bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return "", false, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return "", false, err
		}
		data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		// O_EXCL: never over a key that appeared in the meantime.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", false, err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return "", false, err
		}
		if err := f.Close(); err != nil {
			return "", false, err
		}
		created = true
	} else if err != nil {
		return "", false, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", false, fmt.Errorf("%s: not a PEM file", path)
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return "", false, fmt.Errorf("%s: not an RSA key", path)
		}
		key = rk
	} else if key, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		return "", false, fmt.Errorf("%s: not an RSA private key", path)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", false, err
	}
	return base64.StdEncoding.EncodeToString(der), created, nil
}

// listenPort is the port of a listen address of the config (":993",
// "127.0.0.1:1993"), or "" for none.
func listenPort(listen string) string {
	if listen == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "0" {
		return ""
	}
	return port
}

// txtValue writes a TXT value as zone files want it: in strings of at most
// 255 octets.
func txtValue(s string) string {
	var parts []string
	for len(s) > 255 {
		parts = append(parts, `"`+s[:255]+`"`)
		s = s[255:]
	}
	return strings.Join(append(parts, `"`+s+`"`), " ")
}

// dns prints the DNS records a domain needs to send and receive mail
// through this server: MX, SPF, DKIM, DMARC, MTA-STS, TLS-RPT, and what
// mail clients look up to configure themselves. With a key file it includes the DKIM record of that
// key, and makes the key if the file is not there yet.
func (a *app) dns(arg, keyFile string) error {
	name, err := domain(arg)
	if err != nil {
		return err
	}
	host := a.cfg.Hostname
	w := a.stdout
	fmt.Fprintf(w, "; DNS records for %s, with mail handled by %s\n\n", name, host)
	fmt.Fprintf(w, "%s.\tMX\t10 %s.\n", name, host)
	fmt.Fprintf(w, "%s.\tTXT\t%s\n", name, txtValue("v=spf1 a:"+host+" -all"))
	if keyFile != "" {
		pub, created, err := dkimPublicKey(keyFile)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s._domainkey.%s.\tTXT\t%s\n", dkimSelector, name, txtValue("v=DKIM1; k=rsa; p="+pub))
		if created {
			fmt.Fprintf(w, "; wrote a new key to %s: give it to Rspamd as <dkim dir>/%s.%s.key, readable by Rspamd only\n",
				keyFile, name, dkimSelector)
		}
	} else {
		fmt.Fprintf(w, "; no DKIM record: name a key file, as in \"robinmail dns %s %s.%s.key\"\n", name, name, dkimSelector)
	}
	fmt.Fprintf(w, "_dmarc.%s.\tTXT\t%s\n", name, txtValue("v=DMARC1; p=quarantine; rua=mailto:postmaster@"+name))

	// MTA-STS (RFC 8461): the TXT record says there is a policy and which;
	// the policy is served by robinmail under mta-sts.<domain>.
	fmt.Fprintf(w, "\n; MTA-STS (RFC 8461)\n")
	if policy, ok := stsPolicy(a.cfg.MTASTS); ok {
		fmt.Fprintf(w, "_mta-sts.%s.\tTXT\t%s\n", name, txtValue("v=STSv1; id="+stsPolicyID(policy)))
		fmt.Fprintf(w, "mta-sts.%s.\tCNAME\t%s.\n", name, host)
		fmt.Fprintf(w, "; robinmail serves https://mta-sts.%s/.well-known/mta-sts.txt on its web listener:\n;\n", name)
		for _, line := range strings.Split(strings.TrimSpace(policy), "\r\n") {
			fmt.Fprintf(w, ";   %s\n", line)
		}
		fmt.Fprintf(w, ";\n; The id changes with the policy: print these records again after changing mta_sts in the config.\n")
		if !a.cfg.TLS.ACME {
			fmt.Fprintf(w, "; The certificate in the config has to be valid for mta-sts.%s too.\n", name)
		}
	} else {
		fmt.Fprintf(w, "; none: set mta_sts.mode in the config (\"testing\" first) and print these records again\n")
	}

	// TLS reporting (RFC 8460): where other servers send their reports
	// about TLS with this domain. They arrive as mail and are kept
	// ("robinmail tlsrpt received").
	fmt.Fprintf(w, "\n; TLS reports (RFC 8460); the address has to exist, as a user or an alias\n")
	fmt.Fprintf(w, "_smtp._tls.%s.\tTXT\t%s\n", name, txtValue("v=TLSRPTv1; rua=mailto:postmaster@"+name))

	// Mail clients: where they find the servers by the domain alone.
	fmt.Fprintf(w, "\n; Mail clients: Thunderbird's autoconfig, and the SRV records of RFC 6186 and RFC 8314\n")
	fmt.Fprintf(w, "autoconfig.%s.\tCNAME\t%s.\n", name, host)
	srv := func(service string, listen string) {
		// A target of "." says the service is not offered (RFC 2782, RFC 6186 3.4).
		if port := listenPort(listen); port != "" {
			fmt.Fprintf(w, "%s._tcp.%s.\tSRV\t0 1 %s %s.\n", service, name, port, host)
		} else {
			fmt.Fprintf(w, "%s._tcp.%s.\tSRV\t0 0 0 .\n", service, name)
		}
	}
	srv("_imaps", a.cfg.IMAP.ListenTLS)
	srv("_imap", a.cfg.IMAP.Listen)
	srv("_submissions", a.cfg.Submission.ListenTLS)
	srv("_submission", a.cfg.Submission.Listen)
	srv("_pop3s", "")
	srv("_pop3", "")
	srv("_jmap", a.cfg.Web.Listen)
	srv("_autodiscover", a.cfg.Web.Listen)
	if !a.cfg.TLS.ACME {
		fmt.Fprintf(w, "; The certificate in the config has to be valid for autoconfig.%s too.\n", name)
	}

	fmt.Fprintf(w, "\n; Start with mode testing and p=quarantine; move to enforce and p=reject when reports are clean.\n")
	fmt.Fprintf(w, "; %s needs an A/AAAA record and a PTR record that names it.\n", host)
	return nil
}
