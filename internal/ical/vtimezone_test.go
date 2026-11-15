package ical

import (
	"archive/zip"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// goZone loads a zone from the data Go carries (lib/time/zoneinfo.zip,
// what time/tzdata embeds), which is what robinmail uses on a host
// without /usr/share/zoneinfo, as in its Docker image.
func goZone(t *testing.T, name string) *time.Location {
	t.Helper()
	z, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Skip("no zoneinfo.zip in GOROOT:", err)
	}
	defer z.Close()
	f, err := z.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// A VTIMEZONE for years past the zone table of Go's data, across a leap
// year, where that data has a boundary that does not move (found in the
// lab: the server looped and took all memory). RFC 5545 3.6.5: one
// STANDARD and one DAYLIGHT, each with its changes.
func TestVTimezoneGoZoneData(t *testing.T) {
	loc := goZone(t, "Europe/Amsterdam")
	done := make(chan *Component, 1)
	go func() { done <- VTimezone(loc, 2031, 2032) }()
	var vt *Component
	select {
	case vt = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("VTimezone does not finish")
	}
	text := string(Encode(vt))
	if len(vt.Sub("STANDARD")) != 1 || len(vt.Sub("DAYLIGHT")) != 1 {
		t.Fatalf("observances:\n%s", text)
	}
	for _, want := range []string{"DTSTART:20310330T020000", "RDATE:20320328T020000", "DTSTART:20311026T030000", "RDATE:20321031T030000"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %s in\n%s", want, text)
		}
	}
	// And it is read back as the same zone.
	vt.SetText("TZID", "Copy")
	if got := matchZone(vt); got.String() == "Copy" {
		t.Errorf("not matched: %s", text)
	}
}
