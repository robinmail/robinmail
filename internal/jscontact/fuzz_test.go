package jscontact

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

var fuzzNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// FuzzVCard: a vCard from anyone (CardDAV PUT, an import) is turned into
// JSContact and back without a panic.
func FuzzVCard(f *testing.F) {
	f.Add([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:1\r\nFN:Jane Doe\r\nN:Doe;Jane;;Dr.;\r\nEMAIL;TYPE=work;PREF=1:jane@example.test\r\nTEL;VALUE=uri;TYPE=cell:tel:+31-6-1234\r\nADR;TYPE=home:;;Street 1;Town;;1234;NL\r\nitem1.URL:https://example.test\r\nitem1.X-ABLabel:site\r\nBDAY:--0415\r\nPHOTO:data:image/png;base64,AAAA\r\nMEMBER:urn:uuid:2\r\nKIND:group\r\nEND:VCARD\r\n"))
	f.Add([]byte("BEGIN:VCARD\nVERSION:3.0\nFN:x\nORG:A;B\nEND:VCARD\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := ical.ParseOne(data, "VCARD")
		if err != nil {
			return
		}
		c := ToJS(v)
		Names(c)
		if back, err := ToVCard(c, v, fuzzNow); err == nil {
			ToJS(back)
		}
	})
}

// FuzzCard: a JSContact card from an account holder (JMAP ContactCard/set)
// is written as a vCard that parses again.
func FuzzCard(f *testing.F) {
	f.Add([]byte(`{"@type":"Card","version":"1.0","uid":"1","kind":"individual","name":{"components":[{"kind":"given","value":"Jane"},{"kind":"surname","value":"Doe"}]},"emails":{"e":{"address":"jane@example.test","contexts":{"work":true},"pref":1}},"phones":{"p":{"number":"+31","features":{"mobile":true}}},"addresses":{"a":{"components":[{"kind":"name","value":"Street"},{"kind":"locality","value":"Town"}]}},"anniversaries":{"b":{"kind":"birth","date":{"@type":"PartialDate","month":4,"day":15}}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var c Card
		if json.Unmarshal(data, &c) != nil || c == nil {
			return
		}
		v, err := ToVCard(c, nil, fuzzNow)
		if err != nil {
			return
		}
		again, err := ical.ParseOne(ical.Encode(v), "VCARD")
		if err != nil {
			t.Fatalf("ToVCard wrote what does not parse: %v\n%s", err, ical.Encode(v))
		}
		ToJS(again)
	})
}
