package vcard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxPhysicalLineBytes = 16 << 20
	DefaultMaxLogicalLineBytes  = 16 << 20
	DefaultMaxCards             = 100_000
)

type sourceLine struct {
	number int
	text   string
}

// Decode parses vCard 2.1, 3.0, and 4.0 syntax with default bounds.
func Decode(r io.Reader) (Document, error) {
	return DecodeWithOptions(r, DecodeOptions{
		MaxPhysicalLineBytes: DefaultMaxPhysicalLineBytes,
		MaxLogicalLineBytes:  DefaultMaxLogicalLineBytes,
		MaxCards:             DefaultMaxCards,
		AllowV21:             true,
	})
}

// DecodeWithOptions parses ordered vCard syntax with explicit bounds.
func DecodeWithOptions(r io.Reader, opts DecodeOptions) (Document, error) {
	opts, err := normalizeDecodeOptions(opts)
	if err != nil {
		return Document{}, err
	}
	physical, err := readPhysicalLines(r, opts.MaxPhysicalLineBytes)
	if err != nil {
		return Document{}, err
	}
	logical, err := unfoldLines(physical, opts.MaxLogicalLineBytes)
	if err != nil {
		return Document{}, err
	}
	return decodeLogicalLines(logical, opts)
}

func normalizeDecodeOptions(opts DecodeOptions) (DecodeOptions, error) {
	if opts.MaxPhysicalLineBytes == 0 {
		opts.MaxPhysicalLineBytes = DefaultMaxPhysicalLineBytes
	}
	if opts.MaxLogicalLineBytes == 0 {
		opts.MaxLogicalLineBytes = DefaultMaxLogicalLineBytes
	}
	if opts.MaxCards == 0 {
		opts.MaxCards = DefaultMaxCards
	}
	if opts.MaxPhysicalLineBytes < 1 {
		return DecodeOptions{}, errors.New("maximum physical line bytes must be positive")
	}
	if opts.MaxLogicalLineBytes < 1 {
		return DecodeOptions{}, errors.New("maximum logical line bytes must be positive")
	}
	if opts.MaxCards < 1 {
		return DecodeOptions{}, errors.New("maximum cards must be positive")
	}
	return opts, nil
}

func readPhysicalLines(r io.Reader, limit int) ([]sourceLine, error) {
	if r == nil {
		return nil, errors.New("vCard reader is nil")
	}
	reader := bufio.NewReader(r)
	var lines []sourceLine
	var line []byte
	lineNumber := 1
	for {
		fragment, prefix, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, &ParseError{PhysicalLine: lineNumber, Err: fmt.Errorf("read input: %w", err)}
		}
		if len(line)+len(fragment) > limit {
			return nil, &ParseError{
				PhysicalLine: lineNumber,
				Err:          fmt.Errorf("physical line %d exceeds %d bytes", lineNumber, limit),
			}
		}
		line = append(line, fragment...)
		if prefix {
			continue
		}
		if bytes.IndexByte(line, '\r') >= 0 {
			return nil, &ParseError{PhysicalLine: lineNumber, Err: errors.New("bare CR is not allowed")}
		}
		if lineNumber == 1 {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		if !utf8.Valid(line) {
			return nil, &ParseError{
				PhysicalLine: lineNumber,
				Err:          errors.New("content line is not valid UTF-8"),
			}
		}
		lines = append(lines, sourceLine{number: lineNumber, text: string(line)})
		line = nil
		lineNumber++
	}
	return lines, nil
}

func unfoldLines(lines []sourceLine, limit int) ([]sourceLine, error) {
	var logical []sourceLine
	var current *sourceLine
	for _, physical := range lines {
		if current == nil {
			if startsFold(physical.text) {
				return nil, &ParseError{
					PhysicalLine: physical.number,
					Err:          errors.New("folded continuation has no previous content line"),
				}
			}
			if len(physical.text) > limit {
				return nil, &ParseError{
					PhysicalLine: physical.number,
					Err:          fmt.Errorf("logical content line exceeds %d bytes", limit),
				}
			}
			lineCopy := physical
			current = &lineCopy
			continue
		}

		continuation := physical.text
		switch {
		case strings.HasSuffix(current.text, "=") && isQuotedPrintableContentLine(current.text):
			current.text = strings.TrimSuffix(current.text, "=")
			if startsFold(continuation) {
				continuation = continuation[1:]
			}
			current.text += continuation
		case startsFold(continuation):
			current.text += continuation[1:]
		default:
			logical = append(logical, *current)
			lineCopy := physical
			current = &lineCopy
		}
		if len(current.text) > limit {
			return nil, &ParseError{
				PhysicalLine: physical.number,
				Err:          fmt.Errorf("logical content line exceeds %d bytes", limit),
			}
		}
	}
	if current != nil {
		logical = append(logical, *current)
	}
	return logical, nil
}

func startsFold(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

func isQuotedPrintableContentLine(line string) bool {
	colon, err := delimiterOutsideQuotes(line, ':')
	if err != nil || colon < 0 {
		return false
	}
	parts, err := splitOutsideQuotes(line[:colon], ';')
	if err != nil {
		return false
	}
	for _, part := range parts[1:] {
		name, value, hasValue := strings.Cut(part, "=")
		if hasValue &&
			strings.EqualFold(name, "ENCODING") &&
			strings.EqualFold(strings.Trim(value, `"`), "QUOTED-PRINTABLE") {
			return true
		}
		if !hasValue && strings.EqualFold(part, "QUOTED-PRINTABLE") {
			return true
		}
	}
	return false
}

func decodeLogicalLines(lines []sourceLine, opts DecodeOptions) (Document, error) {
	var document Document
	var current *Card
	lastPhysicalLine := 0
	for _, line := range lines {
		lastPhysicalLine = line.number
		if line.text == "" {
			continue
		}
		property, err := parseContentLine(line.text)
		if err != nil {
			return Document{}, parseError(line.number, len(document.Cards)+1, err)
		}
		framing := property.Group == "" && len(property.Parameters) == 0
		switch {
		case framing && property.Name == "BEGIN" && strings.EqualFold(property.RawValue, "VCARD"):
			if current != nil {
				return Document{}, parseError(
					line.number,
					len(document.Cards)+1,
					errors.New("nested BEGIN:VCARD"),
				)
			}
			current = &Card{}
		case framing && property.Name == "END" && strings.EqualFold(property.RawValue, "VCARD"):
			if current == nil {
				return Document{}, parseError(line.number, 0, errors.New("stray END:VCARD"))
			}
			if len(document.Cards)+1 > opts.MaxCards {
				return Document{}, parseError(
					line.number,
					len(document.Cards)+1,
					fmt.Errorf("card count exceeds %d", opts.MaxCards),
				)
			}
			document.Cards = append(document.Cards, *current)
			current = nil
		default:
			if current == nil {
				return Document{}, parseError(line.number, 0, errors.New("content outside VCARD"))
			}
			if property.Name == "VERSION" &&
				strings.TrimSpace(property.RawValue) == string(Version21) &&
				!opts.AllowV21 {
				return Document{}, parseError(
					line.number,
					len(document.Cards)+1,
					errors.New("vCard 2.1 is disabled"),
				)
			}
			current.Properties = append(current.Properties, property)
		}
	}
	if current != nil {
		return Document{}, parseError(
			lastPhysicalLine,
			len(document.Cards)+1,
			errors.New("missing END:VCARD"),
		)
	}
	return document, nil
}

func parseError(physicalLine, cardIndex int, err error) error {
	return &ParseError{PhysicalLine: physicalLine, CardIndex: cardIndex, Err: err}
}
