//go:build !linux

package historypriority

// Enter leaves development hosts' scheduling unchanged. Deployment's Linux
// thread priorities must be verified; this is not a hard realtime guarantee.
func Enter() error { return nil }
