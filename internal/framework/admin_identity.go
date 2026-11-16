package framework

import (
	"crypto/rand"
	"fmt"
	"io"
	"strings"
)

func adminInternalRecord(kind string) bool {
	switch kind {
	case "channels", "modems", "positions", "users", "automations":
		return true
	}
	return false
}

func adminManagedID(kind, field string) bool {
	return field == "id" && (adminInternalRecord(kind) || kind == "system" || kind == "zone" || kind == "site-identity")
}

func webNewRecord(kind string) map[string]any {
	record := webRecordDefaults(kind)
	if adminInternalRecord(kind) {
		record["id"] = strings.ToLower(rand.Text())
	}
	return record
}

// Names are presentation projections. Stable keys remain the stored references.
func (s *Service) adminNames() map[string]map[string]string {
	names := map[string]map[string]string{}
	for _, kind := range []string{"systems", "zones", "sites", "neighbor-sites", "modems", "channels", "positions", "users", "instances", "site-instances"} {
		names[kind] = map[string]string{}
	}
	view := s.view()
	for _, p := range view.System.ConsolePositions {
		names["positions"][p.ID] = p.Name
	}
	if s.Config().Role == RoleSite {
		for _, site := range s.neighborDirectory() {
			names["neighbor-sites"][site.ID] = site.Name
		}
		var site SiteConfig
		if s.owned(&site) == nil {
			for _, m := range site.Modems {
				names["modems"][m.ID] = m.Name
			}
			for _, c := range site.Channels {
				label := c.Name
				if label == "" {
					label = fmt.Sprintf("RF channel %d", c.ChannelNo)
				}
				names["channels"][c.ID] = label
			}
		}
	}
	for _, instance := range s.adminInstances() {
		switch instance.Role {
		case RoleSystem:
			names["systems"][instance.ID] = instance.Name
		case RoleZone:
			names["zones"][instance.ID] = instance.Name
		case RoleSite:
			names["sites"][instance.ID] = instance.Name
			names["site-instances"][instance.ID] = instance.Name
		}
		names["instances"][instance.ID] = instance.Name
	}
	return names
}

func adminName(names map[string]map[string]string, kind, id string) string {
	if name := names[kind][id]; name != "" {
		return name
	}
	if id == "" {
		return ""
	}
	label := map[string]string{"systems": "System", "zones": "Zone", "sites": "Site", "neighbor-sites": "Site", "modems": "USB modem", "channels": "RF channel", "positions": "console position", "users": "login account", "instances": "administration instance", "site-instances": "Site controller"}[kind]
	if label == "" {
		label = "entry"
	}
	return "Unnamed " + label
}

func (s *Service) adminNamedPresentation(value any) any {
	names := s.adminNames()
	var project func(any) any
	project = func(value any) any {
		switch row := value.(type) {
		case []any:
			for i := range row {
				row[i] = project(row[i])
			}
		case map[string]any:
			for key, item := range row {
				kind := map[string]string{"site_id": "sites", "system_id": "systems", "zone_id": "zones", "modem_id": "modems", "channel_id": "channels", "instance_id": "instances"}[key]
				if text, ok := item.(string); ok && kind != "" {
					row[key] = adminName(names, kind, text)
				} else {
					row[key] = project(item)
				}
			}
		}
		return value
	}
	return project(webPresentationValue(value))
}

func (s *Service) webTable(w io.Writer, columns []webColumn, value any) {
	webTable(w, columns, s.adminNamedPresentation(value))
}
func (s *Service) webProperties(w io.Writer, value any, columns []webColumn) {
	webProperties(w, s.adminNamedPresentation(value), columns)
}
