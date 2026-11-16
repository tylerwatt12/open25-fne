package framework

import (
	"context"
	"testing"

	"open25.local/open25-fne/internal/activity"
)

func TestSourceSMTPUsesOnlyLatestAcceptedSystemRetention(t *testing.T) {
	c := integrationNewCluster(t)
	source := c.site.service
	if source.historyRetentionPolicy() != nil {
		t.Fatal("source invented a history policy before acceptance")
	}
	for _, state := range []string{"failed", "smtp_accepted", "queued"} {
		id, err := source.queueSMTP()
		if err != nil {
			t.Fatal(err)
		}
		var d smtpDelivery
		if err := source.Store.smtpRead("smtp-delivery:"+id, &d); err != nil {
			t.Fatal(err)
		}
		d.State = state
		if err := source.Store.smtpDocument("smtp-delivery:"+id, d); err != nil {
			t.Fatal(err)
		}
	}
	source.pruneSMTP(context.Background())
	before, err := source.Store.smtpDeliveries()
	if err != nil || len(before) != 3 {
		t.Fatal("unknown System policy pruned source delivery history", len(before), err)
	}
	policy, err := c.system.service.activity.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	policy.MaxEvents = 1
	if err := c.system.service.activity.SaveRetention(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	if accepted := source.historyRetentionPolicy(); accepted == nil || *accepted != policy {
		t.Fatal("System policy was not accepted", accepted)
	}
	// Archive unavailability is independent of control catalogs. Sources retain
	// the last accepted policy solely for their own SMTP delivery cleanup.
	reopenFixtureWithoutHistory(t, c.system)
	settings := c.zone.service.Config().Settings()
	settings.UpstreamURL = c.system.server.URL
	if err := c.zone.service.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	if accepted := source.historyRetentionPolicy(); accepted == nil || *accepted != policy {
		t.Fatal("missing optional policy replaced accepted System policy", accepted)
	}
	source.pruneSMTP(context.Background())
	after, err := source.Store.smtpDeliveries()
	if err != nil || len(after) != 2 {
		t.Fatal("source ignored accepted System policy", len(after), err)
	}
	queued := false
	for _, d := range after {
		queued = queued || d.State == "queued"
	}
	if !queued {
		t.Fatal("SMTP policy deleted unfinished delivery")
	}
	if _, err := source.activity.Retention(context.Background()); err != activity.ErrArchiveOnly {
		t.Fatal("source retained its own history policy", err)
	}
}
