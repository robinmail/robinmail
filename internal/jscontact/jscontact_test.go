package jscontact

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/ical"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func norm(v any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// The example vCard of RFC 6350 8.
const rfc6350Example = "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Simon Perreault\r\nN:Perreault;Simon;;;ing. jr,M.Sc.\r\n" +
	"BDAY:--0203\r\nANNIVERSARY:20090808T1430-0500\r\nGENDER:M\r\nLANG;PREF=1:fr\r\nLANG;PREF=2:en\r\n" +
	"ORG;TYPE=work:Viagenie\r\nADR;TYPE=work:;Suite D2-630;2875 Laurier;\r\n Quebec;QC;G1V 2M2;Canada\r\n" +
	"TEL;VALUE=uri;TYPE=\"work,voice\";PREF=1:tel:+1-418-656-9254;ext=102\r\n" +
	"TEL;VALUE=uri;TYPE=\"work,cell,voice,video,text\":tel:+1-418-262-6501\r\n" +
	"EMAIL;TYPE=work:simon.perreault@viagenie.ca\r\nGEO;TYPE=work:geo:46.772673,-71.282945\r\n" +
	"KEY;TYPE=work;VALUE=uri:\r\n http://www.viagenie.ca/simon.perreault/simon.asc\r\nTZ:-0500\r\n" +
	"URL;TYPE=home:http://nomis80.org\r\nEND:VCARD\r\n"

// RFC 9555 2 and 3: the vCard of RFC 6350 8 in JSContact.
func TestRFC9555_RFC6350Example(t *testing.T) {
	v, err := ical.ParseOne([]byte(rfc6350Example), "VCARD")
	if err != nil {
		t.Fatal(err)
	}
	c := norm(ToJS(v))
	name := c["name"].(map[string]any)
	if name["full"] != "Simon Perreault" {
		t.Errorf("name %v", name)
	}
	comps := name["components"].([]any)
	var kinds []string
	for _, x := range comps {
		m := x.(map[string]any)
		kinds = append(kinds, m["kind"].(string)+"="+m["value"].(string))
	}
	if strings.Join(kinds, ";") != "given=Simon;surname=Perreault;credential=ing. jr;credential=M.Sc." {
		t.Errorf("components %v", kinds)
	}
	birth := c["anniversaries"].(map[string]any)["birth"].(map[string]any)["date"].(map[string]any)
	if birth["month"] != 2.0 || birth["day"] != 3.0 || birth["year"] != nil {
		t.Errorf("BDAY --0203: %v", birth)
	}
	var mobile map[string]any
	for _, x := range c["phones"].(map[string]any) {
		p := x.(map[string]any)
		if p["number"] == "tel:+1-418-262-6501" {
			mobile = p
		}
	}
	f := mobile["features"].(map[string]any)
	if f["mobile"] != true || f["voice"] != true || f["video"] != true || f["text"] != true || mobile["contexts"].(map[string]any)["work"] != true {
		t.Errorf("phone %v", mobile)
	}
	for _, x := range c["addresses"].(map[string]any) {
		a := x.(map[string]any)
		comps := a["components"].([]any)
		if len(comps) != 6 || comps[1].(map[string]any)["value"] != "2875 Laurier" {
			t.Errorf("address %v", a)
		}
	}
	// Back to vCard: the card keeps 4.0 and what JSContact had no word for.
	// The example has no UID; a stored card always has one.
	c["uid"] = "urn:uuid:4fbe8971-0bc3-424c-9c26-36c3e1eff6b1"
	out, err := ToVCard(c, v, now)
	if err != nil {
		t.Fatal(err)
	}
	text := string(ical.Encode(out))
	for _, want := range []string{"VERSION:4.0", "GENDER:M", "TZ:-0500", "geo:46.772673,-71.282945", "FN:Simon Perreault",
		"N:Perreault;Simon;;;ing. jr,M.Sc.", "BDAY:--0203", "EMAIL;TYPE=work:simon.perreault@viagenie.ca"} {
		if !strings.Contains(strings.ReplaceAll(text, "\r\n ", ""), want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	// And again to JSContact: the same card.
	again := norm(ToJS(out))
	delete(again, "updated")
	delete(c, "updated")
	a, _ := json.Marshal(again)
	b, _ := json.Marshal(c)
	if string(a) != string(b) {
		t.Errorf("not stable:\n%s\n%s", a, b)
	}
}

// Apple's cards (vCard 3.0): labels on grouped properties, groups as
// X-ADDRESSBOOKSERVER-KIND. RFC 6352 5.1: a server has to take 3.0.
func TestAppleCard(t *testing.T) {
	in := "BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//Apple Inc.//iPhone OS 18.0//EN\r\nN:Doe;Jane;;;\r\nFN:Jane Doe\r\n" +
		"item1.EMAIL;type=INTERNET;type=pref:jane@example.com\r\nitem1.X-ABLabel:_$!<Other>!$_\r\n" +
		"TEL;type=CELL;type=VOICE;type=pref:+31 6 12345678\r\nBDAY;value=date:1990-04-15\r\nUID:6A2E-1\r\n" +
		"X-ABUID:6A2E-1:ABPerson\r\nEND:VCARD\r\n"
	v, err := ical.ParseOne([]byte(in), "VCARD")
	if err != nil {
		t.Fatal(err)
	}
	c := norm(ToJS(v))
	var email map[string]any
	for _, x := range c["emails"].(map[string]any) {
		email = x.(map[string]any)
	}
	if email["label"] != "Other" || email["pref"] != 1.0 {
		t.Errorf("email %v", email)
	}
	c["emails"].(map[string]any)["new"] = map[string]any{"@type": "EmailAddress", "address": "jd@work.example", "contexts": map[string]any{"work": true}}
	out, err := ToVCard(c, v, now)
	if err != nil {
		t.Fatal(err)
	}
	text := string(ical.Encode(out))
	for _, want := range []string{"VERSION:3.0", "X-ABUID:6A2E-1:ABPerson", "X-ABLABEL:Other", "EMAIL;TYPE=work,internet:jd@work.example",
		"BDAY:1990-04-15"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if strings.Count(text, "X-ABLABEL") != 1 {
		t.Errorf("labels left over:\n%s", text)
	}

	group := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Family\r\nN:Family;;;;\r\nX-ADDRESSBOOKSERVER-KIND:group\r\n" +
		"X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:6A2E-1\r\nUID:g1\r\nEND:VCARD\r\n"
	g, _ := ical.ParseOne([]byte(group), "VCARD")
	gc := ToJS(g)
	if gc["kind"] != "group" || gc["members"].(map[string]any)["6A2E-1"] != true {
		t.Errorf("group %v", gc)
	}
}

// RFC 9553 2.1.9 and 2.2.1: a card from JSContact alone becomes vCard 3.0
// with FN and N, which RFC 2426 requires.
func TestNewCard(t *testing.T) {
	var c map[string]any
	json.Unmarshal([]byte(`{"@type":"Card","version":"1.0","uid":"urn:uuid:1","name":{"components":[
		{"kind":"given","value":"Robert"},{"kind":"given2","value":"Pau"},{"kind":"surname","value":"Shou Chang"}]},
		"phones":{"p1":{"number":"+1-555-555-5555","features":{"mobile":true},"contexts":{"private":true}}},
		"anniversaries":{"a":{"kind":"birth","date":{"@type":"PartialDate","year":1980,"month":2,"day":29}}}}`), &c)
	v, err := ToVCard(c, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	text := string(ical.Encode(v))
	for _, want := range []string{"VERSION:3.0", "FN:Robert Pau Shou Chang", "N:Shou Chang;Robert;Pau;;", "TEL;TYPE=home,cell:+1-555-555-5555", "BDAY:1980-02-29"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	for prop, js := range map[string]string{"uid": `{"@type":"Card"}`, "emails/x/address": `{"uid":"u","emails":{"x":{}}}`, "kind": `{"uid":"u","kind":"robot"}`} {
		c = nil
		json.Unmarshal([]byte(js), &c)
		_, err := ToVCard(c, nil, now)
		if pe, ok := err.(*PropError); !ok || pe.Props[0] != prop {
			t.Errorf("%s: %v", prop, err)
		}
		c = nil
	}
}
