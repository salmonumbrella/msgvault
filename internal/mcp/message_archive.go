package mcp

import (
	"context"
	"encoding/json/v2"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/emailtags"
)

const ToolArchiveMessage = "archive_message"

type MessageArchiveBackend interface {
	ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error)
}

func messageArchiveDefinition() toolDefinition {
	result, failure := outputSchemaFor[emailtags.ArchiveResult](), outputSchemaFor[emailtags.MessageTagError]()
	result.Schema, failure.Schema = "", ""
	definition := writeDefinition(ToolArchiveMessage, "Set a Gmail or IMAP message's archive state, or archive its entire Beeper chat with whole_chat=true. Beeper requires a source-wide grant without incoming sender restrictions. archived=false restores the inbox. Other providers return archive_unsupported. dry_run previews without writing. For multiple IMAP copies, an authorized read-capable preview lists imap_candidates; select one with imap_copy and use the confirmed destination tuple to restore it.", closedObject(map[string]*jsonschema.Schema{
		"imap_copy": closedObject(map[string]*jsonschema.Schema{
			"mailbox": {Type: "string", MinLength: new(1)}, "uidvalidity": {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(4294967295))}, "uid": {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(4294967295))},
		}, "mailbox", "uidvalidity", "uid"),
		"whole_chat": booleanSchema("Required for Beeper: affects the entire chat"),
		"message_id": safeIDSchema("Archived message ID"), "archived": booleanSchema("Desired archive state"), "dry_run": booleanSchema("Preview without writing"),
	}, "message_id", "archived"), &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{result, failure}}, (*handlers).archiveMessage)
	definition.availability = func(c catalogCapabilities) bool { return c.messageArchive }
	yes := true
	definition.annotations.OpenWorldHint = &yes
	definition.annotations.IdempotentHint = true
	return definition
}

func (h *handlers) archiveMessage(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	id, err := getIDArg(args, "message_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	archived, ok := args["archived"].(bool)
	if !ok {
		return toolErrorResult("archived must be true or false"), nil
	}
	dryRun, _ := args["dry_run"].(bool)
	wholeChat, _ := args["whole_chat"].(bool)
	var selection *emailtags.IMAPCopy
	if raw, present := args["imap_copy"]; present {
		data, err := json.Marshal(raw)
		if err != nil {
			return toolErrorResult("invalid imap_copy"), nil //nolint:nilerr // MCP reports input errors in the tool result.
		}
		selection = &emailtags.IMAPCopy{}
		if err := json.Unmarshal(data, selection); err != nil || !selection.Valid() {
			return toolErrorResult("imap_copy requires mailbox, uidvalidity and uid"), nil //nolint:nilerr // MCP reports input errors in the tool result.
		}
	}
	value, err := h.messageArchive.ArchiveMessage(ctx, id, emailtags.ArchiveChange{Archived: &archived, DryRun: dryRun, WholeChat: wholeChat, IMAPCopy: selection})
	if err != nil {
		return messageTagErrorResult(err)
	}
	return jsonResult(value)
}
