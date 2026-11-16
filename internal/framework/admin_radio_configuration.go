package framework

import (
	"fmt"
	"io"

	"open25.local/open25-fne/internal/radio"
)

func adminRadioColumnHelp(key string) string {
	switch key {
	case "ready":
		return "Yes means radio worker startup completed and current host observations report no admission error. Operating mode identifies hardware or test operation. Busy shows tracked call activity. RF reception and audio quality require separate observations."
	case "state", "host_state":
		return "Starting: resources are starting or warming up. Ready: the native host is available. Stopping: resources are shutting down. Stopped: no active channel worker. Fault: startup, host contact or admission failed; inspect Last error. Busy shows traffic independently of State."
	}
	return ""
}

// Read-only comparison with the running RF snapshot. No validation or RF action.
func (s *Service) webRFConfigurationNotice(w io.Writer) {
	if s.Config().Role != RoleSite {
		return
	}
	status := s.radioStatus()
	if status.State != "running" {
		return
	}
	profile, err := s.profileRadio()
	if err != nil || status.AppliedRFConfiguration == "" {
		return
	}
	if radio.RFConfigurationSignature(profile) != status.AppliedRFConfiguration {
		fmt.Fprint(w, `<div class=ts-notice role=status data-rf-pending>Configured but awaiting restart. Saved RF settings differ from the running Site. <a href=/radio-operation>Stop and start Site to apply</a></div>`)
	}
}
