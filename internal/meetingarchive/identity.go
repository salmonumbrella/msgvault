package meetingarchive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

func normalizedPeople(snapshot Snapshot) (Person, []Person) {
	var organizer Person
	if snapshot.Organizer != nil {
		organizer = snapshot.Organizer.Normalized()
	}
	attendees := make([]Person, 0, len(snapshot.Attendees))
	seen := make(map[string]bool, len(snapshot.Attendees))
	for _, raw := range snapshot.Attendees {
		attendee := raw.Normalized()
		key := archiveParticipantKey(attendee)
		if key != "" && !seen[key] {
			seen[key] = true
			attendees = append(attendees, attendee)
		}
	}
	return organizer, attendees
}

// archiveParticipantKey keeps resolved people distinct even when they share an address.
func archiveParticipantKey(person Person) string {
	if person.ParticipantID > 0 {
		return fmt.Sprintf("participant:%d", person.ParticipantID)
	}
	return person.PrimaryKey()
}

func persistData(person Person) store.ParticipantPersistData {
	if person.Email != "" {
		return store.ParticipantPersistData{
			EmailAddress: person.Email,
			DisplayName:  person.Name,
			Domain:       emailDomain(person.Email),
		}
	}
	return store.ParticipantPersistData{PhoneNumber: person.Phone, DisplayName: person.Name}
}

// Normalized returns the person with trimmed, lowercased emails, E.164-only
// phones, invalid values dropped, and extra identities de-duplicated against
// the primary ones. A room email excludes the whole person.
func (p Person) Normalized() Person {
	for _, email := range append([]string{p.Email}, p.OtherEmails...) {
		email = normalizePersonEmail(email)
		if email != "" && calevent.IsRoomAttendee(email, false) {
			return Person{}
		}
	}
	out := Person{
		Name:                  strings.TrimSpace(p.Name),
		ParticipantID:         p.ParticipantID,
		Email:                 normalizePersonEmail(p.Email),
		Phone:                 normalizePersonPhone(p.Phone),
		Anchor:                strings.TrimSpace(p.Anchor),
		LinkExcludedAddresses: p.LinkExcludedAddresses,
	}
	seenEmails := map[string]bool{out.Email: true}
	for _, email := range p.OtherEmails {
		if email = normalizePersonEmail(email); email != "" && !seenEmails[email] {
			seenEmails[email] = true
			out.OtherEmails = append(out.OtherEmails, email)
		}
	}
	seenPhones := map[string]bool{out.Phone: true}
	for _, phone := range p.OtherPhones {
		if phone = normalizePersonPhone(phone); phone != "" && !seenPhones[phone] {
			seenPhones[phone] = true
			out.OtherPhones = append(out.OtherPhones, phone)
		}
	}
	return out
}

// PrimaryKey names the identity that makes this person a recipient: the
// email when present, otherwise the phone. Empty means the person has no
// usable identity.
func (p Person) PrimaryKey() string {
	switch {
	case p.Email != "":
		return "email:" + p.Email
	case p.Phone != "":
		return "phone:" + p.Phone
	default:
		return ""
	}
}

// identities lists every email and phone of a normalized person, primary
// identities first.
func (p Person) identities() []identity {
	var out []identity
	if p.Email != "" {
		out = append(out, identity{kind: identityEmail, value: p.Email})
	}
	if p.Phone != "" {
		out = append(out, identity{kind: identityPhone, value: p.Phone})
	}
	for _, email := range p.OtherEmails {
		out = append(out, identity{kind: identityEmail, value: email})
	}
	for _, phone := range p.OtherPhones {
		out = append(out, identity{kind: identityPhone, value: phone})
	}
	return out
}

type identityKind string

const (
	identityEmail identityKind = "email"
	identityPhone identityKind = "phone"
)

type identity struct {
	kind  identityKind
	value string
}

// normalizePersonEmail keeps the archiver's historical leniency (providers
// validate their own addresses) while refusing values that cannot be an
// address at all.
func normalizePersonEmail(value string) string {
	value = meetingidentity.Normalize(value)
	if !strings.Contains(value, "@") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return ""
	}
	return value
}

// normalizePersonPhone accepts only E.164 values. Providers normalize their
// own formats before building a snapshot; guessing a country here could
// attach a meeting to the wrong person.
func normalizePersonPhone(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 8 || len(value) > 16 || value[0] != '+' || value[1] == '0' {
		return ""
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return value
}

func (a *Archiver) normalizePeople(ctx context.Context, people []Person) ([]Person, error) {
	if a == nil || a.store == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Person, len(people))
	for i, person := range people {
		out[i] = person.Normalized()
	}
	return out, nil
}

func (a *Archiver) normalizeSnapshot(ctx context.Context, snapshot Snapshot) (Snapshot, error) {
	people, err := a.normalizePeople(ctx, snapshotPeople(snapshot))
	if err != nil {
		return snapshot, err
	}
	if snapshot.Organizer != nil {
		snapshot.Organizer = &people[0]
		people = people[1:]
	}
	snapshot.Attendees = people
	return snapshot, nil
}

func (s Snapshot) validatePeople() error {
	if s.Organizer != nil && s.Organizer.ParticipantID < 0 {
		return errors.New("meeting organizer participant ID cannot be negative")
	}
	for _, attendee := range s.Attendees {
		if attendee.ParticipantID < 0 {
			return errors.New("meeting attendee participant ID cannot be negative")
		}
	}
	return nil
}
