package framework

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSystemSignalLabelCatalogsSurviveStoreAndCacheRoundTrip(t *testing.T) {
	system := storeFixtureSystem()
	system.StatusLabels = []SignalLabel{{ID: 1, Label: "Available"}, {ID: 258, Label: "En route"}, {ID: 65535, Label: "Unavailable"}}
	system.MessageLabels = []SignalLabel{{ID: 1, Label: "Acknowledged"}}
	store := storeOpen(t, RoleSystem)
	if err := store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Config()
	if err != nil {
		t.Fatal(err)
	}
	site := storeOpen(t, RoleSite)
	if err = site.WriteCache("system", raw); err != nil {
		t.Fatal(err)
	}
	cached, _, err := site.ReadCache("system")
	if err != nil {
		t.Fatal(err)
	}
	var actual SystemConfig
	if err = json.Unmarshal(cached, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual.StatusLabels, system.StatusLabels) || !reflect.DeepEqual(actual.MessageLabels, system.MessageLabels) {
		t.Fatal("System catalog metadata changed during persistence or caching")
	}
}

func TestSystemSignalLabelCatalogsDistributeWithoutSiteTalkgroupScoping(t *testing.T) {
	cluster := integrationNewCluster(t)
	var system SystemConfig
	if err := cluster.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	system.StatusLabels = []SignalLabel{{ID: 0, Label: "Available"}, {ID: 258, Label: "En route"}, {ID: 65535, Label: "Unavailable"}}
	system.MessageLabels = []SignalLabel{{ID: 0, Label: "Acknowledged"}, {ID: 65535, Label: "Maximum"}}
	if err := cluster.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, cluster)
	for _, service := range []*integrationService{cluster.zone, cluster.site, cluster.otherSite, cluster.dispatch} {
		var received SystemConfig
		if !service.service.readCache("system", &received) {
			t.Fatal("System configuration was not distributed")
		}
		if !reflect.DeepEqual(received.StatusLabels, system.StatusLabels) || !reflect.DeepEqual(received.MessageLabels, system.MessageLabels) {
			t.Fatalf("%s altered System-wide display catalogs", service.service.Config().InstanceID)
		}
		if received.ID != system.ID || received.Name != system.Name || received.WACN != system.WACN || received.SystemID != system.SystemID || received.Callsign != system.Callsign || !reflect.DeepEqual(received.Radios, system.Radios) {
			t.Fatal("display catalog distribution changed other System metadata")
		}
		if service.service.Config().Role == RoleSite {
			blocked := 1003
			if service == cluster.otherSite {
				blocked = 1002
			}
			for _, group := range received.Talkgroups {
				if group.TGID == blocked {
					t.Fatal("System-wide display catalogs bypassed Site talkgroup scoping")
				}
			}
		}
	}
}

func TestInitialConfigurationKeepsExplicitSignalLabelCatalogs(t *testing.T) {
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	wantedStatuses := []SignalLabel{{ID: 1, Label: "READY"}, {ID: 2, Label: "BUSY"}}
	wantedMessages := []SignalLabel{{ID: 1, Label: "TESTMSG"}}
	if !reflect.DeepEqual(seed.System.StatusLabels, wantedStatuses) || !reflect.DeepEqual(seed.System.MessageLabels, wantedMessages) {
		t.Fatal("initial configuration changed its display catalogs")
	}
	for _, empty := range []bool{false, true} {
		document := initialFixture(t)
		sourceSystem := document["system"].(map[string]any)
		statuses := []SignalLabel{{ID: 7, Label: "  Custom status  "}}
		messages := []SignalLabel{{ID: 8, Label: "  Custom message  "}}
		if empty {
			statuses, messages = []SignalLabel{}, []SignalLabel{}
		}
		sourceSystem["status_labels"], sourceSystem["message_labels"] = statuses, messages
		seed, err = decodeFixture(t, document)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(seed.System.StatusLabels, statuses) || !reflect.DeepEqual(seed.System.MessageLabels, messages) {
			t.Fatal("explicit source catalogs were changed")
		}
	}
	document := initialFixture(t)
	sourceSystem := document["system"].(map[string]any)
	sourceSystem["status_labels"] = []SignalLabel{{ID: 1, Label: "First"}, {ID: 1, Label: "Second"}}
	if _, err = decodeFixture(t, document); err != nil {
		t.Fatal("duplicate display label was rejected:", err)
	}
}
