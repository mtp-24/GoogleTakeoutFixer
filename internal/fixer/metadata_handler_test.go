package fixer

import (
	"testing"
	"time"
)

func TestEscapeExifValue(t *testing.T) {
	got := escapeExifValue("Ganda dito!\n\nP.S. Tom & Jerry <3")
	want := "Ganda dito!&#10;&#10;P.S. Tom &amp; Jerry &lt;3"
	if got != want {
		t.Errorf("escapeExifValue = %q; want %q", got, want)
	}
}

func TestIsRealTitle(t *testing.T) {
	tests := []struct {
		title, fileName string
		want            bool
	}{
		{"IMG_0001.HEIC", "IMG_0001.HEIC", false},
		{"IMG_0001.HEIC", "IMG_0001(1).HEIC", false},
		{"IMG_0001.jpg", "IMG_0001-edited.jpg", false},
		{"IMG_0001.HEIC", "IMG_0001.jpg", false}, // same name, Google changed the extension
		{"Our wedding day", "IMG_0001.jpg", true},
		{"", "IMG_0001.jpg", false},
	}
	for _, test := range tests {
		if got := isRealTitle(test.title, test.fileName); got != test.want {
			t.Errorf("isRealTitle(%q, %q) = %v; want %v", test.title, test.fileName, got, test.want)
		}
	}
}

func TestPhotoLocation(t *testing.T) {
	taken := time.Date(2025, 4, 26, 1, 23, 16, 0, time.UTC) // 09:23:16 in Perth (+08:00)
	wall := func(s string) time.Time {
		t, _ := time.Parse("2006:01:02 15:04:05", s)
		return t
	}

	tests := []struct {
		name     string
		geo      geoData
		existing existingDate
		offset   string
		source   string
	}{
		{"location wins", geoData{Latitude: -31.95, Longitude: 115.86}, existingDate{found: true, hasOffset: true, offsetSec: 0}, "+08:00", "location"},
		{"offset in file", geoData{}, existingDate{found: true, wallClock: wall("2025:04:26 09:23:16"), hasOffset: true, offsetSec: 8 * 3600}, "+08:00", "offset in file"},
		{"local time in file", geoData{}, existingDate{found: true, wallClock: wall("2025:04:26 09:23:16")}, "+08:00", "local time in file"},
		{"half hour zone", geoData{}, existingDate{found: true, wallClock: wall("2025:04:26 10:53:16")}, "+09:30", "local time in file"},
		{"implausible local time", geoData{}, existingDate{found: true, wallClock: wall("2025:04:26 09:30:00")}, "", "this computer"},
		{"nothing known", geoData{}, existingDate{}, "", "this computer"},
	}
	for _, test := range tests {
		loc, source := photoLocation(taken, test.geo, test.existing)
		_, offsetSec := taken.In(loc).Zone()
		if source != test.source || (test.offset != "" && formatTimezoneOffset(offsetSec) != test.offset) {
			t.Errorf("%s: got %s from %q; want %s from %q", test.name, formatTimezoneOffset(offsetSec), source, test.offset, test.source)
		}
	}
}
