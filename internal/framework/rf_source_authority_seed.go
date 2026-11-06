package framework

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type RFSourceSeedReceipt struct {
	ZoneID                    string `json:"zone_id"`
	Output                    string `json:"output"`
	RadioCount                int    `json:"radio_count"`
	SHA256                    string `json:"sha256"`
	UnknownRunAffiliations    int    `json:"unknown_run_affiliations"`
	NoncurrentRunAffiliations int    `json:"noncurrent_run_affiliations"`
	UnknownMemberships        int    `json:"unknown_memberships"`
}

// SeedRFSourceAuthority is an explicit offline preparation, never a runtime fallback.
func SeedRFSourceAuthority(ctx context.Context, dataDir, zoneID, output string) (RFSourceSeedReceipt, error) {
	receipt := RFSourceSeedReceipt{ZoneID: zoneID}
	if !recordID.MatchString(zoneID) {
		return receipt, errors.New("invalid Zone identity")
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return receipt, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return receipt, errors.New("stop the Zone service before preparing backend authority")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	open := func(name string) (*sql.DB, error) {
		path, err := filepath.Abs(filepath.Join(dataDir, name))
		if err != nil {
			return nil, err
		}
		if info, e := os.Stat(path + "-wal"); e == nil && info.Size() != 0 {
			return nil, errors.New("checkpoint and close the Zone databases before offline preparation")
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		if info, e := os.Stat(path); e != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("offline input %s is unavailable", name)
		}
		u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
		db, e := sql.Open("sqlite", u.String())
		if e != nil {
			return nil, e
		}
		db.SetMaxOpenConns(1)
		return db, nil
	}
	config, err := open("configuration.db")
	if err != nil {
		return receipt, err
	}
	defer config.Close()
	var role string
	if err = config.QueryRowContext(ctx, "SELECT value FROM framework_metadata WHERE key='role'").Scan(&role); err != nil || role != string(RoleZone) {
		return receipt, errors.New("offline input is not a Zone configuration database")
	}
	readDoc := func(name string, target any) error {
		var raw []byte
		if err := config.QueryRowContext(ctx, "SELECT document FROM framework_documents WHERE name=?", name).Scan(&raw); err != nil {
			return err
		}
		return DecodeStrict(raw, target)
	}
	var zone ZoneConfig
	var system SystemConfig
	if err = readDoc("config", &zone); err != nil {
		return receipt, err
	}
	if zone.ID != zoneID {
		return receipt, errors.New("Zone identity does not match offline configuration")
	}
	if err = readDoc("cache:system", &system); err != nil {
		return receipt, err
	}
	sites := map[string]bool{}
	radios := map[int]bool{}
	groups := map[int]bool{}
	for _, site := range zone.Sites {
		sites[site.ID] = true
	}
	for _, r := range system.Radios {
		if r.Kind == "subscriber" {
			radios[r.RID] = true
		}
	}
	for _, g := range system.Talkgroups {
		groups[g.TGID] = true
	}
	d := rfAuthorityDocument{ZoneID: zoneID, NativeRuns: map[string]time.Time{}, Units: map[int]rfAuthorityUnit{}}
	var reports []SiteReport
	if err = readDoc("cache:site_reports", &reports); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	validAt := func(at time.Time) bool { return !at.IsZero() && !at.After(time.Now().Add(time.Minute)) }
	consider := func(rid int, site string, group int, at time.Time, kind string, run time.Time) error {
		if !radios[rid] || !sites[site] {
			return nil
		}
		if !validAt(at) {
			return errors.New("accepted RF record has an invalid semantic time")
		}
		if group != 0 && !groups[group] {
			return nil
		}
		if run.After(at) {
			run = time.Time{}
		}
		f := rfAuthorityFact{SiteID: site, GroupID: group, At: at.UTC(), NativeStartedAt: run.UTC()}
		unit := d.Units[rid]
		switch kind {
		case "affiliation":
			unit.Affiliation = mergeRFFact(unit.Affiliation, f)
		case "registration":
			unit.Registration = mergeRFFact(unit.Registration, f)
		case "deregistration":
			f.NativeStartedAt = time.Time{}
			unit.Terminal = mergeRFFact(unit.Terminal, f)
		case "null_affiliation":
			f.NativeStartedAt = time.Time{}
			unit.NullAffiliation = mergeRFFact(unit.NullAffiliation, f)
		}
		d.Units[rid] = unit
		return nil
	}
	// A lossy projection is not proof of an RF null-affiliation procedure.
	// Its exclusion time must still prevent unseen older membership reviving.
	recordUnknown := func(rid int, site string, at time.Time) {
		if radios[rid] && sites[site] {
			unit := d.Units[rid]
			unit.MembershipUnknown = mergeRFFact(unit.MembershipUnknown, rfAuthorityFact{SiteID: site, At: at.UTC()})
			d.Units[rid] = unit
		}
	}
	for _, report := range reports {
		if !sites[report.ID] || report.Radio == nil || report.ReceivedAt.IsZero() {
			continue
		}
		var run time.Time
		for _, ch := range report.Radio.Channels {
			if ch.Role == "control" && validAt(ch.StartedAt) && ch.StartedAt.After(run) {
				run = ch.StartedAt.UTC()
			}
		}
		if run.After(d.NativeRuns[report.ID]) {
			d.NativeRuns[report.ID] = run
		}
		for _, r := range report.Radio.Registrations {
			if err = consider(r.RID, report.ID, 0, r.At, "registration", run); err != nil {
				return receipt, err
			}
		}
		for _, a := range report.Radio.Affiliations {
			if a.TGID == 0 {
				if err = consider(a.RID, report.ID, 0, a.At, "registration", run); err != nil {
					return receipt, err
				}
				recordUnknown(a.RID, report.ID, a.At)
				continue
			}
			if err = consider(a.RID, report.ID, a.TGID, a.At, "affiliation", run); err != nil {
				return receipt, err
			}
		}
	}
	history, err := open("activity.db")
	if err != nil {
		return receipt, err
	}
	defer history.Close()
	rows, err := history.QueryContext(ctx, "SELECT radio_id,site_id,talkgroup_id,at,type FROM radio_events WHERE source_kind='rf' AND type IN ('registration','affiliation','deregistration')")
	if err != nil {
		return receipt, err
	}
	for rows.Next() {
		var rid, tg int
		var site, stamp, kind string
		if err = rows.Scan(&rid, &site, &tg, &stamp, &kind); err != nil {
			break
		}
		var at time.Time
		at, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			break
		}
		if kind != "affiliation" {
			tg = 0
		} else if tg == 0 {
			kind = "null_affiliation"
		}
		err = consider(rid, site, tg, at, kind, time.Time{})
		if err != nil {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return receipt, err
	}
	rows, err = history.QueryContext(ctx, "SELECT radio_id,site_id,registered,talkgroup_id,last_seen FROM radio_presence WHERE kind='rf'")
	if err != nil {
		return receipt, err
	}
	for rows.Next() {
		var rid, tg int
		var registered bool
		var site, stamp string
		if err = rows.Scan(&rid, &site, &registered, &tg, &stamp); err != nil {
			break
		}
		var at time.Time
		at, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			break
		}
		kind := "registration"
		if !registered {
			kind = "deregistration"
		} else {
			a := d.Units[rid].Affiliation
			if tg == 0 || (a.SiteID == site && tg != a.GroupID) {
				recordUnknown(rid, site, at)
			}
		}
		err = consider(rid, site, 0, at, kind, time.Time{})
		if err != nil {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return receipt, err
	}
	if err = validateRFAuthorityDocument(d); err != nil {
		return receipt, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return receipt, err
	}
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return receipt, err
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return receipt, errors.New("output must be a new private file")
	}
	f.Close()
	if err = writeRFAuthority(output, d); err != nil {
		os.Remove(output)
		return receipt, err
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		return receipt, err
	}
	digest := sha256.Sum256(raw)
	receipt.Output, receipt.RadioCount, receipt.SHA256 = output, len(d.Units), hex.EncodeToString(digest[:])
	for _, unit := range d.Units {
		if !unit.MembershipUnknown.At.IsZero() {
			receipt.UnknownMemberships++
		}
		a := unit.Affiliation
		if a.At.IsZero() {
			continue
		}
		if a.NativeStartedAt.IsZero() {
			receipt.UnknownRunAffiliations++
		} else if a.NativeStartedAt.Before(d.NativeRuns[a.SiteID]) {
			receipt.NoncurrentRunAffiliations++
		}
	}

	return receipt, nil
}
