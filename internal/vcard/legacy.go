// Package vcard parses and renders vCard documents and projects them into the
// legacy contact shape used by message importers.
package vcard

import (
	"fmt"
	"os"
	"strings"

	"go.kenn.io/msgvault/internal/textimport"
)

// Contact is a single parsed vCard entry.
type Contact struct {
	FullName string
	Phones   []string // normalized to E.164
	Emails   []string // lowercased
}

// ParseFile reads a vCard file and projects its contact identity fields.
func ParseFile(path string) ([]Contact, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open vCard %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	document, err := DecodeWithOptions(file, DecodeOptions{
		MaxPhysicalLineBytes: DefaultMaxPhysicalLineBytes,
		MaxLogicalLineBytes:  DefaultMaxLogicalLineBytes,
		MaxCards:             DefaultMaxCards,
		AllowV21:             true,
	})
	if err != nil {
		return nil, fmt.Errorf("decode vCard %q: %w", path, err)
	}

	contacts := make([]Contact, 0, len(document.Cards))
	for _, card := range document.Cards {
		var contact Contact
		for _, property := range card.Properties {
			switch {
			case strings.EqualFold(property.Name, "FN"):
				name, decodeErr := decodeLegacyText(property)
				if decodeErr != nil {
					return nil, fmt.Errorf("decode FN in %q: %w", path, decodeErr)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					contact.FullName = name
				}
			case strings.EqualFold(property.Name, "TEL"):
				phone, decodeErr := decodeLegacyText(property)
				if decodeErr != nil {
					return nil, fmt.Errorf("decode TEL in %q: %w", path, decodeErr)
				}
				phone = strings.TrimSpace(phone)
				if len(phone) >= len("tel:") && strings.EqualFold(phone[:len("tel:")], "tel:") {
					phone = phone[len("tel:"):]
				}
				if normalized := normalizePhone(phone); normalized != "" {
					contact.Phones = append(contact.Phones, normalized)
				}
			case strings.EqualFold(property.Name, "EMAIL"):
				email, decodeErr := decodeLegacyText(property)
				if decodeErr != nil {
					return nil, fmt.Errorf("decode EMAIL in %q: %w", path, decodeErr)
				}
				email = strings.ToLower(strings.TrimSpace(email))
				if email != "" && strings.Contains(email, "@") {
					contact.Emails = append(contact.Emails, email)
				}
			}
		}
		if contact.FullName != "" || len(contact.Phones) > 0 || len(contact.Emails) > 0 {
			contacts = append(contacts, contact)
		}
	}
	return contacts, nil
}

func decodeLegacyText(property Property) (string, error) {
	raw := property.RawValue
	if propertyIsQuotedPrintable(property) {
		decoded, err := DecodeQuotedPrintable(raw)
		if err != nil {
			return "", err
		}
		raw = decoded
	}
	return UnescapeText(raw)
}

func propertyIsQuotedPrintable(property Property) bool {
	for _, parameter := range property.Parameters {
		for _, value := range parameter.Values {
			if !strings.EqualFold(value.Decoded, "QUOTED-PRINTABLE") {
				continue
			}
			if strings.EqualFold(parameter.Name, "ENCODING") ||
				(parameter.Name == "TYPE" && parameter.OriginalName == "") {
				return true
			}
		}
	}
	return false
}

// normalizePhone normalizes a vCard phone number through the same path used by
// message imports, keeping the lookup keys symmetric across sources.
func normalizePhone(raw string) string {
	normalized, err := textimport.NormalizePhone(raw)
	if err != nil {
		return ""
	}
	return normalized
}
