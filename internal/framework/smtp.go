package framework

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP profiles are local documents, never catalogues or upstream caches.
type smtpProfile struct {
	Inherit        bool   `json:"inherit_smtp"`
	Enabled        bool   `json:"enabled"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Authentication string `json:"authentication"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	SenderName     string `json:"sender_name"`
	SenderAddress  string `json:"sender_address"`
	Recipients     string `json:"recipients"`
}
type smtpDelivery struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	Hops        int       `json:"hops"`
	State       string    `json:"state"`
	Attempts    int       `json:"attempts"`
	CreatedAt   time.Time `json:"created_at"`
	AttemptedAt time.Time `json:"attempted_at"`
	Result      string    `json:"result"`
	Route       string    `json:"route"`
}

func (s *Store) smtpDocument(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", name, raw, nowStamp())
	return err
}
func (s *Store) smtpRead(name string, value any) error {
	var raw []byte
	if err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name=?", name).Scan(&raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
func (s *Service) smtpProfile() (smtpProfile, error) {
	var p smtpProfile
	err := s.Store.smtpRead("smtp-profile", &p)
	if errors.Is(err, sql.ErrNoRows) {
		p.Inherit = s.Config().Role != RoleSystem
		return p, nil
	}
	return p, err
}
func (s *Store) smtpDeliveries() ([]smtpDelivery, error) {
	rows, err := s.db.Query("SELECT document FROM framework_documents WHERE name LIKE 'smtp-delivery:%' ORDER BY at DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []smtpDelivery{}
	for rows.Next() {
		var raw []byte
		var d smtpDelivery
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Service) queueSMTP() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce[:])
	c := s.Config()
	d := smtpDelivery{ID: id, Subject: "Open25 SMTP test", Body: "Explicit SMTP test requested by the administrator of " + c.InstanceID + " (" + string(c.Role) + ").", State: "queued", CreatedAt: time.Now().UTC()}
	return id, s.Store.smtpDocument("smtp-delivery:"+id, d)
}

// Retry is deliberately explicit. An interrupted attempt is never resent automatically.
func (s *Service) retrySMTP(id string) error {
	s.smtpMu.Lock()
	defer s.smtpMu.Unlock()
	var d smtpDelivery
	if err := s.Store.smtpRead("smtp-delivery:"+id, &d); err != nil {
		return err
	}
	if d.State == "attempting" {
		return errors.New("delivery is in progress or its result is uncertain; wait for completion or service restart before retrying")
	}
	if d.State == "smtp_accepted" {
		return errors.New("SMTP already accepted this delivery")
	}
	d.State = "queued"
	d.Result = "Explicit retry queued"
	return s.Store.smtpDocument("smtp-delivery:"+id, d)
}
func (s *Service) runSMTP(ctx context.Context) {
	if s.Store == nil {
		return
	}
	// Resolve interrupted attempts once at startup. Their SMTP result is unknown.
	s.smtpMu.Lock()
	_, _ = s.Store.db.Exec(`UPDATE framework_documents SET document=json_set(document,'$.state','failed','$.result','Previous attempt interrupted; SMTP acceptance unknown. Explicit retry may duplicate mail.') WHERE name LIKE 'smtp-delivery:%' AND json_extract(document,'$.state')='attempting'`)
	s.smtpMu.Unlock()
	maintenance := time.NewTicker(time.Minute)
	defer maintenance.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.processSMTP(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-maintenance.C:
			s.pruneSMTP(ctx)
		}
	}
}
func (s *Service) processSMTP(ctx context.Context) {
	// Query queued work separately from the bounded history presentation.
	rows, err := s.Store.db.Query(`SELECT document FROM framework_documents WHERE name LIKE 'smtp-delivery:%' AND json_extract(document,'$.state')='queued' ORDER BY at LIMIT 1`)
	if err != nil {
		return
	}
	var raw []byte
	if rows.Next() {
		err = rows.Scan(&raw)
	}
	rows.Close()
	if err != nil || len(raw) == 0 {
		return
	}
	var d smtpDelivery
	if json.Unmarshal(raw, &d) != nil {
		return
	}
	_, _ = s.attemptSMTP(ctx, d.ID)
}
func (s *Service) attemptSMTP(ctx context.Context, id string) (smtpDelivery, error) {
	s.smtpMu.Lock()
	defer s.smtpMu.Unlock()
	var d smtpDelivery
	if err := s.Store.smtpRead("smtp-delivery:"+id, &d); err != nil {
		return d, err
	}
	if d.State != "queued" {
		return d, nil
	}
	d.State = "attempting"
	d.Attempts++
	d.AttemptedAt = time.Now().UTC()
	if err := s.Store.smtpDocument("smtp-delivery:"+id, d); err != nil {
		return d, err
	}
	// Network deadlines apply to each hop; the hop bound prevents configured cycles.
	result, route, err := s.sendSMTP(ctx, d)
	d.Route = route
	d.Result = result
	if err != nil {
		d.State = "failed"
	} else {
		d.State = "smtp_accepted"
	}
	if saveErr := s.Store.smtpDocument("smtp-delivery:"+id, d); saveErr != nil {
		return d, saveErr
	}
	return d, nil
}
func (s *Service) sendSMTP(ctx context.Context, d smtpDelivery) (string, string, error) {
	p, err := s.smtpProfile()
	if err != nil {
		return "Could not read local SMTP settings", "", err
	}
	if d.Hops >= 8 {
		return "Notification parent chain exceeded eight hops", "parent", errors.New("parent chain")
	}
	if p.Inherit && s.Config().Role != RoleSystem {
		d.Hops++
		d.ID = fmt.Sprintf("%s-%d", d.ID, d.Attempts)
		var receipt smtpDelivery
		err = s.upstreamRequest(ctx, http.MethodPost, "/internal/smtp", d, &receipt)
		if err != nil {
			return "Parent delivery result unavailable; explicit retry may duplicate mail", "parent", err
		}
		if receipt.State != "smtp_accepted" {
			return receipt.Result, "parent", errors.New("parent delivery failed")
		}
		return "Parent reports SMTP acceptance; inbox delivery unconfirmed", "parent → " + receipt.Route, nil
	}
	route := "local: " + s.Config().InstanceID
	if !p.Enabled {
		return "Local SMTP profile is disabled", route, errors.New("disabled")
	}
	err = smtpSend(ctx, p, d)
	if err != nil {
		return err.Error(), route, err
	}
	return "SMTP server accepted the message; inbox delivery unconfirmed", route, nil
}
func (s *Service) smtpInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	role := s.Config().Role
	allowed := (role == RoleSystem && peer.Role == RoleZone) || (role == RoleZone && (peer.Role == RoleSite || peer.Role == RoleApplication)) || (role == RoleSite && peer.Role == RoleApplication)
	if !allowed {
		fail(w, 403, errors.New("this peer cannot request notification delivery"))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var d smtpDelivery
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
		fail(w, 400, errors.New("unreadable notification request"))
		return
	}
	if d.Hops < 1 || d.Hops > 8 {
		fail(w, 400, errors.New("invalid notification hop count"))
		return
	}
	sum := sha256.Sum256([]byte(string(peer.Role) + ":" + peer.InstanceID + ":" + d.ID))
	id := hex.EncodeToString(sum[:])
	s.smtpMu.Lock()
	var existing smtpDelivery
	err := s.Store.smtpRead("smtp-delivery:"+id, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		d.ID = id
		d.Attempts = 0
		d.State = "queued"
		d.CreatedAt = time.Now().UTC()
		d.Result = ""
		d.Route = ""
		err = s.Store.smtpDocument("smtp-delivery:"+id, d)
	}
	s.smtpMu.Unlock()
	if err != nil {
		fail(w, 500, errors.New("notification storage failed"))
		return
	}
	receipt, err := s.attemptSMTP(r.Context(), id)
	if err != nil {
		fail(w, 500, errors.New("notification result could not be stored"))
		return
	}
	// Return only the public receipt, never the message or a profile/credential.
	receipt.Body = ""
	receipt.Subject = ""
	respond(w, 200, receipt)
}

// The approved internal relay uses plain SMTP over the VPN, including AUTH PLAIN
// when explicitly configured. Do not opportunistically negotiate STARTTLS.
type vpnSMTPAuth struct{ user, password string }

func (a vpnSMTPAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.password), nil
}
func (a vpnSMTPAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected authentication challenge")
	}
	return nil, nil
}
func smtpSend(ctx context.Context, p smtpProfile, d smtpDelivery) error {
	// These are runtime encoding/operation errors, never configuration save checks.
	sender, err := mail.ParseAddress(p.SenderAddress)
	if err != nil {
		return errors.New("Sender address cannot be encoded")
	}
	recipients, err := mail.ParseAddressList(strings.ReplaceAll(p.Recipients, "\n", ","))
	if err != nil || len(recipients) == 0 {
		return errors.New("Recipient addresses cannot be encoded")
	}
	from := (&mail.Address{Name: p.SenderName, Address: sender.Address}).String()
	if strings.ContainsAny(d.Subject, "\r\n") {
		return errors.New("Subject cannot be encoded")
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	if err != nil {
		return errors.New("SMTP connection failed")
	}
	defer conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, p.Host)
	if err != nil {
		return errors.New("SMTP greeting failed")
	}
	defer client.Close()
	if p.Authentication == "password" {
		if err = client.Auth(vpnSMTPAuth{p.Username, p.Password}); err != nil {
			return errors.New("SMTP authentication failed")
		}
	} else if p.Authentication != "none" {
		return errors.New("Configured SMTP authentication is unavailable")
	}
	if err = client.Mail(sender.Address); err != nil {
		return errors.New("SMTP sender rejected")
	}
	to := []string{}
	for _, address := range recipients {
		if err = client.Rcpt(address.Address); err != nil {
			return errors.New("SMTP recipient rejected")
		}
		to = append(to, address.String())
	}
	writer, err := client.Data()
	if err != nil {
		return errors.New("SMTP DATA rejected")
	}
	message := "From: " + from + "\r\nTo: " + strings.Join(to, ", ") + "\r\nSubject: " + d.Subject + "\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\nMessage-ID: <" + d.ID + "@open25.invalid>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(d.Body))
	for len(encoded) > 76 {
		message += encoded[:76] + "\r\n"
		encoded = encoded[76:]
	}
	message += encoded + "\r\n"
	if _, err = io.WriteString(writer, message); err != nil {
		return errors.New("SMTP body transmission failed; acceptance unknown")
	}
	if err = writer.Close(); err != nil {
		return errors.New("SMTP acceptance not confirmed")
	}
	// QUIT failure after successful DATA must not turn accepted mail into a retry.
	_ = client.Quit()
	return nil
}

// Delivery history follows the service's already configured event retention.
// Queued and uncertain/in-progress deliveries are never removed by retention.
func (s *Service) pruneSMTP(ctx context.Context) {
	if s.activity == nil {
		return
	}
	retention, err := s.activity.Retention(ctx)
	if err != nil {
		return
	}
	s.smtpMu.Lock()
	defer s.smtpMu.Unlock()
	if retention.Days > 0 {
		_, _ = s.Store.db.ExecContext(ctx, `DELETE FROM framework_documents WHERE name LIKE 'smtp-delivery:%' AND json_extract(document,'$.state') IN ('smtp_accepted','failed') AND at<?`, time.Now().UTC().AddDate(0, 0, -retention.Days).Format(time.RFC3339Nano))
	}
	if retention.MaxEvents > 0 {
		_, _ = s.Store.db.ExecContext(ctx, `DELETE FROM framework_documents WHERE name IN (SELECT name FROM framework_documents WHERE name LIKE 'smtp-delivery:%' AND json_extract(document,'$.state') IN ('smtp_accepted','failed') ORDER BY at DESC LIMIT -1 OFFSET ?)`, retention.MaxEvents)
	}
}
