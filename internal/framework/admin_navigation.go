package framework

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"open25.local/open25-fne/internal/frameworkui"
)

type adminLink struct{ Path, Label string }
type adminCategory struct {
	Label string
	Links []adminLink
}
type adminInstance struct {
	ID, Name, URL, Icon string
	Role                Role
	Level               int
	Current             bool
}

func adminIcon(name string) string { return frameworkui.Icon(name) }
func adminRoleIcon(role Role) string {
	return map[Role]string{RoleSystem: "network", RoleZone: "git-branch", RoleSite: "radio-tower", RoleApplication: "headphones"}[role]
}
func adminTier(role Role) string {
	return map[Role]string{RoleSystem: "System", RoleZone: "Zone", RoleSite: "Site", RoleApplication: "Application Server"}[role]
}
func adminRoleName(role Role) string {
	return map[Role]string{RoleSystem: "Open25 System Administration", RoleZone: "Open25 Zone Administration", RoleSite: "Open25 Site Administration", RoleApplication: "Open25 Application Server Administration"}[role]
}
func adminURL(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return strings.TrimRight(address, "/") + "/"
}
func (s *Service) adminInstances() []adminInstance {
	c := s.Config()
	var system struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if c.Role == RoleSystem {
		if raw, err := s.Store.Config(); err == nil {
			_ = json.Unmarshal(raw, &system)
		}
	} else {
		s.readCache("system", &system)
	}
	zone := s.webKnownZone()
	tree := s.adminTopology(zone)
	systemName, systemID := system.Name, system.ID
	if systemName == "" {
		systemName = "System"
	}
	if systemID == "" {
		systemID = "system"
	}
	zoneName, zoneID := "Zone", "zone"
	if zone != nil {
		zoneName, zoneID = zone.Name, zone.ID
		if zoneName == "" {
			zoneName = zoneID
		}
	}
	nodes := []adminInstance{{ID: systemID, Name: systemName, URL: tree.SystemURL, Icon: "network", Role: RoleSystem}, {ID: zoneID, Name: zoneName, URL: tree.ZoneURL, Icon: "git-branch", Role: RoleZone, Level: 1}}
	ids := map[string]string{}
	for id, address := range tree.SiteURLs {
		ids[id] = address
	}
	if c.Role == RoleSite {
		ids[c.InstanceID] = c.PublicURL
	}
	names := map[string]string{}
	if zone != nil {
		for _, v := range zone.Sites {
			if v.Name != "" {
				names[v.ID] = v.Name
			}
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		name := id
		if names[id] != "" {
			name = names[id]
		}
		nodes = append(nodes, adminInstance{ID: id, Name: name, URL: ids[id], Icon: "radio-tower", Role: RoleSite, Level: 2})
	}
	appName, appID := "Application Server", "application"
	if c.Role == RoleApplication {
		var app ApplicationConfig
		if s.owned(&app) == nil && app.Name != "" {
			appName = app.Name
		}
		appID = c.InstanceID
	}
	nodes = append(nodes, adminInstance{ID: appID, Name: appName, URL: tree.ApplicationURL, Icon: "headphones", Role: RoleApplication, Level: 1})
	for i := range nodes {
		nodes[i].Current = nodes[i].Role == c.Role && (c.Role != RoleSite || nodes[i].ID == c.InstanceID)
		nodes[i].URL = adminURL(nodes[i].URL)
		if nodes[i].Current {
			nodes[i].ID = c.InstanceID
			nodes[i].URL = "/"
		}
	}
	return nodes
}

var adminPrimaryPages = map[Role][]string{
	RoleSystem:      {"system", "talkgroups", "radios", "users", "profiles", "keys"},
	RoleZone:        {"zone", "sites", "identifiers"},
	RoleSite:        {"channels", "modems", "site-identity"},
	RoleApplication: {"application-settings"},
}

func adminConfiguration(role Role) []adminCategory {
	service := adminCategory{"Service", []adminLink{{"networking", "Connections"}, {"history-settings", "History storage"}, {"account", "Administrator password"}}}
	switch role {
	case RoleSystem:
		return []adminCategory{
			{"System", []adminLink{{"system", "System settings"}}},
			{"Radio programming", []adminLink{{"talkgroups", "Talkgroups"}, {"radios", "Radios"}, {"status-labels", "Status meanings"}, {"message-labels", "Radio message meanings"}}},
			{"People & applications", []adminLink{{"positions", "Console positions"}, {"users", "User accounts"}, {"profiles", "User profiles"}, {"automations", "Automation clients"}}},
			{"Security", []adminLink{{"keys", "Encryption keys"}, {"recipients", "Application key enrollment"}}},
			service}
	case RoleZone:
		return []adminCategory{{"Site programming", []adminLink{{"zone", "Zone identity"}, {"identifiers", "Band Plan"}, {"sites", "Site registry"}}}, service}
	case RoleSite:
		return []adminCategory{{"Radio setup", []adminLink{{"modems", "USB modems"}, {"channels", "RF channels"}, {"control-policy", "Control channel behavior"}, {"identification", "Station identification"}, {"site-identity", "Local Site identity"}}}, service}
	default:
		return []adminCategory{{"Application", []adminLink{{"application-settings", "Application Server"}}}, service}
	}
}
func adminCategories(role Role, group string) []adminCategory {
	switch group {
	case "configuration":
		return adminConfiguration(role)
	case "status":
		links := []adminLink{{"alarms", "Alarms"}, {"connections", "Service connections"}}
		if role == RoleSite || role == RoleZone {
			links = append(links, adminLink{"live-status", "Radio status"})
		}
		if role == RoleSystem || role == RoleZone {
			links = append(links, adminLink{"reports", "Reported Sites"}, adminLink{"modem-status", "Modem observations"})
		}
		if role == RoleSystem {
			links = append(links, adminLink{"zone-status", "Reported Zone"})
		}
		if role == RoleSite {
			links = append(links, adminLink{"modem-status", "Modem observations"}, adminLink{"hardware", "Hardware observations"})
		}
		if role == RoleSystem || role == RoleApplication {
			links = append(links, adminLink{"enrollment-status", "Key enrollment status"})
		}
		links = append(links, adminLink{"alarm-history", "Alarm history"}, adminLink{"notification-delivery", "Notification delivery"}, adminLink{"activity-status", "Recent activity"}, adminLink{"history", "History"})
		return []adminCategory{{"Status & records", links}}
	case "notifications":
		return []adminCategory{{"Notifications", []adminLink{{"alarm-settings", "Alarm settings"}, {"notification-rules", "Notification rules"}, {"smtp-settings", "SMTP settings"}}}}
	case "operations":
		links := []adminLink{}
		if role == RoleSite || role == RoleZone {
			links = append(links, adminLink{"radio-commands", "Radio commands"})
		}
		if role == RoleSite {
			links = append(links, adminLink{"radio-operation", "Channel operation"}, adminLink{"c4fm-test", "C4FM transmitter test"})
		}
		if role == RoleZone || role == RoleSite {
			links = append(links, adminLink{"command-results", "Command results"})
		}
		links = append(links, adminLink{"alarm-actions", "Alarm acknowledgment"}, adminLink{"notification-actions", "Email test and retry"})
		if role == RoleApplication {
			links = append(links, adminLink{"dispatch-test", "Open25 Dispatch"}, adminLink{"ptt-test", "Open25 PTT"})
		}
		return []adminCategory{{"Explicit actions", links}}
	case "reference":
		links := []adminLink{}
		if role != RoleSystem {
			links = append(links, adminLink{"reference-system", "System programming"})
		}
		if role == RoleSite || role == RoleApplication {
			links = append(links, adminLink{"reference-zone", "Zone programming"})
		}
		links = append(links, adminLink{"installation", "Host installation"}, adminLink{"planned", "Feature coverage"})
		return []adminCategory{{"Reference", links}}
	}
	return nil
}
func adminPageGroup(path string) string {
	path = strings.TrimSuffix(path, "/edit")
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		for _, group := range []string{"status", "operations", "notifications", "reference"} {
			for _, category := range adminCategories(role, group) {
				for _, link := range category.Links {
					if path == link.Path {
						if path == "command-results" {
							return "status"
						}
						return group
					}
				}
			}
		}
	}
	if path == "" || path == "home" || path == "dashboard" || path == "status" {
		return "dashboard"
	}
	return "configuration"
}

var adminIcons = map[string]string{"radios": "radio", "talkgroups": "users-round", "users": "user-round", "profiles": "sliders-horizontal", "positions": "headphones", "automations": "workflow", "keys": "key-round", "recipients": "fingerprint", "sites": "radio-tower", "identifiers": "list-ordered", "modems": "usb", "channels": "radio", "control-policy": "repeat-2", "identification": "badge-info", "system": "network", "zone": "git-branch", "site-identity": "map-pin", "application-settings": "headphones", "networking": "plug", "history-settings": "database", "account": "lock-keyhole", "radio-commands": "send", "radio-operation": "power", "c4fm-test": "audio-lines", "zone-status": "git-branch", "reference-system": "network", "reference-zone": "git-branch", "history": "history", "status-labels": "list", "message-labels": "message-square", "alarms": "bell", "alarm-settings": "bell-ring", "notification-rules": "list-filter", "smtp-settings": "mail", "notification-delivery": "mail", "alarm-history": "history", "alarm-actions": "check-check", "notification-actions": "mail-check", "connections": "plug", "reports": "radio-tower", "modem-status": "usb", "hardware": "usb", "enrollment-status": "fingerprint", "activity-status": "list", "live-status": "radio", "installation": "server", "planned": "list-checks"}

func adminLabel(role Role, path, fallback string) string {
	for _, group := range []string{"configuration", "notifications", "status", "operations", "reference"} {
		for _, c := range adminCategories(role, group) {
			for _, link := range c.Links {
				if link.Path == path {
					return link.Label
				}
			}
		}
	}
	return fallback
}
func (s *Service) webHeader(w http.ResponseWriter, r *http.Request, title string, session Session, live bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	c := s.Config()
	path := strings.Trim(r.URL.Path, "/")
	base := strings.TrimSuffix(strings.TrimSuffix(path, "/edit"), "/bulk")
	editing := strings.HasSuffix(path, "/edit") || strings.HasSuffix(path, "/bulk")
	group := adminPageGroup(base)
	if !editing {
		title = adminLabel(c.Role, base, title)
	}
	if group == "dashboard" {
		title = "Overview"
	}
	fmt.Fprintf(w, `<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><title>%s — %s</title><link rel=stylesheet href=/admin.css><script src=/forms.js defer></script><script src=/admin.js defer></script><script src=/admin-chooser.js defer></script>`, esc(adminRoleName(c.Role)), esc(title))
	if live {
		fmt.Fprint(w, `<meta http-equiv=refresh content=5>`)
	}
	fmt.Fprintf(w, `<body class=administration><div id=open25-administration><div class=ts-admin><header class=ts-topbar><div class=ts-brand>%sOpen25</div><div class=ts-product>%s<strong>%s</strong></div>`, adminIcon("radio-tower"), adminIcon(adminRoleIcon(c.Role)), esc(strings.TrimPrefix(adminRoleName(c.Role), "Open25 ")))
	if session.Username != "" {
		fmt.Fprintf(w, `<div class=ts-account><button type=button data-admin-theme>Theme</button><span>%s</span><form method=post action=/logout>`, esc(session.Username))
		webHidden(w, session.CSRF)
		fmt.Fprint(w, `<button>Sign out</button></form></div>`)
	}
	fmt.Fprint(w, `</header>`)
	if session.Username == "" {
		fmt.Fprintf(w, `<div class="ts-layout ts-login-layout"><main id=admin-content class=ts-main><section class=ts-page><div class=ts-heading><h1>%s</h1></div>`, esc(title))
		return
	}
	nodes := s.adminInstances()
	current := adminInstance{ID: c.InstanceID, Name: c.InstanceID, Role: c.Role}
	for _, n := range nodes {
		if n.Current {
			current = n
		}
	}
	fmt.Fprint(w, `<div class=ts-contextbar><div class=ts-actions><input type=hidden id=admin-instance-choice value=""><button type=button data-chooser-kind=instances data-chooser-target=admin-instance-choice data-chooser-navigate>Choose administration instance</button></div>`)
	fmt.Fprintf(w, `<form method=get action=/find-setting class=ts-search role=search><input type=search name=q placeholder="Find a setting…" aria-label="Find a setting across administration"></form></div><div class=ts-layout><aside class=ts-sidebar><nav aria-label="%s navigation"><div class=ts-treeowner>Network</div><ul class=ts-instance-tree>`, esc(adminRoleName(c.Role)))
	nodes = adminVisibleInstances(nodes)

	depth := 0
	for i, n := range nodes {
		if i > 0 {
			if n.Level > depth {
				fmt.Fprint(w, `<ul>`)
			} else {
				fmt.Fprint(w, `</li>`)
				for depth > n.Level {
					fmt.Fprint(w, `</ul></li>`)
					depth--
				}
			}
		}
		depth = n.Level
		fmt.Fprint(w, `<li class=ts-treeitem>`)
		if n.URL == "" {
			fmt.Fprintf(w, `<span class=ts-unavailable>%s<span>%s</span></span>`, adminIcon(n.Icon), esc(adminTier(n.Role)+": "+n.Name))
		} else {
			selected := ""
			if n.Current {
				selected = " aria-current=location"
			}
			outside := ""
			if !n.Current {
				outside = adminPortalIcon()
			}
			fmt.Fprintf(w, `<a href="%s"%s>%s<span>%s</span>%s</a>`, esc(n.URL), selected, adminIcon(n.Icon), esc(adminTier(n.Role)+": "+n.Name), outside)
		}
	}
	fmt.Fprint(w, `</li>`)
	for depth > 0 {
		fmt.Fprint(w, `</ul></li>`)
		depth--
	}
	fmt.Fprint(w, `</ul>`)
	fmt.Fprintf(w, `<div class=ts-navgroup>%s</div>`, esc(current.Name))
	nav := func(link adminLink) {
		selected := ""
		if base == link.Path || (base == "" && link.Path == "dashboard") {
			selected = " aria-current=page"
		}
		icon := adminIcons[link.Path]
		if link.Path == "dashboard" {
			icon = "layout-dashboard"
		}
		fmt.Fprintf(w, `<a href="/%s"%s>%s<span>%s</span></a>`, esc(link.Path), selected, adminIcon(icon), esc(link.Label))
	}
	nav(adminLink{"dashboard", "Overview"})
	for _, section := range []string{"configuration", "status", "notifications", "operations", "reference"} {
		folded := section != "configuration"
		if folded {
			open := ""
			if group == section {
				open = " open"
			}
			label := map[string]string{"status": "Status & troubleshooting", "notifications": "Alarms & email", "operations": "Operations", "reference": "Reference"}[section]
			fmt.Fprintf(w, `<details class=ts-navbranch%s><summary>%s%s</summary>`, open, adminIcon("chevron-down"), esc(label))
		} else {
			fmt.Fprint(w, `<div class=ts-navgroup>Configuration</div>`)
			for _, path := range adminPrimaryPages[c.Role] {
				nav(adminLink{path, adminLabel(c.Role, path, path)})
			}
		}
		for _, category := range adminCategories(c.Role, section) {
			if folded {
				for _, link := range category.Links {
					nav(link)
				}
			} else {
				remaining := []adminLink{}
				open := ""
				for _, link := range category.Links {
					if !slices.Contains(adminPrimaryPages[c.Role], link.Path) {
						remaining = append(remaining, link)
						if base == link.Path {
							open = " open"
						}
					}
				}
				if len(remaining) > 0 {
					fmt.Fprintf(w, `<details class=ts-navbranch%s><summary>%s%s</summary>`, open, adminIcon("chevron-down"), esc(category.Label))
					for _, link := range remaining {
						nav(link)
					}
					fmt.Fprint(w, `</details>`)
				}
			}
		}
		if folded {
			fmt.Fprint(w, `</details>`)
		}
	}
	fmt.Fprint(w, `</nav></aside><main id=admin-content class=ts-main><header class=ts-pageheader><nav class=ts-breadcrumb aria-label=Breadcrumb>`)
	first := true
	for _, n := range nodes {
		if !n.Current && n.Role != RoleSystem && !(c.Role == RoleSite && n.Role == RoleZone) {
			continue
		}
		if !first {
			fmt.Fprint(w, `<span aria-hidden=true>/</span>`)
		}
		first = false
		label := adminTier(n.Role) + ": " + n.Name
		if n.Current || n.URL == "" {
			fmt.Fprintf(w, `<span>%s</span>`, esc(label))
		} else {
			fmt.Fprintf(w, `<a href="%s">%s%s</a>`, esc(n.URL), esc(label), adminPortalIcon())
		}
	}
	if editing {
		fmt.Fprintf(w, `<span aria-hidden=true>/</span><span>%s</span>`, esc(adminLabel(c.Role, base, base)))
	}
	fmt.Fprint(w, `</nav><div class=ts-heading><div class=ts-titleline>`)
	if editing {
		fmt.Fprintf(w, `<a class=ts-back href="/%s" aria-label="Back to %s">%s</a>`, esc(base), esc(adminLabel(c.Role, base, base)), adminIcon("arrow-left"))
	}
	fmt.Fprintf(w, `<h1>%s</h1></div>`, esc(title))

	fmt.Fprint(w, `<div class=ts-actions>`)
	collection := false
	for _, section := range webSections() {
		if section.Path == base && section.Collection != "" {
			collection = true
		}
	}
	if base == "users" || base == "profiles" || base == "automations" || base == "keys" || base == "recipients" {
		collection = true
	}
	if collection && !editing {
		target, label := base+"/edit", "Add "+strings.TrimPrefix(webRecordTitle(base, map[string]any{}), "New ")
		if base == "profiles" {
			target, label = "users/edit", "Add login account"
		}
		fmt.Fprintf(w, `<a class="ts-button ts-primary" href="/%s">%s%s</a>`, target, adminIcon("plus"), esc(label))
	}
	fmt.Fprintf(w, `</div></div></header><section class=ts-page data-page-group="%s">`, esc(group))
}
func webRecordTitle(kind string, record map[string]any) string {
	if kind == "profiles" {
		if p, ok := record["profile"].(map[string]any); ok {
			if name := webText(p["name"]); name != "" {
				return name
			}
		}
	}
	for _, key := range []string{"alias", "name", "username", "recipient"} {
		if name := webText(record[key]); name != "" {
			return name
		}
	}

	labels := map[string]string{"radios": "radio", "talkgroups": "talkgroup", "channels": "RF channel", "modems": "USB modem", "identifiers": "Band Plan entry", "sites": "Site", "users": "login account", "profiles": "saved operator profile", "positions": "console position", "keys": "traffic key", "recipients": "Application key enrollment", "automations": "automation client", "status-labels": "status meaning", "message-labels": "message meaning"}
	return "New " + labels[kind]
}
func webEditorFields(w io.Writer, kind string, fields []webField, record map[string]any, submitted ...url.Values) {
	spec := adminPageSpecs[kind]
	groups := spec.Sections
	if len(groups) == 0 {
		names := []string{}
		for _, f := range fields {
			names = append(names, f.Name)
		}
		groups = []adminFormSection{{Name: "Settings", Fields: names}}
	}
	fmt.Fprint(w, `<nav class=ts-editor-nav aria-label="Editor sections">`)
	for i, g := range groups {
		if !g.Advanced {
			fmt.Fprintf(w, `<a href="#section-%d">%s</a>`, i, esc(g.Name))
		}
	}
	for _, g := range groups {
		if g.Advanced {
			fmt.Fprint(w, `<a href="#advanced-settings">Advanced</a>`)
			break
		}
	}
	fmt.Fprint(w, `</nav>`)
	used := map[string]bool{}
	order := []int{}
	for i, g := range groups {
		if !g.Advanced {
			order = append(order, i)
		}
	}
	for i, g := range groups {
		if g.Advanced {
			order = append(order, i)
		}
	}
	advanced := false
	for _, i := range order {
		g := groups[i]
		if g.Advanced && !advanced {
			advanced = true
			fmt.Fprint(w, `<details class=ts-advanced id=advanced-settings><summary>`, adminIcon("chevron-down"), `Advanced</summary>`)
		}
		selected := []webField{}
		for _, name := range g.Fields {
			for _, f := range fields {
				if f.Name == name {
					selected = append(selected, f)
					used[name] = true
				}
			}
		}
		if len(selected) == 0 {
			continue
		}
		fmt.Fprintf(w, `<section class=ts-formsection id=section-%d><div class=ts-sectiontitle><h2>%s</h2></div>`, i, esc(g.Name))
		webFields(w, selected, record, submitted...)
		if kind == "talkgroups" && g.Name == "Encryption" {
			fmt.Fprint(w, `<div class=ts-reference><div class=ts-itemline><strong>Receiving</strong></div>Permitted clear calls play with a clear indication. Encrypted calls use the loaded receive key identified by the call.</div>`)
		}
		fmt.Fprint(w, `</section>`)
	}
	if advanced {
		fmt.Fprint(w, `</details>`)
	}
	rest := []webField{}
	for _, f := range fields {
		if !used[f.Name] {
			rest = append(rest, f)
		}
	}
	if len(rest) > 0 {
		fmt.Fprint(w, `<section class=ts-formsection><h2>Additional settings</h2>`)
		webFields(w, rest, record, submitted...)
		fmt.Fprint(w, `</section>`)
	}
}

// Keep the current ancestry visible without expanding a large Site directory.
func adminVisibleInstances(nodes []adminInstance) []adminInstance {
	out := []adminInstance{}
	sites := 0
	for _, n := range nodes {
		if n.Role == RoleSite {
			sites++
			if sites > 5 && !n.Current {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}
