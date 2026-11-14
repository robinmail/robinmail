package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
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
// through this server. With a key file it includes the DKIM record of that
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
	fmt.Fprintf(w, "_mta-sts.%s.\tTXT\t%s\n", name, txtValue("v=STSv1; id="+time.Now().UTC().Format("20060102150405")))
	fmt.Fprintf(w, "\n; MTA-STS also needs https://mta-sts.%s/.well-known/mta-sts.txt, served by a web\n", name)
	fmt.Fprintf(w, "; server with a certificate for that name (robinmail does not serve it yet), as text/plain:\n;\n")
	for _, line := range []string{"version: STSv1", "mode: testing", "mx: " + host, "max_age: 86400"} {
		fmt.Fprintf(w, ";   %s\n", line)
	}
	fmt.Fprintf(w, ";\n; Start with mode testing and p=quarantine; move to enforce and p=reject when reports are clean.\n")
	fmt.Fprintf(w, "; %s needs an A/AAAA record and a PTR record that names it.\n", host)
	return nil
}
