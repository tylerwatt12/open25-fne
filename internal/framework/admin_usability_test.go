package framework

import (
	"bytes"
	"net/http"
	"open25.local/open25-fne/internal/radio"
	"reflect"
	"testing"
	"time"
)

func TestAdminUSBListRetainsOnlyAvailableStableChoicesAndDoesNotWrite(t *testing.T) {
	inventory := HardwareInventoryView{Devices: []HardwareDeviceView{
		{HardwareDevice: radio.HardwareDevice{Kind: "usb", Present: true, PresenceKnown: true, Device: "/dev/ttyACM3", Paths: []string{"/dev/serial/by-id/board-one", "/dev/serial/by-id/board-one", "/dev/serial/by-path/hub-one"}}},
		{HardwareDevice: radio.HardwareDevice{Kind: "usb", Present: false, PresenceKnown: true, Device: "/dev/serial/by-id/unplugged"}},
		{HardwareDevice: radio.HardwareDevice{Kind: "gpio", Present: true, PresenceKnown: true, Device: "/dev/serial0"}},
	}}
	if got := webUSBChoices(inventory); !reflect.DeepEqual(got, []adminChoice{{ID: "/dev/serial/by-id/board-one"}}) {
		t.Fatal("unstable or absent choices", got)
	}
	c := integrationNewCluster(t)
	s := c.site.service
	before, _ := s.Store.Config()
	client := earlyWebLogin(t, c.site)
	earlyWebGet(t, client, c.site.web.URL+"/chooser?kind=usb", http.StatusOK)
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("device Refresh changed saved roles/configuration")
	}
	earlyWebGet(t, earlyWebLogin(t, c.system), c.system.web.URL+"/chooser?kind=usb", http.StatusNotFound)
}

func TestAdminAdvancedSavePreservesModemConfiguration(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	client := earlyWebLogin(t, c.site)
	address := c.site.web.URL + "/modems/edit?row=0"
	before, _ := s.Store.Config()
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "device")
	// Native collapsed details must never remove calibration controls from submission.
	if !form.Has("calibration.rx_offset_hz") || !form.Has("calibration.tx_level") {
		t.Fatal("Advanced omitted calibration")
	}
	earlyWebPost(t, client, address, form, c.site.web.URL, http.StatusSeeOther)
	after, _ := s.Store.Config()
	first, _ := webConfig(before)
	second, _ := webConfig(after)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("unchanged collapsed Advanced Save changed configuration")
	}
}

func TestAdminAgeLabelsRetainFullObservationTime(t *testing.T) {
	at := time.Date(2026, 11, 4, 18, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		age  time.Duration
		want string
	}{{30 * time.Second, "just now"}, {2 * time.Minute, "2 minutes ago"}, {3 * time.Hour, "3 hours ago"}, {48 * time.Hour, "2 days ago"}, {-2 * time.Minute, "in 2 minutes"}} {
		if got := webTimeAgo(at, at.Add(test.age)); got != test.want {
			t.Fatalf("%v: %s", test.age, got)
		}
	}
	var out bytes.Buffer
	webScalar(&out, "at", at.Format(time.RFC3339Nano))
	if !bytes.Contains(out.Bytes(), []byte(`title="2026-11-04 18:30:00 UTC"`)) || !bytes.Contains(out.Bytes(), []byte(`datetime="2026-11-04T18:30:00Z"`)) {
		t.Fatal("age label lost exact observation time")
	}
}
