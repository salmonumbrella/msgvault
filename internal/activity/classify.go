package activity

import (
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

const messageTypeCalendar = "calendar_event"

// DefaultMaxDirectCounterparts is the largest outbound audience that counts
// as direct contact. Larger sends remain visible activity but are classified
// as co-presence rather than a personal interaction with every recipient.
const DefaultMaxDirectCounterparts = 25

// Classification is the owner-relative interpretation of one archived native
// event. Every person link is already collapsed to its strongest evidence and
// deterministic representative role.
type Classification struct {
	RefKind       store.ActivityRefKind
	Channel       store.ActivityChannel
	Direction     store.ActivityDirection
	OwnerSourceID *int64
	OwnerAddress  string
	Persons       []store.ActivityEventPerson
}

// Classify applies the same pure rules to incremental and backstop projection.
func Classify(candidate store.ActivityCandidate, maxDirectCounterparts int) Classification {
	if maxDirectCounterparts <= 0 {
		maxDirectCounterparts = DefaultMaxDirectCounterparts
	}

	meeting := candidate.MessageType == messageTypeCalendar
	result := Classification{
		RefKind: store.RefKindMessage,
		Channel: channelFor(candidate.ConversationType, meeting),
	}
	if meeting {
		result.RefKind = store.RefKindMeeting
	}

	senderIndex := -1
	for index, counterpart := range candidate.Counterparts {
		if counterpart.RecipientType == "from" {
			senderIndex = index
			break
		}
	}

	switch {
	case senderIndex >= 0 && candidate.Counterparts[senderIndex].IsOwner:
		result.Direction = store.DirectionOutbound
		result.OwnerAddress = candidate.Counterparts[senderIndex].OwnerAddress
	case ownerAmongNonSenders(candidate.Counterparts, senderIndex):
		result.Direction = store.DirectionInbound
		result.OwnerAddress = firstNonSenderOwnerAddress(candidate.Counterparts, senderIndex)
	default:
		result.Direction = store.DirectionObserved
	}
	if result.Direction != store.DirectionObserved && candidate.SourceID > 0 {
		sourceID := candidate.SourceID
		result.OwnerSourceID = &sourceID
	}

	nonOwnerCount := 0
	for _, counterpart := range candidate.Counterparts {
		if !counterpart.IsOwner {
			nonOwnerCount++
		}
	}
	broadcast := nonOwnerCount > maxDirectCounterparts

	for index, counterpart := range candidate.Counterparts {
		if counterpart.IsOwner || counterpart.PersonID == nil {
			continue
		}
		isSender := index == senderIndex
		result.Persons = append(result.Persons, store.ActivityEventPerson{
			PersonID: *counterpart.PersonID,
			Role:     activityRole(counterpart.RecipientType, isSender, meeting),
			Evidence: activityEvidence(result.Direction, isSender, broadcast),
		})
	}
	result.Persons = strongestPersonLinks(result.Persons)
	return result
}

func channelFor(conversationType string, meeting bool) store.ActivityChannel {
	if meeting {
		return store.ChannelMeeting
	}
	switch conversationType {
	case "email_thread":
		return store.ChannelEmail
	case "group_chat", "direct_chat", "channel":
		return store.ChannelChat
	default:
		return store.ChannelOther
	}
}

func ownerAmongNonSenders(counterparts []store.ActivityCounterpart, senderIndex int) bool {
	for index, counterpart := range counterparts {
		if index != senderIndex && counterpart.IsOwner {
			return true
		}
	}
	return false
}

func firstNonSenderOwnerAddress(counterparts []store.ActivityCounterpart, senderIndex int) string {
	for index, counterpart := range counterparts {
		if index != senderIndex && counterpart.IsOwner {
			return counterpart.OwnerAddress
		}
	}
	return ""
}

func activityRole(recipientType string, sender, meeting bool) store.ActivityRole {
	switch {
	case sender && meeting:
		return store.RoleOrganizer
	case sender:
		return store.RoleSender
	case recipientType == "member":
		return store.RoleMember
	case meeting:
		return store.RoleAttendee
	default:
		return store.RoleAddressed
	}
}

func activityEvidence(
	direction store.ActivityDirection,
	sender bool,
	broadcast bool,
) store.ActivityEvidence {
	switch direction {
	case store.DirectionOutbound:
		if !broadcast {
			return store.EvidenceDirect
		}
	case store.DirectionInbound:
		if sender {
			return store.EvidenceDirect
		}
	case store.DirectionObserved:
	}
	return store.EvidenceCoPresence
}

func strongestPersonLinks(links []store.ActivityEventPerson) []store.ActivityEventPerson {
	if len(links) == 0 {
		return nil
	}

	strongest := make(map[int64]store.ActivityEventPerson, len(links))
	for _, link := range links {
		current, found := strongest[link.PersonID]
		if !found || strongerActivityLink(link, current) {
			strongest[link.PersonID] = link
		}
	}

	result := make([]store.ActivityEventPerson, 0, len(strongest))
	for _, link := range strongest {
		result = append(result, link)
	}
	slices.SortFunc(result, func(left, right store.ActivityEventPerson) int {
		switch {
		case left.PersonID < right.PersonID:
			return -1
		case left.PersonID > right.PersonID:
			return 1
		default:
			return 0
		}
	})
	return result
}

func strongerActivityLink(candidate, current store.ActivityEventPerson) bool {
	candidateEvidence := activityEvidencePriority(candidate.Evidence)
	currentEvidence := activityEvidencePriority(current.Evidence)
	if candidateEvidence != currentEvidence {
		return candidateEvidence < currentEvidence
	}
	return activityRolePriority(candidate.Role) < activityRolePriority(current.Role)
}

func activityEvidencePriority(evidence store.ActivityEvidence) int {
	if evidence == store.EvidenceDirect {
		return 0
	}
	return 1
}

func activityRolePriority(role store.ActivityRole) int {
	switch role {
	case store.RoleSender:
		return 0
	case store.RoleOrganizer:
		return 1
	case store.RoleAddressed:
		return 2
	case store.RoleAttendee:
		return 3
	case store.RoleMember:
		return 4
	default:
		return 5
	}
}
