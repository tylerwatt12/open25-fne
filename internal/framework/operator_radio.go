package framework

import (
	"context"
	"errors"
	"open25.local/open25-fne/internal/activity"
	"slices"
	"time"
)

// Operator commands use the subscriber's current Site. Operators cannot choose
// a different Site or elevate a page/check into an administrator command.
func (s *Service) operatorRadioCommand(input OperatorInput, authority operatorAuthority) (OperatorSnapshot, error) {
	if authority.admin || (input.Action != "page" && input.Action != "radio_check" && input.Action != "message") || !operatorPermitted(authority, input.Action) {
		return OperatorSnapshot{}, errors.New("this radio command is not permitted")
	}
	if err := s.validateRadioCommand(radioCommandInput{Action: input.Action, RadioID: input.RadioID, Code: input.Code}); err != nil {
		return OperatorSnapshot{}, err
	}
	units, err := s.operatorUnits(authority, input.TalkgroupID)
	if err != nil {
		return OperatorSnapshot{}, err
	}
	index := slices.IndexFunc(units, func(u OperatorUnit) bool { return u.RadioID == input.RadioID })
	if index < 0 {
		if input.TalkgroupID == 0 {
			return OperatorSnapshot{}, errors.New("this radio has no current affiliation on a permitted talkgroup")
		}
		return OperatorSnapshot{}, errors.New("this radio no longer has a current affiliation on the selected permitted talkgroup; refresh the radio list")
	}
	target := units[index]
	if input.SiteID != "" && input.SiteID != target.SiteID {
		return OperatorSnapshot{}, errors.New("this radio has moved to another Site; refresh the radio list")
	}
	siteID, group := target.SiteID, target.TalkgroupID
	v := s.view()
	inputCommand := radioCommandInput{ID: input.ID, Action: input.Action, RadioID: input.RadioID, SiteID: siteID, Code: input.Code}
	ctx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	var receipt radioCommandReceipt
	if s.Config().Role == RoleSite {
		receipt, err = s.sendSiteRadioCommand(ctx, inputCommand, authority.actor, group)
	} else {
		if v.Zone == nil {
			return OperatorSnapshot{}, errors.New("Zone settings unavailable")
		}
		i := slices.IndexFunc(v.Zone.Sites, func(s SiteIdentity) bool { return s.ID == siteID })
		if i < 0 {
			return OperatorSnapshot{}, errors.New("the radio's Site is unavailable")
		}
		_, peer, e := s.peerIdentityForSite(siteID)
		if e != nil {
			return OperatorSnapshot{}, e
		}
		err = s.requestAt(ctx, v.Zone.Sites[i].Endpoint, "POST", "/internal/radio/command", radioCommandEnvelope{Input: inputCommand, Actor: authority.actor, TalkgroupID: group}, &receipt, &peer)
	}
	if err != nil {
		return OperatorSnapshot{}, err
	}
	if !receipt.Sent || receipt.RadioID != input.RadioID || receipt.Action != input.Action || !sameCommandCode(receipt.Code, input.Code) {
		return OperatorSnapshot{}, errors.New("the Site did not confirm sending this command")
	}
	s.emit(activity.Event{Type: input.Action, RadioID: input.RadioID, TalkgroupID: group, SiteID: siteID, Audience: "dispatcher", Outcome: "sent", SourceKind: "application", Details: map[string]any{"user_id": authority.actor.UserID, "source_radio_id": authority.user.RadioID}})
	result := s.filterOperatorState(s.operatorSnapshot(), authority)
	result.Commands = []radioCommandReceipt{receipt}
	return result, nil
}
