package framework

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestPaddedNumericPresentationPreservesExactValues(t *testing.T) {
	for _, row := range []struct{ key, value, want string }{
		{"tgid", "1", "00001"}, {"talkgroup_id", "1001", "01001"},
		{"radio_id", "10001", "00010001"}, {"rid", "-1", "-00000001"},
		{"tgid", "123456", "123456"}, {"radio_id", "", ""},
		{"tx_frequency_hz", "434200000", "434.200000"},
		{"rx_frequency_hz", "1", "0.000001"},
		{"base_frequency_hz", "433450000.1", "433.4500001"},
		{"uplink_offset_hz", "5000000", "+5.000000"},
		{"uplink_offset_hz", "-5000000", "-5.000000"},
		{"uplink_offset_hz", "0", "+0.000000"},
		{"tx_frequency_hz", "Unavailable", "Unavailable"},
	} {
		if got := webUnitText(row.key, json.Number(row.value)); got != row.want {
			t.Errorf("%s %q = %q, want %q", row.key, row.value, got, row.want)
		}
	}
}

func TestModemChooserBlocksOtherAssignmentsAndRetainsCurrentOwner(t *testing.T) {
	c := integrationNewCluster(t)
	site := storeFixtureSite()
	site.Modems = append(site.Modems, Modem{ID: "unused", Name: "Unused"})
	site.Channels[0].Enabled = false
	if err := c.site.service.Store.SaveConfig(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	before, _ := c.site.service.Store.Config()
	for _, owner := range []string{"", "channel-1"} {
		response := httptest.NewRecorder()
		c.site.service.webChooser(response, httptest.NewRequest("GET", "/chooser?kind=modems&owner="+owner, nil))
		var page adminChoicePage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatal(response.Body.String())
		}
		if len(page.Items) != 2 || page.Items[0].Disabled != (owner == "") || page.Items[1].Disabled {
			t.Fatal("modem assignment choice", page)
		}
		if owner == "" && !strings.Contains(page.Items[0].InUse, "Control") {
			t.Fatal("missing assignment explanation", page)
		}
	}
	// Duplicate record IDs still identify distinct editor occurrences.
	site.Channels = append(site.Channels, Channel{ID: "channel-1", Name: "Second record", ModemID: "unused"})
	if err := c.site.service.Store.SaveConfig(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	for _, occurrence := range []string{"0", "1"} {
		response := httptest.NewRecorder()
		c.site.service.webChooser(response, httptest.NewRequest("GET", "/chooser?kind=modems&owner=channel-1&owner_occurrence="+occurrence, nil))
		var page adminChoicePage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatal(response.Body.String())
		}
		if page.Items[0].Disabled != (occurrence == "1") || page.Items[1].Disabled != (occurrence == "0") {
			t.Fatal("chooser exempted another record with the same ID", page)
		}
	}
	if err := c.site.service.Store.SaveConfig(before); err != nil {
		t.Fatal(err)
	}
	after, _ := c.site.service.Store.Config()
	if string(before) != string(after) {
		t.Fatal("chooser changed saved configuration")
	}
}

func TestUSBChooserBlocksSameHostAssignmentsAcrossSites(t *testing.T) {
	inventory := HardwareInventoryView{Devices: []HardwareDeviceView{{
		HardwareDevice: radio.HardwareDevice{Kind: "usb", Present: true, PresenceKnown: true, Device: "/dev/serial/by-id/example", Paths: []string{"/dev/serial/by-id/alias"}},
		Assignments:    []HardwareAssignmentView{{SiteID: "site-1", ModemID: "modem-1", ModemName: "Own modem", Local: true}},
	}}}
	if choices := webUSBChoices(inventory); len(choices) != 2 || !choices[0].Disabled || !choices[1].Disabled {
		t.Fatal("assigned device aliases must be blocked", choices)
	}
	if choices := webUSBChoices(inventory, "modem-1"); choices[0].Disabled {
		t.Fatal("current owner must retain its device", choices)
	}
	inventory.Devices[0].Assignments = append(inventory.Devices[0].Assignments, HardwareAssignmentView{SiteID: "site-2", ModemID: "modem-1", ModemName: "Other Site modem", SiteName: "Test site 2", Stale: true})
	choices := webUSBChoices(inventory, "modem-1")
	if !choices[0].Disabled || !strings.Contains(choices[0].InUse, "Test site 2") || !strings.Contains(choices[0].InUse, "last reported") {
		t.Fatal("remote Site ownership must remain distinct", choices)
	}
	inventory.Devices[0].Assignments = []HardwareAssignmentView{
		{ModemID: "modem-1", Local: true, ModemRecordIndex: 0},
		{ModemID: "modem-1", Local: true, ModemRecordIndex: 1},
	}
	if choices := webUSBChoices(inventory, "modem-1", "0"); !choices[0].Disabled {
		t.Fatal("another USB owner with the same record ID must remain occupied", choices)
	}
}
