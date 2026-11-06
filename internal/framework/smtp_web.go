package framework

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

func (s *Service) webSMTP(w http.ResponseWriter, r *http.Request, session Session, path string) {
	w.Header().Set("Cache-Control", "no-store")
	message := ""
	var operationErr error
	if r.Method == http.MethodPost {
		switch path {
		case "smtp-settings":
			port, err := strconv.Atoi(r.FormValue("port"))
			if r.FormValue("port") == "" {
				port = 0
				err = nil
			}
			if err != nil {
				operationErr = errors.New("Port could not be read as a decimal integer")
				break
			}
			s.Store.mu.Lock()
			old, err := s.smtpProfile()
			if err == nil {
				p := smtpProfile{Inherit: r.FormValue("inherit_smtp") == "true", Enabled: r.FormValue("enabled") == "true", Host: r.FormValue("host"), Port: port, Authentication: r.FormValue("authentication"), Username: r.FormValue("username"), Password: old.Password, SenderName: r.FormValue("sender_name"), SenderAddress: r.FormValue("sender_address"), Recipients: r.FormValue("recipients")}
				if replacement := r.FormValue("smtp_password"); replacement != "" {
					p.Password = replacement
				}
				err = s.Store.smtpDocument("smtp-profile", p)
			}
			s.Store.mu.Unlock()
			if err != nil {
				operationErr = errors.New("SMTP settings could not be stored")
			} else {
				message = "SMTP settings saved. No email was sent."
			}
		case "notification-actions":
			switch r.FormValue("operation") {
			case "test":
				id, err := s.queueSMTP()
				if err != nil {
					operationErr = errors.New("Test could not be queued")
				} else {
					message = "Test queued: " + id + ". Check Notification delivery for the actual result."
				}
			case "retry":
				operationErr = s.retrySMTP(r.FormValue("delivery_id"))
				if errors.Is(operationErr, sql.ErrNoRows) {
					operationErr = errors.New("Delivery identity was not found")
				}
				if operationErr == nil {
					message = "Explicit retry queued. Check Notification delivery for the actual result."
				}
			default:
				operationErr = errors.New("Unknown notification operation")
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	}
	if operationErr != nil {
		w.WriteHeader(http.StatusBadRequest)
	}
	title := map[string]string{"smtp-settings": "SMTP settings", "notification-actions": "Email test and retry", "notification-delivery": "Notification delivery"}[path]
	s.webHeader(w, r, title, session, false)
	defer webFooter(w)
	if message != "" {
		fmt.Fprintf(w, `<p class=ts-notice role=status>%s</p>`, esc(message))
	}
	if operationErr != nil {
		fmt.Fprintf(w, `<p class=ts-notice role=alert>%s</p>`, esc(operationErr.Error()))
	}
	if path == "notification-delivery" {
		rows, err := s.Store.smtpDeliveries()
		if err != nil {
			fmt.Fprint(w, `<p role=alert>Delivery records unavailable.</p>`)
			return
		}
		fmt.Fprint(w, `<p class=ts-muted>SMTP acceptance is not proof of inbox delivery. Failed deliveries require explicit Retry. An interrupted attempt may already have sent mail.</p>`)
		display := []map[string]any{}
		for _, delivery := range rows {
			row := webObject(delivery)
			row["state"] = map[string]string{"queued": "Queued", "attempting": "Attempting", "failed": "Failed", "smtp_accepted": "SMTP accepted"}[delivery.State]
			if delivery.AttemptedAt.IsZero() {
				row["attempted_at"] = "Not attempted"
			}
			display = append(display, row)
		}
		webTable(w, []webColumn{{"id", "Delivery"}, {"created_at", "Queued"}, {"state", "State"}, {"attempts", "Attempts"}, {"attempted_at", "Last attempt"}, {"route", "Route"}, {"result", "Result"}}, display)
		fmt.Fprint(w, `<a class=ts-link href=/notification-actions>Email test and retry</a>`)
		return
	}
	page := adminPlannedPages[path]
	fields := append([]webField(nil), page.Fields...)
	record := map[string]any{}
	if path == "smtp-settings" {
		profile, err := s.smtpProfile()
		if err != nil {
			fmt.Fprint(w, `<p role=alert>SMTP settings unavailable.</p>`)
			return
		}
		record = webObject(profile)
		delete(record, "password")
		if s.Config().Role == RoleSystem {
			fields = fields[1:]
		}
		route := "Local SMTP profile"
		if profile.Inherit && s.Config().Role != RoleSystem {
			route = "Parent service sends using its effective SMTP profile"
			if s.upstreamURL() == "" {
				route = "Parent route unavailable: no upstream connection configured"
			}
		}
		fmt.Fprintf(w, `<div class=ts-reference><div class=ts-itemline><strong>Effective delivery route</strong><span class=ts-badge>Read only</span></div><p>%s</p></div><p class=ts-muted>Plain SMTP over the internal VPN. Local fields remain editable and preserved while inheriting. Blank replacement password retains the stored password. Save does not send email.</p>`, esc(route))
	}
	fmt.Fprint(w, `<div class=ts-editor><form method=post data-config-form`)
	if operationErr != nil {
		fmt.Fprint(w, ` data-admin-unsaved`)
	}
	fmt.Fprint(w, `>`)
	webHidden(w, session.CSRF)
	var submitted = r.PostForm
	if r.Method != http.MethodPost || operationErr == nil {
		submitted = nil
	}
	s.adminEditorFields(w, path, fields, record, submitted)
	if path == "notification-actions" {
		fmt.Fprint(w, `<div class=ts-savebar><span class=ts-muted>Explicit operation using saved settings</span><button class=ts-primary>Submit operation</button></div>`)
	} else {
		webSaveBar(w, "/"+path)
	}
	fmt.Fprint(w, `</form></div>`)
}
