package framework

import "time"

type affiliationOwner struct {
	SiteID string
	TGID   int
	At     time.Time
}
