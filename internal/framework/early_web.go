package framework

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type webField struct {
	Name, Label, Kind, Choices, Help, Hint string
	Related                                bool
	ChoiceKind                             string
	Computed                               string
	RelationLabels                         map[string]string
	Multiple                               bool
	BulkApply, ApplyChecked                bool
}
type webSection struct {
	Path, Title, Collection string
	Role                    Role
	Fields                  []webField
}

func esc(v any) string { return html.EscapeString(fmt.Sprint(v)) }
func webHidden(w io.Writer, csrf string) {
	fmt.Fprintf(w, "<input type=hidden name=csrf value=\"%s\">", esc(csrf))
}
func webFooter(w http.ResponseWriter) {

	fmt.Fprint(w, "</section><footer class=ts-foot>Open25 FNE</footer></main></div></div></div></body></html>")
}
func webPlainFooter(w http.ResponseWriter) { fmt.Fprint(w, "</main></html>") }
func webLoaded(w io.Writer) {
	fmt.Fprint(w, `<p class=loaded><a class=ts-button href="">Refresh</a></p>`)
}
func webError(w io.Writer, err error) { fmt.Fprintf(w, "<p class=notice role=alert>%s</p>", esc(err)) }
func webPlainHeader(w http.ResponseWriter, title string, role Role, session Session, live bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content=\"width=device-width,initial-scale=1\"><title>%s</title><link rel=stylesheet href=/style.css>", esc(title))
	if live {
		fmt.Fprint(w, "<meta http-equiv=refresh content=5>")
	}
	fmt.Fprint(w, "<script src=/forms.js defer></script><header><strong>Open25 FNE</strong> | ", esc(role), "</header>")
	if session.Username != "" {
		fmt.Fprint(w, "<nav><a href=/>Home</a> | ")
		if session.UserID == "" {
			for _, p := range webSections() {
				if p.Role == role {
					fmt.Fprintf(w, "<a href=/%s>%s</a> | ", p.Path, esc(p.Title))
				}
			}
			fmt.Fprint(w, "<a href=/networking>Networking</a> | <a href=/reports>Site Reports</a> | <a href=/topology>Topology</a> | <a href=/account>Administrator</a> | ")
			if role == RoleSystem {
				fmt.Fprint(w, "<a href=/history>History</a> | <a href=/users>Users</a> | <a href=/profiles>Programmed Profiles</a> | <a href=/automations>Automation Clients</a> | <a href=/keys>Encryption Keys</a> | <a href=/recipients>Authorized Application Servers</a> | ")
			}
			if role == RoleZone || role == RoleSite {
				fmt.Fprint(w, "<a href=/live-status>Live Status</a> | <a href=/radio-commands>Radio Commands</a> | ")
			}
			if role == RoleSite {
				fmt.Fprint(w, "<a href=/radio-operation>Radio Operation</a> | ")
			}
		}
		if role == RoleApplication {
			fmt.Fprint(w, "<a href=/dispatch-test>Dispatch Testing</a> | <a href=/ptt-test>PTT Testing</a> | ")
		}
		fmt.Fprintf(w, "</nav><p>Signed in: %s</p><form method=post action=/logout>", esc(session.Username))
		webHidden(w, session.CSRF)
		fmt.Fprint(w, "<button>Sign out</button></form>")
	}
	fmt.Fprintf(w, "<main><h1>%s</h1>", esc(title))
}
func webObject(value any) map[string]any {
	raw, _ := json.Marshal(value)
	out, _ := webConfig(raw)
	return out
}
func webConfig(raw []byte) (map[string]any, error) {
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	err := d.Decode(&out)
	if out == nil {
		out = map[string]any{}
	}
	return out, err
}
func webValue(record map[string]any, path string) any {
	var value any = record
	for _, name := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[name]
	}
	return value
}
func webSet(record map[string]any, path string, value any) {
	names := strings.Split(path, ".")
	for _, name := range names[:len(names)-1] {
		next, ok := record[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			record[name] = next
		}
		record = next
	}
	record[names[len(names)-1]] = value
}
func webText(value any) string {
	if value == nil {
		return ""
	}
	if list, ok := value.([]any); ok {
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = fmt.Sprint(item)
		}
		return strings.Join(out, ", ")
	}
	return fmt.Sprint(value)
}

// webScaleDecimal changes decimal units without rounding through a float.
// Exponents that would expand to long runs of zeros stay in scientific form.
func webScaleDecimal(text string, exponent int) (string, error) {
	original := text
	sign := ""
	if strings.HasPrefix(text, "-") || strings.HasPrefix(text, "+") {
		if text[0] == '-' {
			sign = "-"
		}
		text = text[1:]
	}
	power := new(big.Int)
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		if _, ok := power.SetString(text[index+1:], 10); !ok {
			return "", fmt.Errorf("could not read %q as a number", original)
		}
		text = text[:index]
	}
	whole, fraction, _ := strings.Cut(text, ".")
	digits := whole + fraction
	if digits == "" {
		return "", fmt.Errorf("could not read %q as a number", original)
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("could not read %q as a number", original)
		}
	}
	power.Add(power, big.NewInt(int64(exponent)))
	point := new(big.Int).Add(power, big.NewInt(int64(len(whole))))
	if !point.IsInt64() || point.Int64() < -32 || point.Int64() > int64(len(digits))+32 {
		whole = strings.TrimLeft(whole, "0")
		if whole == "" {
			whole = "0"
		}
		fraction = strings.TrimRight(fraction, "0")
		if fraction != "" {
			whole += "." + fraction
		}
		return sign + whole + "e" + power.String(), nil
	}
	position := int(point.Int64())
	switch {
	case position <= 0:
		whole, fraction = "0", strings.Repeat("0", -position)+digits
	case position >= len(digits):
		whole, fraction = digits+strings.Repeat("0", position-len(digits)), ""
	default:
		whole, fraction = digits[:position], digits[position:]
	}
	whole, fraction = strings.TrimLeft(whole, "0"), strings.TrimRight(fraction, "0")
	if whole == "" {
		whole = "0"
	}
	if fraction != "" {
		whole += "." + fraction
	}
	return sign + whole, nil
}

// Only these known measurements have presentation units. IDs remain numbers.
func webUnit(key string) (string, int) {
	key = strings.TrimPrefix(key, "calibration.")
	switch key {
	case "base_frequency_hz", "tx_frequency_hz", "rx_frequency_hz", "uplink_offset_hz":
		return "MHz", 6
	case "channel_spacing_hz", "bandwidth_hz":
		return "kHz", 3
	case "lifetime_ms", "call_lifetime_ms", "allocated_ms", "speech_ms", "duration_ms":
		return "s", 3
	case "rx_offset_hz", "tx_offset_hz":
		return "Hz", 0
	case "last_frame_age_ms", "maximum_processing_ms":
		return "ms", 0
	case "poll_seconds", "location_stale_seconds", "age_seconds":
		return "s", 0
	case "retention_days":
		return "days", 0
	case "rx_level", "tx_level", "power_percent":
		return "%", 0
	case "volume":
		return "%", -2
	case "latitude", "longitude":
		return "°", 0
	case "accuracy":
		return "m", 0
	case "baud":
		return "baud", 0
	}
	return "", 0
}

func webUnitText(key string, value any) string {
	text := webText(value)
	_, places := webUnit(key)
	if places != 0 && text != "" {
		if scaled, err := webScaleDecimal(text, -places); err == nil {
			if unit, _ := webUnit(key); unit == "MHz" && !strings.ContainsAny(scaled, "eE") {
				whole, fraction, _ := strings.Cut(scaled, ".")
				if len(fraction) < 6 {
					fraction += strings.Repeat("0", 6-len(fraction))
				}
				scaled = whole + "." + fraction
				if key == "uplink_offset_hz" && !strings.HasPrefix(scaled, "-") {
					scaled = "+" + scaled
				}
			}
			return scaled
		}
	}
	if key == "talkgroups" || key == "transmit_talkgroups" || key == "affiliation_radio_ids" || key == "transmit_radio_ids" {
		items := strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' })
		for i := range items {
			items[i] = webDecimalText(key, items[i])
		}
		return strings.Join(items, ", ")
	}
	return webDecimalText(key, text)
}

func webReadFields(r *http.Request, fields []webField, record map[string]any) error {
	for _, f := range fields {
		if f.Kind == "derived-frequency" || f.Kind == "internal" {
			continue
		}
		text := r.FormValue(f.Name)
		if webHexWidth(f) > 0 && r.FormValue(f.Name+"__base") == "hex" {
			text = r.FormValue(f.Name + "__hex")
			if text != "" {
				n, ok := new(big.Int).SetString(text, 16)
				if !ok {
					return fmt.Errorf("could not read %s as hexadecimal", f.Label)
				}
				text = n.String()
			}
		}
		var value any = text
		switch f.Kind {
		case "zones", "receive":
			var err error
			value, err = webReadProfileRows(r, f.Kind)
			if err != nil {
				return err
			}
		case "bool":
			value = text == "true"
		case "number", "select-number":
			if text == "" {
				value = nil
			} else {
				if _, e := strconv.ParseFloat(text, 64); e != nil {
					return fmt.Errorf("could not read %s as a number", f.Label)
				}
				if _, places := webUnit(f.Name); places != 0 {
					var err error
					text, err = webScaleDecimal(text, places)
					if err != nil {
						return fmt.Errorf("could not read %s as a number", f.Label)
					}
				}
				text, e := webScaleDecimal(text, 0)
				if e != nil {
					return fmt.Errorf("could not read %s as a number", f.Label)
				}
				value = json.Number(text)
			}
		case "numbers":
			list := []any{}
			for _, v := range strings.FieldsFunc(text, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' }) {
				if _, e := strconv.ParseInt(v, 10, 64); e != nil {
					return fmt.Errorf("could not read %s as numbers", f.Label)
				}
				canonical, _ := webScaleDecimal(v, 0)
				list = append(list, json.Number(canonical))
			}
			value = list
		case "strings":
			text = strings.Join(r.PostForm[f.Name], ",")
			list := []any{}
			for _, v := range strings.FieldsFunc(text, func(c rune) bool { return c == ',' || c == '\n' || c == '\r' }) {
				list = append(list, strings.TrimSpace(v))
			}
			value = list
		}
		webSet(record, f.Name, value)
	}
	return nil
}

type webResult struct {
	header http.Header
	bytes.Buffer
	code int
}

func (v *webResult) Header() http.Header  { return v.header }
func (v *webResult) WriteHeader(code int) { v.code = code }

// Forms call existing authenticated operations directly, without another connection.
func (s *Service) webAPI(r *http.Request, session Session, path, method string, value any) *webResult {
	raw, _ := json.Marshal(value)
	req := r.Clone(r.Context())
	req.URL, _ = url.Parse(path)
	req.Method = method
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", session.CSRF)
	out := &webResult{header: http.Header{}, code: 200}
	switch path {
	case "/api/login":
		s.login(out, req)
	case "/api/settings":
		var settings Settings
		err := json.Unmarshal(raw, &settings)
		if err == nil {
			err = s.SaveSettings(settings)
		}
		if err != nil {
			fail(out, 400, err)
		} else {
			respond(out, 200, map[string]bool{"saved": true})
		}
	case "/api/account":
		s.changePassword(out, req)
	default:
		if !s.operationsHTTP(out, req, session) {
			http.NotFound(out, req)
		}
	}
	return out
}
func webJSONResult(w io.Writer, result *webResult) {
	var value map[string]any
	_ = json.Unmarshal(result.Bytes(), &value)
	if message, ok := value["error"]; ok {
		fmt.Fprintf(w, "<p class=notice role=alert>%s</p>", esc(message))
		return
	}
	if result.code >= 300 {
		fmt.Fprintf(w, "<p role=alert>Operation returned HTTP %d: %s</p>", result.code, esc(result.String()))
		return
	}
	if token, ok := value["token"]; ok {
		fmt.Fprintf(w, "<p>Credential — copy it now:</p><pre>%s</pre>", esc(token))
	}
	if result.code == 202 {
		fmt.Fprint(w, "<p>Request accepted. Refresh status to read its outcome.</p>")
	} else {
		fmt.Fprint(w, "<p>Saved.</p>")
	}
}
func (s *Service) webLoginForm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("logged_out") == "1" {
		if r.URL.Query().Get("pending") == "1" {
			fmt.Fprint(w, `<p role=status>Signed out locally. Shared sign-out delivery is pending reconnection.</p>`)
		} else {
			fmt.Fprint(w, `<p role=status>Signed out of this shared session. Disconnected portals will revoke it when they reconnect.</p>`)
		}
	}
	fmt.Fprint(w, "<form method=post action=/login><p><label>Username <input name=username autocomplete=username></label></p><p><label>Password <input type=password name=password autocomplete=current-password></label></p>")
	fmt.Fprintf(w, `<input type=hidden name=continue value="%s">`, esc(ssoContinue(r.FormValue("continue"))))
	if s.Config().Role != RoleSystem {
		fmt.Fprint(w, `<p><a href="/sso/start">Sign in through System Administration</a></p><p>Local sign-in works while disconnected.</p>`)
	} else if continuation, err := url.Parse(ssoContinue(r.FormValue("continue"))); err == nil && continuation.Path == "/sso/authorize" {
		callback := continuation.Query().Get("redirect_uri")
		if s.ssoCallbackAllowed(callback) {
			fmt.Fprintf(w, `<p><a href="%s">Sign in locally at the destination instead</a></p>`, esc(strings.TrimSuffix(callback, "/sso/callback")+"/login?local=1"))
		}
	}
	if s.Config().Role == RoleApplication {
		fmt.Fprint(w, "<p><label>Console position ID <input name=position_id></label> <small>Leave empty for a PTT account or its default position.</small></p>")
	}
	fmt.Fprint(w, "<div class=actions><button class=primary type=submit>Sign in</button></div></form>")
}
func (s *Service) earlyWeb(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/style.css" || r.URL.Path == "/forms.js" || r.URL.Path == "/testing.js" || r.URL.Path == "/messaging.js" || r.URL.Path == "/admin.css" || r.URL.Path == "/admin.js" || r.URL.Path == "/admin-chooser.js" || r.URL.Path == "/modem-status.js" || r.URL.Path == "/hardware-activity.js" || r.URL.Path == "/timestamps.js" {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.FileServer(http.FS(s.assets)).ServeHTTP(w, r)
		return
	}
	if strings.Contains(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".") {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	if r.Method == "POST" {
		if !sameOrigin(r) {
			http.Error(w, "Request must originate from this service.", 403)
			return
		}
		// URL encoding can triple both the 4 MiB snapshot and edited fields.
		r.Body = http.MaxBytesReader(w, r.Body, 6*(4<<20)+4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Could not read the form.", 400)
			return
		}
	}
	if s.ssoWeb(w, r) {
		return
	}
	session, authenticated := s.session(r)
	if r.URL.Path == "/login" {
		s.ssoLoginPolicy(w, r)
		if r.Method == "POST" {
			result := s.webAPI(r, Session{}, "/api/login", "POST", map[string]string{"username": r.FormValue("username"), "password": r.FormValue("password"), "position_id": r.FormValue("position_id")})
			if result.code < 300 {
				for _, cookie := range result.header.Values("Set-Cookie") {
					w.Header().Add("Set-Cookie", cookie)
				}
				destination := ssoContinue(r.FormValue("continue"))
				if destination == "" {
					destination = "/"
				}
				http.Redirect(w, r, destination, http.StatusSeeOther)
				return
			}
			s.webHeader(w, r, "Sign in", Session{}, false)
			webJSONResult(w, result)
			s.webLoginForm(w, r)
			webFooter(w)
			return
		}
		s.webHeader(w, r, "Sign in", Session{}, false)
		s.webLoginForm(w, r)
		webFooter(w)
		return
	}
	if !authenticated {
		destination := "/login"
		role := s.Config().Role
		if role != RoleSystem && !(role == RoleApplication && (r.URL.Path == "/dispatch-test" || r.URL.Path == "/ptt-test")) {
			destination = "/sso/start?return=" + url.QueryEscape(r.URL.RequestURI())
		}
		http.Redirect(w, r, destination, http.StatusSeeOther)
		return
	}
	if r.Method == "POST" && subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(session.CSRF)) != 1 {
		http.Error(w, "Reload this page before submitting.", 403)
		return
	}
	if r.URL.Path == "/logout" && r.Method == "POST" {
		delivered, err := s.signOutSession(session)
		if err != nil {
			http.Error(w, "Could not record shared sign-out.", 500)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
		destination := "/login?logged_out=1"
		if !delivered {
			destination += "&pending=1"
		}
		http.Redirect(w, r, destination, http.StatusSeeOther)
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	if (path == "dispatch-test" || path == "ptt-test") && s.Config().Role == RoleApplication {
		title, kind := "Open25 Dispatch", "dispatch"
		if path == "ptt-test" {
			title, kind = "Open25 PTT", "ptt"
		}
		webPlainHeader(w, title, s.Config().Role, session, false)
		fmt.Fprintf(w, "<section id=messages aria-label=Messages></section><section id=audio-test data-kind=%s></section><script src=/timestamps.js defer></script><script src=/messaging.js defer></script><script src=/testing.js defer></script>", kind)
		webPlainFooter(w)
		return
	}
	if session.UserID != "" {
		if path == "" {
			webPlainHeader(w, "Operator testing", s.Config().Role, session, false)
			fmt.Fprint(w, "<p><a href=/dispatch-test>Dispatch Testing</a> | <a href=/ptt-test>PTT Testing</a></p>")
			webPlainFooter(w)
		} else {
			http.Error(w, "Administrator sign-in required.", 403)
		}
		return
	}
	for _, section := range webSections() {
		if section.Role == s.Config().Role && (path == section.Path || path == section.Path+"/edit" || path == section.Path+"/bulk") {
			if strings.HasSuffix(path, "/bulk") {
				s.webBulkRecords(w, r, session, section)
				return
			}
			s.webConfigSection(w, r, session, section)
			return
		}
	}
	if s.webAdmin(w, r, session, path) {
		return
	}
	http.NotFound(w, r)
}
func (s *Service) webConfigSection(w http.ResponseWriter, r *http.Request, session Session, section webSection) {
	raw, err := s.Store.Config()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	config, err := webConfig(raw)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet && section.Path == "control-policy" {
		if _, exists := config["control_policy"]; !exists {
			config["control_policy"] = webNewControlPolicy()
		}
	}
	if r.Method == http.MethodGet && section.Path == "neighbor-announcements" {
		if _, exists := config["neighbor_mode"]; !exists {
			config["neighbor_mode"] = "all"
		}
	}
	row := -1
	clone := -1
	if r.Method == http.MethodGet && r.URL.Query().Has("clone") {
		clone, err = strconv.Atoi(r.URL.Query().Get("clone"))
		if err != nil || clone < 0 {
			http.NotFound(w, r)
			return
		}
	}
	if r.URL.Query().Has("row") {
		row, err = strconv.Atoi(r.URL.Query().Get("row"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	list, _ := config[section.Collection].([]any)
	if r.Method != http.MethodPost && (row < -1 || (row >= len(list) && section.Collection != "")) {
		http.NotFound(w, r)
		return
	}
	editing := section.Collection == "" || strings.HasSuffix(r.URL.Path, "/edit")
	if clone >= len(list) && clone >= 0 {
		http.NotFound(w, r)
		return
	}
	record := config
	resource, selected := r.PostForm.Get("resource"), r.PostForm.Has("resource")
	identity := section.Fields[0].Name
	occurrence, _ := strconv.Atoi(r.PostForm.Get("resource_occurrence"))
	if section.Collection != "" {
		record = map[string]any{}
		if r.Method == http.MethodPost {
			record, err = webConfig([]byte(r.PostForm.Get("record")))
			if err != nil {
				http.Error(w, "Could not read submitted record.", 400)
				return
			}
			row = -1
			if selected {
				remaining := occurrence
				for i, item := range list {
					object, _ := item.(map[string]any)
					if webText(webValue(object, identity)) == resource {
						if remaining > 0 {
							remaining--
							continue
						}
						row = i
						break
					}
				}
			}
		} else if row >= 0 && row < len(list) {
			record, _ = list[row].(map[string]any)
			resource, selected = webText(webValue(record, identity)), true
			for _, item := range list[:row] {
				v, _ := item.(map[string]any)
				if webText(webValue(v, identity)) == resource {
					occurrence++
				}
			}
		} else if clone >= 0 {
			record, _ = list[clone].(map[string]any)
			if adminInternalRecord(section.Path) {
				record["id"] = webNewRecord(section.Path)["id"]
			}
		} else if editing {
			record = webNewRecord(section.Path)
			if section.Path == "channels" {
				record["name"] = fmt.Sprintf("RF channel %d", len(list)+1)
			}
			if section.Path == "modems" {
				record["name"] = fmt.Sprintf("USB modem %d", len(list)+1)
			}
		}
	}
	if section.Path == "identification" {
		record, _ = config["identification"].(map[string]any)
		if r.Method == http.MethodPost {
			record = map[string]any{}
		}
	}
	if record == nil {
		record = map[string]any{}
	}
	if r.Method == http.MethodPost {
		if selected && adminInternalRecord(section.Path) {
			record["id"] = resource
		}
		if r.FormValue("action") == "delete" && section.Collection != "" {
			if row >= 0 {
				config[section.Collection] = append(list[:row], list[row+1:]...)
			}
		} else {
			saveFields := append([]webField(nil), section.Fields...)
			for i := range saveFields {
				if adminManagedID(section.Path, saveFields[i].Name) {
					saveFields[i].Kind = "internal"
				}
			}
			err = webReadFields(r, saveFields, record)
			if err == nil && adminInternalRecord(section.Path) && !selected && webText(record["id"]) == "" {
				record["id"] = webNewRecord(section.Path)["id"]
			}
			if err == nil && section.Collection != "" {
				if !selected && adminInternalRecord(section.Path) {
					for i, item := range list {
						existing, _ := item.(map[string]any)
						if webText(existing["id"]) == webText(record["id"]) {
							row = i
							break
						}
					}
				}
				if row >= 0 {
					list[row] = record
				} else {
					list = append(list, record)
				}
				config[section.Collection] = list
			}
			if err == nil && section.Path == "identification" {
				config["identification"] = record
			}
		}
		if err == nil {
			var encoded []byte
			encoded, err = json.Marshal(config)
			if err == nil {
				err = s.saveOwned(encoded)
			}
		}
		if err == nil {
			s.refreshDirectory()
			http.Redirect(w, r, "/"+section.Path+"?saved=1", http.StatusSeeOther)
			return
		}
	}

	title := section.Title
	if editing && section.Collection != "" {
		title = webRecordTitle(section.Path, record)
		if section.Path == "channels" {
			title = "Edit RF channel"
			if !selected {
				title = "New RF channel"
			}
		}
		if clone >= 0 {
			title = "Copy of " + title
		}
	}
	s.webHeader(w, r, title, session, false)
	defer webFooter(w)
	if err != nil {
		webError(w, err)
	}
	if r.Method == http.MethodGet && r.URL.Query().Has("saved") {
		if r.URL.Query().Has("updated") {
			fmt.Fprintf(w, `<div class=ts-notice role=status>Saved %s selected records.</div>`, esc(r.URL.Query().Get("updated")))
		} else {
			fmt.Fprint(w, `<div class=ts-notice role=status>Settings saved.</div>`)
		}
	}

	if editing {
		if section.Path == "identification" || section.Path == "site-identity" || section.Path == "control-policy" || section.Path == "neighbor-announcements" {
			s.webRFConfigurationNotice(w)
		}
		if section.Path != "channels" {
			s.webProgrammingReference(w, section.Path)
		}
		fmt.Fprint(w, `<div class=ts-editor>`)
		fmt.Fprintf(w, `<form method=post data-config-form data-admin-page="%s" action="%s"`, esc(section.Path), esc(r.URL.RequestURI()))
		if err != nil && r.Method == http.MethodPost {
			fmt.Fprint(w, ` data-admin-unsaved`)
		}
		fmt.Fprint(w, `>`)
		webHidden(w, session.CSRF)
		snapshot := record
		if section.Collection == "" {
			snapshot = map[string]any{}
			for _, f := range section.Fields {
				webSet(snapshot, f.Name, webValue(record, f.Name))
			}
		}
		encoded, _ := json.Marshal(snapshot)
		fmt.Fprintf(w, `<input type=hidden name=record value="%s">`, esc(string(encoded)))
		if selected {
			fmt.Fprintf(w, `<input type=hidden name=resource value="%s"><input type=hidden name=resource_occurrence value="%d">`, esc(resource), occurrence)
		}
		var submitted url.Values
		if err != nil && r.Method == http.MethodPost {
			submitted = r.PostForm
		}
		s.adminEditorFields(w, section.Path, section.Fields, record, submitted)
		if section.Path == "channels" {
			s.webProgrammingReference(w, section.Path)
		}
		webSaveBar(w, "/"+section.Path, section.Collection != "" && row >= 0)
		fmt.Fprint(w, `</form></div>`)
		return
	}
	if section.Path == "channels" || section.Path == "modems" {
		s.webRFConfigurationNotice(w)
	}
	if section.Path == "modems" {
		fmt.Fprint(w, `<p class=ts-reference><a href=/hardware>Discovered radio hardware</a></p>`)
	}
	if section.Path == "channels" {
		zone := s.webKnownZone()
		for _, item := range list {
			channel, _ := item.(map[string]any)
			identifier, idErr := strconv.Atoi(webText(channel["identifier"]))
			number, numberErr := strconv.Atoi(webText(channel["channel_no"]))
			tx, rx, known := bandPlanFrequencies(zone, identifier, number)
			channel["tx_frequency_hz"], channel["rx_frequency_hz"] = "Unavailable", "Unavailable"
			if known && idErr == nil && numberErr == nil {
				channel["tx_frequency_hz"], channel["rx_frequency_hz"] = tx, rx
			}
		}
	}
	if section.Path == "modems" {
		var site SiteConfig
		_ = s.owned(&site)
		inventory := s.hardwareInventory()
		for _, item := range list {
			modem, _ := item.(map[string]any)
			names := []string{}
			for _, ch := range site.Channels {
				if ch.ModemID == webText(modem["id"]) {
					names = append(names, ch.Name)
				}
			}
			modem["channel_names"] = strings.Join(names, ", ")
			if len(names) == 0 {
				modem["channel_names"] = "Unassigned"
			}
			modem["hardware_identity"] = "Not observed"
			for _, device := range inventory.Devices {
				if device.Device == webText(modem["device"]) {
					if device.UID != "" {
						modem["hardware_identity"] = device.UID
					}
					break
				}
			}
		}
	}
	presented, _ := s.adminNamedPresentation(list).([]any)
	webRecordPage(w, r, section.Path, presented, webListFields(section.Path), session.CSRF)
	if section.Path == "channels" {
		s.webProgrammingReference(w, section.Path)
	}

}

// List columns are presentation only. Editors retain every declared field.
func webListFields(path string) []webField {
	columns := map[string][]string{
		"radios":        {"rid", "alias", "group", "kind", "enabled"},
		"talkgroups":    {"tgid", "alias", "group", "site_access", "admission_mode", "enabled"},
		"status-labels": {"id", "label"}, "message-labels": {"id", "label"},
		"positions":   {"name", "radio_id", "talkgroups", "enabled"},
		"identifiers": {"identifier", "base_frequency_hz", "channel_spacing_hz", "uplink_offset_hz"},
		"sites":       {"name", "site_id", "lra"},
		"modems":      {"name", "channel_names", "hardware_identity"},
		"channels":    {"name", "modem_id", "tx_frequency_hz", "rx_frequency_hz", "enabled"},
		"users":       {"username", "role", "radio_id", "enabled"},
		"profiles":    {"profile.name", "profile.preferred_position_id", "profile.selected_talkgroup"},
		"automations": {"name", "radio_id", "enabled"},
		"keys":        {"name", "algorithm_id", "key_id", "enabled"},
		"recipients":  {"recipient", "public_key"},
	}
	fields := webDirectoryFields(path)
	for _, section := range webSections() {
		if section.Path == path {
			fields = section.Fields
		}
	}
	if path == "profiles" {
		fields = []webField{{Name: "profile.preferred_position_id", Label: "Preferred console position"}, {Name: "profile.name", Label: "Profile name"}, {Name: "profile.selected_talkgroup", Label: "Selected group", Kind: "number"}}
	}
	if path == "modems" {
		fields = append(fields, webField{Name: "channel_names", Label: "Assigned channel"}, webField{Name: "hardware_identity", Label: "USB serial identity"})
	}
	out := make([]webField, 0, len(columns[path]))
	for _, name := range columns[path] {
		for _, field := range fields {
			if field.Name == name {
				labels := map[string]string{"rid": "Radio / Unit ID", "tgid": "Talkgroup ID", "alias": "Name", "enabled": "Access", "affiliation_access": "Affiliation", "transmit_access": "Radio transmit", "site_access": "Sites", "start_policy": "Call start", "priority": "Priority", "admission_mode": "Allowed calls", "transmit_mode": "Transmit strapping", "device": "Device", "baud": "Rate (baud)", "role": "Role", "modem_id": "Modem", "tx_frequency_hz": "Site TX (MHz)", "rx_frequency_hz": "Site RX (MHz)", "radio_id": "Radio ID", "talkgroups": "Talkgroups", "key_id": "Key ID", "algorithm_id": "Algorithm", "recipient": "Application Server", "public_key": "Public key"}
				if label, ok := labels[name]; ok {
					field.Label = label
				}
				if path == "channels" {
					if name == "name" {
						field.Label = "Channel"
					}
					if name == "modem_id" {
						field.Label = "Assigned modem"
					}
					if name == "enabled" {
						field.Label = "Enabled"
					}
				}
				if path == "modems" && name == "name" {
					field.Label = "Modem"
				}
				out = append(out, field)
				break
			}
		}
	}
	return out
}
func webDisplay(field webField, value any) string {
	if flag, ok := value.(bool); ok {
		if flag {
			return "Enabled"
		}
		return "Disabled"
	}
	text := webUnitText(field.Name, value)
	if field.Kind == "select" || field.Kind == "select-number" {
		for _, choice := range strings.Split(field.Choices, "|") {
			pair := strings.SplitN(choice, "=", 2)
			if pair[0] == webText(value) && len(pair) == 2 {
				if width := webHexWidth(field); width > 0 {
					if notation := webHexText(text, width); notation != "" {
						return text + " / " + notation + " · " + pair[1]
					}
				}
				return pair[1]
			}
		}
	}
	if text == "" {
		return "—"
	}
	if width := webHexWidth(field); width > 0 {
		if notation := webHexText(text, width); notation != "" {
			return text + " / " + notation + " hex"
		}
	}
	return text
}
