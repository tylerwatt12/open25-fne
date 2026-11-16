package framework

import (
	"context"
	"errors"
	"fmt"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// Read the Site-owned preference once, when Run begins. Saving programming or
// opening a page cannot schedule startup in an already-running service.
func (s *Service) radioStartupContext(parent context.Context) context.Context {
	if s.Config().Role != RoleSite {
		return nil
	}
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		s.runtimeError(fmt.Errorf("read Site startup setting: %w", err))
		return nil
	}
	if !site.StartOnServiceStart {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	s.operationMu.Lock()
	s.radioStartupCancel, s.radioStartupError = cancel, ""
	s.operationMu.Unlock()
	return ctx
}

// Automatic startup uses the same admission, maintenance lock and RF startup
// as Start Site. Its only additional behavior is a cancellable startup retry.
func (s *Service) runRadioStartup(ctx context.Context) {
	defer func() {
		s.operationMu.Lock()
		if s.radioStartupCancel != nil {
			s.radioStartupCancel()
		}
		s.radioStartupCancel, s.radioStartupError = nil, ""
		s.operationMu.Unlock()
	}()
	delay, signature := time.Second, ""
	for ctx.Err() == nil {
		if s.radioStatus().State == "running" {
			return // An explicit Start already established the Site.
		}
		attempt, cancel := context.WithTimeout(ctx, 60*time.Second)
		admission, err := s.admitRadioStart(attempt, "")
		if err == nil {
			if signature != "" && signature != admission.signature {
				s.finishRadioStart(admission)
				cancel()
				return // Changed RF programming cancels the original startup intent.
			}
			signature = admission.signature
			err = s.startRadioAdmitted(admission.ctx, admission)
			s.finishRadioStart(admission)
		}
		cancel()
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		if errors.Is(err, radio.ErrMaintenance) {
			// Maintenance refusals are never queued for replay after release.
			s.runtimeError(fmt.Errorf("automatic Site startup withheld: %w", err))
			return
		}
		if err == nil {
			_ = s.Store.AddEvent("radio", "Automatic Site startup completed.")
			return
		}
		s.operationMu.Lock()
		changed := s.radioStartupError != err.Error()
		if ctx.Err() == nil {
			s.radioStartupError = err.Error()
		}
		s.operationMu.Unlock()
		if changed {
			s.runtimeError(fmt.Errorf("automatic Site startup failed; retrying: %w", err))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}
