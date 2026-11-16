package framework

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Administrative pages read once when opened and submit ordinary forms.
func (s *Service) webAdmin(w http.ResponseWriter, r *http.Request, session Session, path string) bool {
	role := s.Config().Role
	if r.Method == http.MethodPost {
		switch path {
		case "", "home", "dashboard", "configuration", "operations", "reference", "connections", "topology", "reports", "status", "zone-status", "reference-system", "reference-zone", "live-status", "modem-status", "hardware", "history", "command-results", "enrollment-status", "activity-status", "planned", "installation", "find-setting":
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
	}
	switch path {
	case "chooser":
		s.webChooser(w, r)
		return true

	case "alarms", "alarm-history", "notification-delivery", "alarm-settings", "notification-rules", "smtp-settings", "alarm-actions", "notification-actions":
		s.webNotifications(w, r, session, path)
		return true
	case "installation":
		s.webInstallation(w, r, session)
		return true
	case "find-setting":
		s.webFindSetting(w, r, session)
		return true

	case "", "home", "dashboard":
		s.webDashboard(w, r, session)
		return true
	case "configuration", "operations", "reference":
		s.webSectionOverview(w, r, session, path)
		return true
	case "networking":
		fields := []webField{
			{Name: "public_url", Label: "Web administration address", Kind: "text", Help: "Address used to open this service in a browser."},
			{Name: "internal_url", Label: "Internal service address", Kind: "text", Help: "Address used by the other services over the internal VPN."},
			{Name: "upstream_url", Label: "Upstream controller address", Kind: "text"},
			{Name: "poll_seconds", Label: "Settings synchronization interval (seconds)", Kind: "number"},
		}
		record := webObject(s.Config().Settings())
		s.webSingleForm(w, r, session, "Networking", fields, record, "/api/settings", "PUT")
		return true
	case "account":
		var accountResult *webResult
		if r.Method == "POST" && role == RoleSystem {
			accountResult = s.webAPI(r, session, "/api/account", "PUT", map[string]string{"current_password": r.FormValue("current_password"), "new_password": r.FormValue("new_password")})
			if accountResult.code < 300 {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return true
			}
		}
		s.webHeader(w, r, "Administrator Account", session, false)
		defer webFooter(w)
		if role != RoleSystem {
			webSourceTag(w, "System")
			tree := s.adminTopology(s.webKnownZone())
			fmt.Fprint(w, "<p>")
			s.webAdminAddress(w, tree.SystemURL+"/account", "Open System Administration")
			fmt.Fprint(w, "</p>")
			return true
		}
		fields := []webField{{Name: "current_password", Label: "Current password", Kind: "password"}, {Name: "new_password", Label: "New password", Kind: "password"}}
		record := map[string]any{}
		if accountResult != nil {
			webJSONResult(w, accountResult)
		}
		fmt.Fprint(w, "<section>")
		s.webForm(w, session, "account", fields, record)
		fmt.Fprint(w, "</section>")
		return true
	case "users", "users/edit", "profiles", "profiles/edit", "automations", "automations/edit", "keys", "keys/edit", "recipients", "recipients/edit":
		if role != RoleSystem {
			return false
		}
		s.webDirectoryPage(w, r, session, path)
		return true
	case "history":
		if role != RoleSystem {
			return false
		}
		s.webHistoryPage(w, r, session)
		return true
	case "history-settings":
		if role != RoleSystem {
			return false
		}
		s.webHistorySettings(w, r, session)
		return true
	case "topology":
		s.webHeader(w, r, "Administration Interfaces", session, false)
		defer webFooter(w)
		s.webInterfaces(w)
		return true
	case "reports":
		s.webSiteReports(w, r, session)
		return true
	case "status":
		s.webDashboard(w, r, session)
		return true
	case "connections":
		if role == RoleSite {
			s.webSiteStatus(w, r, session)
			return true
		}
		s.webLocalStatus(w, r, session, false)
		return true
	case "activity-status":
		if role == RoleSystem {
			return false
		}
		s.webActivityStatus(w, r, session)
		return true

	case "planned":
		s.webHeader(w, r, "Planned features", session, false)
		defer webFooter(w)
		webSitePanel(w, "Infrastructure alarms", "")
		fmt.Fprint(w, `<div class=table-wrap><table class=record-table><thead><tr><th>Feature</th><th>Implementation</th></tr></thead><tbody>`)
		for _, link := range []adminLink{{"alarms", "Current alarms"}, {"alarm-history", "Alarm history"}, {"alarm-settings", "Alarm settings"}, {"notification-rules", "Notification rules"}, {"alarm-actions", "Alarm acknowledgment"}} {
			fmt.Fprintf(w, `<tr><td><a href="/%s">%s</a></td><td>Planned</td></tr>`, link.Path, link.Label)
		}
		fmt.Fprint(w, `</tbody></table></div></section>`)
		webFeatureCoverage(w)
		return true
	case "zone-status":
		if role != RoleSystem {
			return false
		}
		s.webZoneStatus(w, r, session)
		return true
	case "enrollment-status":
		if role != RoleSystem && role != RoleApplication {
			return false
		}
		s.webHeader(w, r, "Public Enrollment Identity", session, false)
		defer webFooter(w)

		result := s.webAPI(r, session, "/api/crypto/identity", "GET", nil)
		if result.code >= 300 {
			webJSONResult(w, result)
		} else {
			value, _ := webConfig(result.Bytes())
			webProperties(w, value, []webColumn{{"system_id", "System record"}, {"recipient", "Application Server recipient"}, {"public_key", "Public enrollment key"}})
		}
		return true
	case "reference-system", "reference-zone":
		if path == "reference-zone" && (role == RoleSystem || role == RoleZone) {
			return false
		}
		s.webReference(w, r, session, path == "reference-zone")
		return true
	case "live-status":
		if role != RoleZone && role != RoleSite {
			return false
		}
		if role == RoleSite {
			s.webSiteStatus(w, r, session)
		} else {
			s.webSiteReports(w, r, session)
		}
		return true
	case "modem-status":
		if role != RoleSite && role != RoleZone && role != RoleSystem {
			return false
		}
		if role == RoleSite {
			http.Redirect(w, r, "/hardware", http.StatusSeeOther)
			return true
		}
		s.webModemStatus(w, r, session)
		return true
	case "hardware":
		if role != RoleSite {
			return false
		}
		s.webHardware(w, r, session)
		return true
	case "radio-commands":
		if role != RoleZone && role != RoleSite {
			return false
		}
		s.webHeader(w, r, "Radio Commands", session, false)
		defer webFooter(w)
		fields := []webField{
			{Name: "action", Label: "Command", Kind: "select", Choices: "radio_check=Radio check|page=Call alert|message=Send coded message|registration_demand=Request registration|affiliation_query=Request group affiliation|status_query=Request radio status|inhibit=Disable radio|uninhibit=Enable radio"},
			{Name: "radio_id", Label: "Radio / Unit ID (decimal)", Kind: "number"},
			{Name: "code", Label: "Message code (decimal)", Kind: "number", Help: "Numeric code for Send coded message. The radio uses its own programmed message table; editing a System label does not program the radio. Other commands do not use this field."},
			{Name: "site_id", Label: "Site record ID", Kind: "text", Help: "Names the Site that should send the command."},
		}
		record := map[string]any{}
		if r.Method == "POST" {
			if err := webReadFields(r, fields, record); err != nil {
				webError(w, err)
			} else {
				if record["action"] == "message" && r.FormValue("code") == "" && r.FormValue("code__hex") == "" {
					delete(record, "code")
				}
				webJSONResult(w, s.webAPI(r, session, "/api/radio/command", "POST", record))
			}
		}
		fmt.Fprint(w, "<form method=post>")
		webHidden(w, session.CSRF)
		var submitted url.Values
		if r.Method == http.MethodPost {
			submitted = r.PostForm
		}
		s.adminEditorFields(w, "radio-commands", fields, record, submitted)
		fmt.Fprint(w, "<p><button>Send command</button></p></form>")
		fmt.Fprint(w, "<p><a href=/command-results>Command results</a></p>")
		return true
	case "command-results":
		if role != RoleZone && role != RoleSite {
			return false
		}
		s.webHeader(w, r, "Command Results", session, false)
		defer webFooter(w)

		webLoaded(w)
		s.webCommandResults(w, r, session)
		return true
	case "parent-zone":
		if role != RoleSite {
			return false
		}
		s.webParentLink(w, r, session)
		return true
	case "radio-operation":
		if role != RoleSite {
			return false
		}
		s.webSiteOperation(w, r, session)
		return true
	case "c4fm-test":
		if role != RoleSite {
			return false
		}
		s.webC4FMTest(w, r, session)
		return true
	}
	return false
}

func (s *Service) webForm(w io.Writer, session Session, kind string, fields []webField, record map[string]any, submitted ...url.Values) {
	fmt.Fprint(w, `<div class=ts-editor><form method=post data-config-form`)
	if len(submitted) > 0 && submitted[0] != nil {
		fmt.Fprint(w, ` data-admin-unsaved`)
	}
	fmt.Fprint(w, `>`)
	webHidden(w, session.CSRF)
	s.adminEditorFields(w, kind, fields, record, submitted...)
	webSaveBar(w, "/"+kind)
	fmt.Fprint(w, `</form></div>`)
}

func (s *Service) webSingleForm(w http.ResponseWriter, r *http.Request, session Session, title string, fields []webField, record map[string]any, api, method string) {
	var result *webResult
	var err error
	if r.Method == "POST" {
		err = webReadFields(r, fields, record)
		if err == nil {
			result = s.webAPI(r, session, api, method, record)
			if result.code < 300 {
				http.Redirect(w, r, r.URL.Path+"?saved=1", http.StatusSeeOther)
				return
			}
		}
	}
	s.webHeader(w, r, title, session, false)
	defer webFooter(w)
	if err != nil {
		webError(w, err)
	}
	if result != nil {
		webJSONResult(w, result)
	}
	if r.Method == http.MethodGet && r.URL.Query().Has("saved") {
		fmt.Fprint(w, "<p role=status>Settings saved.</p>")
	}
	fmt.Fprint(w, "<section>")
	var submitted url.Values
	if r.Method == http.MethodPost {
		submitted = r.PostForm
	}
	s.webForm(w, session, strings.Trim(r.URL.Path, "/"), fields, record, submitted)
	fmt.Fprint(w, "</section>")
}

func webDataField(w io.Writer, key string, value any) {
	if (key == "radio_id" || key == "talkgroup_id") && webText(value) == "0" {
		return
	}
	if _, ok := value.([]any); ok && webDecimalWidth(key) > 0 {
		fmt.Fprintf(w, `<span class=ts-numeric>%s</span>`, esc(webUnitText(key, value)))
		return
	}
	if _, places := webUnit(key); places != 0 && value != nil {
		fmt.Fprint(w, esc(webUnitText(key, value)))
		return
	}
	identifier := key
	if key == "talkgroup_id" || key == "talkgroup" || key == "affiliated_talkgroup" {
		identifier = "tgid"
	}
	numeric := false
	switch value.(type) {
	case json.Number, int, int32, int64, uint, uint32, uint64:
		numeric = true
	}
	if width := webHexWidth(webField{Name: identifier, Kind: "number"}); numeric && width > 0 {
		if notation := webHexText(webText(value), width); notation != "" {
			fmt.Fprintf(w, `<span class=ts-numeric>%s / %s hex</span>`, esc(webDecimalText(identifier, webText(value))), esc(notation))
			return
		}
	}
	webScalar(w, key, value)
}

func webLabel(key string) string {
	if key == "lra" {
		return "Location Registration Area (LRA)"
	}
	if key == "channel_identifiers" {
		return "Band Plan"
	}
	if key == "identifier" {
		return "Band Plan ID"
	}
	unit, _ := webUnit(key)
	if unit != "" {
		for _, suffix := range []string{"_hz", "_ms", "_seconds", "_percent", "_days"} {
			key = strings.TrimSuffix(key, suffix)
		}
	}
	words := strings.Split(key, "_")
	names := map[string]string{"id": "ID", "ids": "IDs", "rid": "Radio ID", "tgid": "Talkgroup ID", "rfss": "RFSS", "nac": "NAC", "wacn": "WACN", "url": "URL", "urls": "URLs", "rx": "RX", "tx": "TX", "rf": "RF", "hz": "Hz", "api": "API", "ptt": "PTT", "p25": "P25", "baud": "Serial Rate"}
	for i, word := range words {
		if label, ok := names[word]; ok {
			words[i] = label
		} else if word != "" {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	label := strings.Join(words, " ")
	if unit != "" {
		label += " (" + unit + ")"
	}
	return label
}

func webDirectoryFields(kind string) []webField {
	switch kind {
	case "users":
		return []webField{
			{Name: "id", Label: "User record ID", Kind: "text"}, {Name: "username", Label: "Username", Kind: "text"},
			{Name: "password", Label: "Password", Kind: "password", Help: "Enter a password for a new user. Leave blank to keep the current password."},
			{Name: "radio_id", Label: "Radio / Unit ID (decimal)", Kind: "number"}, {Name: "position_ids", Label: "Console position IDs (comma separated)", Kind: "strings"},
			{Name: "role", Label: "Application role", Kind: "select", Choices: "dispatcher=Dispatcher|operator=Operator|mobile=PTT application"}, {Name: "enabled", Label: "Enabled", Kind: "bool"},
			{Name: "talkgroups", Label: "Talkgroup IDs (comma separated)", Kind: "numbers"}, {Name: "permissions", Label: "Permissions (comma separated)", Kind: "strings", Help: "ptt, secure_ptt, emergency, patch, radio_check, page, status, message, location_report, location_view, location_poll, emergency_ack, emergency_clear, crypto_admin, inhibit, uninhibit, regroup."},
		}
	case "profiles":
		return []webField{
			{Name: "name", Label: "Profile name", Kind: "text"}, {Name: "preferred_position_id", Label: "Preferred console position ID", Kind: "text"},
			{Name: "selected_talkgroup", Label: "Selected Talkgroup ID (decimal)", Kind: "number"}, {Name: "transmit_talkgroups", Label: "Transmit Talkgroup IDs (comma separated)", Kind: "numbers"},
			{Name: "zones", Label: "Zones and channel order", Kind: "zones"},
			{Name: "receive", Label: "Receive settings", Kind: "receive"},
		}
	case "automations":
		return []webField{
			{Name: "id", Label: "Automation record ID", Kind: "text"}, {Name: "name", Label: "Name", Kind: "text"}, {Name: "radio_id", Label: "Radio / Unit ID (decimal)", Kind: "number"},
			{Name: "enabled", Label: "Enabled", Kind: "bool"}, {Name: "talkgroups", Label: "Talkgroup IDs (comma separated)", Kind: "numbers"}, {Name: "actions", Label: "Actions (comma separated)", Kind: "strings", Help: "Permitted operations for this automation identity, such as ptt or secure_ptt."},
		}
	case "keys":
		return []webField{
			{Name: "algorithm_id", Label: "Encryption algorithm", Kind: "select-number", Choices: webAlgorithmChoices}, {Name: "key_id", Label: "Key ID (decimal)", Kind: "number"}, {Name: "name", Label: "Key name", Kind: "text"}, {Name: "enabled", Label: "Enabled", Kind: "bool"},
			{Name: "material", Label: "Key material (hexadecimal)", Kind: "password", Help: "Enter the key bytes. Leave blank when editing to keep the stored key."},
		}
	case "recipients":
		return []webField{{Name: "recipient", Label: "Application Server identity", Kind: "text"}, {Name: "public_key", Label: "Application Server public key", Kind: "text", Help: "Public enrollment key reported by that Application Server."}}
	}
	return nil
}

func (s *Service) webDirectoryPage(w http.ResponseWriter, r *http.Request, session Session, path string) {
	kind := strings.Split(path, "/")[0]
	apiKind := kind
	if kind == "profiles" {
		apiKind = "users"
	}
	api := "/api/" + apiKind
	if kind == "keys" || kind == "recipients" {
		api = "/api/crypto/" + kind
	}
	titles := map[string]string{"users": "Application Users", "profiles": "Programmed Profiles", "automations": "Automation Clients", "keys": "Encryption Keys", "recipients": "Authorized Application Servers"}
	fields := webDirectoryFields(kind)
	var err error
	var result *webResult
	record := map[string]any{}
	resource, selected := r.PostForm.Get("resource"), r.PostForm.Has("resource")
	if r.Method == http.MethodPost {
		if encoded := r.PostForm.Get("record"); encoded != "" {
			record, err = webConfig([]byte(encoded))
		}
		if selected && adminInternalRecord(kind) {
			record["id"] = resource
		}
		target, method := api, "POST"
		if selected {
			target += "/" + url.PathEscape(resource)
			method = "PUT"
		}
		switch r.FormValue("action") {
		case "delete":
			method = "DELETE"
		case "token":
			target += "/token"
			method = "POST"
		default:
			if err == nil && kind == "profiles" {
				profile, _ := record["profile"].(map[string]any)
				if profile == nil {
					profile = map[string]any{}
				}
				input := webProfileFields(profile)
				err = webReadFields(r, fields, input)
				if err == nil {
					err = webReadProfile(input, profile)
				}
				record["profile"] = profile
			} else if err == nil {
				saveFields := append([]webField(nil), fields...)
				for i := range saveFields {
					if adminManagedID(kind, saveFields[i].Name) {
						saveFields[i].Kind = "internal"
					}
				}
				err = webReadFields(r, saveFields, record)
				if err == nil && adminInternalRecord(kind) && !selected && webText(record["id"]) == "" {
					record["id"] = webNewRecord(kind)["id"]
				}
				if kind == "keys" && r.FormValue("material") == "" {
					delete(record, "material")
				}
			}
		}
		if err == nil {
			result = s.webAPI(r, session, target, method, record)
			if result.code < 300 && !strings.Contains(result.String(), `"token"`) {
				http.Redirect(w, r, "/"+kind+"?saved=1", http.StatusSeeOther)
				return
			}
		}
	}
	loaded := &webResult{code: 200}
	var rows []any
	var window *adminRecordWindow
	var document map[string]any
	if kind == "users" || kind == "profiles" {
		var readErr error
		rows, window, readErr = s.webUserRows(r)
		if readErr != nil {
			if readErr == sql.ErrNoRows {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Could not read login records.", 500)
			return
		}
	} else {
		loaded = s.webAPI(r, session, api, "GET", nil)
		document, _ = webConfig(loaded.Bytes())
		rows, _ = document[apiKind].([]any)
	}

	if kind == "recipients" {
		recipients, _ := document[kind].(map[string]any)
		names := make([]string, 0, len(recipients))
		for name := range recipients {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			rows = append(rows, map[string]any{"recipient": name, "public_key": recipients[name]})
		}
	}
	resourceID := func(record map[string]any) string {
		switch kind {
		case "keys":
			return webText(record["algorithm_id"]) + "/" + webText(record["key_id"])
		case "recipients":
			return webText(record["recipient"])
		default:
			return webText(record["id"])
		}
	}
	row := -1
	if window != nil && window.Editor {
		if len(rows) != 1 {
			http.NotFound(w, r)
			return
		}
		row = window.Indices[0]
		record, _ = rows[0].(map[string]any)
		resource, selected = resourceID(record), true
	} else if r.Method != http.MethodPost && r.URL.Query().Has("row") {
		row, err = strconv.Atoi(r.URL.Query().Get("row"))
		if err != nil || row < 0 || row >= len(rows) {
			http.NotFound(w, r)
			return
		}
		record, _ = rows[row].(map[string]any)
		resource, selected = resourceID(record), true
	}

	editing := strings.HasSuffix(path, "/edit")
	if r.Method == http.MethodGet && editing && !selected {
		record = webNewRecord(kind)
	}
	title := titles[kind]
	if editing {
		title = webRecordTitle(kind, record)
	}
	s.webHeader(w, r, title, session, false)
	defer webFooter(w)
	if loaded.code >= 300 {
		webJSONResult(w, loaded)
		return
	}
	if err != nil {
		webError(w, err)
	}
	if result != nil {
		webJSONResult(w, result)
	}
	if r.Method == http.MethodGet && r.URL.Query().Has("saved") {
		fmt.Fprint(w, `<div class=ts-notice role=status>Settings saved.</div>`)
	}
	if editing {
		for _, f := range fields {
			if f.Kind == "password" {
				delete(record, f.Name)
			}
		}
		value := record
		if kind == "profiles" {
			profile, _ := record["profile"].(map[string]any)
			value = webProfileFields(profile)
			fmt.Fprintf(w, `<p class=ts-reference><span class=ts-badge>Account · %s</span> <a href="/users/edit?row=%d">Login account</a></p>`, esc(webText(record["username"])), row)
		}
		fmt.Fprintf(w, `<div class=ts-editor><form method=post data-config-form action="%s"`, esc(r.URL.RequestURI()))
		if r.Method == http.MethodPost {
			fmt.Fprint(w, ` data-admin-unsaved`)
		}
		fmt.Fprint(w, `>`)
		webHidden(w, session.CSRF)
		if selected {
			fmt.Fprintf(w, `<input type=hidden name=resource value="%s">`, esc(resource))
		}
		encoded, _ := json.Marshal(record)
		fmt.Fprintf(w, `<input type=hidden name=record value="%s">`, esc(string(encoded)))
		var submitted url.Values
		if r.Method == http.MethodPost {
			submitted = r.PostForm
		}
		s.adminEditorFields(w, kind, fields, value, submitted)
		if kind == "automations" && selected {
			fmt.Fprint(w, `<div class=ts-actions><button name=action value=token formnovalidate>Replace credential</button></div>`)
		}
		webSaveBar(w, "/"+kind, selected && kind != "profiles")
		fmt.Fprint(w, `</form></div>`)
		return
	}
	if kind == "users" || kind == "profiles" {
		fmt.Fprintf(w, `<div class=ts-actions><input type=hidden id=login-choice value=""><button type=button data-chooser-kind=users data-chooser-target=login-choice data-chooser-edit="/%s/edit">Choose login</button></div>`, kind)
	}
	webRecordPage(w, r, kind, rows, webListFields(kind), session.CSRF, window)
	if kind == "recipients" {
		fmt.Fprint(w, `<p class=ts-reference><a href=/enrollment-status>Public enrollment identity</a></p>`)
	}

}

func webProfileFields(profile map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range profile {
		out[k] = v
	}
	return out
}
func webReadProfile(input, profile map[string]any) error {
	for _, key := range []string{"name", "preferred_position_id", "selected_talkgroup", "transmit_talkgroups", "zones", "receive"} {
		profile[key] = input[key]
	}
	return nil
}

func (s *Service) webHistorySettings(w http.ResponseWriter, r *http.Request, session Session) {
	fields := []webField{{Name: "retention_days", Label: "Retention (days)", Kind: "number"}, {Name: "max_events", Label: "Maximum events", Kind: "number"}, {Name: "max_calls", Label: "Maximum calls", Kind: "number"}, {Name: "max_locations", Label: "Maximum location reports", Kind: "number"}, {Name: "location_stale_seconds", Label: "Location becomes stale after (seconds)", Kind: "number"}}
	var result *webResult
	record := map[string]any{}
	if r.Method == http.MethodPost {
		if r.FormValue("action") == "cleanup" {
			result = s.webAPI(r, session, "/api/history/cleanup", http.MethodPost, map[string]any{})
			if result.code < 300 {
				http.Redirect(w, r, r.URL.Path+"?cleanup=1", http.StatusSeeOther)
				return
			}
		} else if err := webReadFields(r, fields, record); err != nil {
			s.webHeader(w, r, "History Storage", session, false)
			defer webFooter(w)
			webError(w, err)
			s.webForm(w, session, "history-settings", fields, record, r.PostForm)
			return
		} else {
			result = s.webAPI(r, session, "/api/history/settings", http.MethodPut, record)
			if result.code < 300 {
				http.Redirect(w, r, r.URL.Path+"?saved=1", http.StatusSeeOther)
				return
			}
		}
	}
	s.webHeader(w, r, "History Storage", session, false)
	defer webFooter(w)
	if result != nil {
		webJSONResult(w, result)
	}
	if r.Method == http.MethodGet && r.URL.Query().Has("saved") {
		fmt.Fprint(w, "<p role=status>Settings saved. Cleanup runs in the background.</p>")
	}
	if r.Method == http.MethodGet && r.URL.Query().Has("cleanup") {
		fmt.Fprint(w, "<p role=status>Cleanup queued.</p>")
	}
	if err := s.activity.InitializationError(); err != nil {
		webError(w, err)
		return
	}
	cleanup := s.activity.CleanupStatus()
	state := "Not run"
	if cleanup.LastCompletedAt != nil {
		state = "Completed"
	}
	if cleanup.LastError != "" {
		state = "Failed"
	}
	if cleanup.Pending {
		state = "Queued"
	}
	if cleanup.Running {
		state = "Running"
	}
	fmt.Fprint(w, "<section><h3>Background cleanup</h3>")
	webProperties(w, map[string]any{"state": state, "last_completed_at": cleanup.LastCompletedAt, "last_error": cleanup.LastError}, []webColumn{{"state", "Status"}, {"last_completed_at", "Last completed"}, {"last_error", "Last error"}})
	fmt.Fprint(w, "<div class=ts-actions><form method=post>")
	webHidden(w, session.CSRF)
	fmt.Fprint(w, "<button name=action value=cleanup>Clean up now</button></form></div></section>")
	if len(record) == 0 {
		settings := s.webAPI(r, session, "/api/history/settings", http.MethodGet, nil)
		if settings.code >= 300 {
			webJSONResult(w, settings)
			return
		}
		status, err := webConfig(settings.Bytes())
		if err != nil {
			webError(w, err)
			return
		}
		record, _ = status["settings"].(map[string]any)
	}
	fmt.Fprint(w, "<section>")
	var submitted url.Values
	if r.Method == http.MethodPost && r.FormValue("action") != "cleanup" {
		submitted = r.PostForm
	}
	s.webForm(w, session, "history-settings", fields, record, submitted)
	fmt.Fprint(w, "</section>")
}

func (s *Service) webHistoryPage(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "History", session, false)
	defer webFooter(w)

	queryFields := []webField{{Name: "since", Label: "From (date and time)", Kind: "text", Help: "Example: 2026-11-02T08:00:00-05:00."}, {Name: "until", Label: "Until (date and time)", Kind: "text"}, {Name: "radio_id", Label: "Radio ID", Kind: "text"}, {Name: "talkgroup_id", Label: "Talkgroup ID", Kind: "text"}, {Name: "site_id", Label: "Site ID", Kind: "text"}, {Name: "channel_id", Label: "Channel ID", Kind: "text"}, {Name: "call_id", Label: "Call ID", Kind: "text"}, {Name: "type", Label: "Event type", Kind: "text"}, {Name: "source_kind", Label: "Source", Kind: "text"}, {Name: "audience", Label: "Audience", Kind: "text"}, {Name: "limit", Label: "Records per page", Kind: "text"}}
	filters := map[string]any{}
	query := url.Values{}
	for _, f := range queryFields {
		value := r.URL.Query().Get(f.Name)
		filters[f.Name] = value
		if f.Name != "record" && value != "" {
			query.Set(f.Name, value)
		}
	}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		query.Set("cursor", cursor)
	}
	kind := r.URL.Query().Get("record")
	if kind == "" {
		kind = "events"
		filters["record"] = kind
	}
	fmt.Fprint(w, "<nav class=ts-editor-nav aria-label=\"History records\">")
	tabs := []struct{ Key, Label string }{{"events", "Events"}, {"calls", "Calls"}, {"locations", "Location reports"}, {"statistics", "Statistics"}}
	for _, tab := range tabs {
		current := ""
		if kind == tab.Key {
			current = " aria-current=page"
		}
		fmt.Fprintf(w, "<a href=\"/history?record=%s\"%s>%s</a>", tab.Key, current, tab.Label)
	}
	fmt.Fprintf(w, "</nav><form class=history-filters method=get><input type=hidden name=record value=\"%s\">", esc(kind))
	s.adminEditorFields(w, "history", queryFields, filters)
	fmt.Fprint(w, "<p><button>Search</button></p></form>")
	if kind == "events" || kind == "calls" || kind == "locations" || kind == "statistics" {
		api := "/api/history/" + kind
		page := s.webAPI(r, session, api+"?"+query.Encode(), "GET", nil)
		webLoaded(w)
		webHistoryResults(w, r, kind, page)
		if kind != "statistics" {
			csv := url.Values{}
			for name, values := range query {
				csv[name] = values
			}
			csv.Set("format", "csv")
			fmt.Fprintf(w, "<p><a href=\"%s?%s\">Download CSV</a></p>", api, esc(csv.Encode()))
		}
		var data map[string]any
		_ = json.Unmarshal(page.Bytes(), &data)
		if cursor := webText(data["next_cursor"]); cursor != "" {
			next := r.URL.Query()
			next.Set("cursor", cursor)
			fmt.Fprintf(w, "<p><a href=\"/history?%s\">Next page</a></p>", esc(next.Encode()))
		}
	}
	fmt.Fprint(w, "<h3>Storage observations</h3>")
	settings := s.webAPI(r, session, "/api/history/settings", "GET", nil)
	if settings.code >= 300 {
		webJSONResult(w, settings)
	} else {
		status, _ := webConfig(settings.Bytes())
		webProperties(w, status, []webColumn{{"events", "Stored events"}, {"calls", "Stored calls"}, {"locations", "Stored location reports"}})
		fmt.Fprint(w, "<h3>Capture observations</h3>")
		webProperties(w, status["ingestion"], []webColumn{{"started_at", "Counters started"}, {"queue_depth", "Queued records"}, {"queue_capacity", "Queue capacity"}, {"queue_dropped", "Dropped records"}, {"rejected_observations", "Rejected observations"}, {"write_failures", "Write failures"}, {"maintenance_failures", "Maintenance failures"}, {"last_write_success_at", "Last successful write"}})
	}
}
