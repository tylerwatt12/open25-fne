// Package frameworkui contains the administration and operational testing assets.
package frameworkui

import (
	"embed"
	"io/fs"
)

//go:embed style.css forms.js testing.js messaging.js admin.css admin.js admin-chooser.js modem-status.js
var assets embed.FS

func Assets() fs.FS { return assets }
