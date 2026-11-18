package cmd

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inline"
)

func (a *storeAPIAdapter) archiveInlineChat(ctx context.Context, id int64, change emailtags.ArchiveChange, grant *agentgrant.Grant, acquireWrite func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error) {
	sourceID, chatKey, err := a.store.MessageArchiveChatContext(ctx, id)
	if err != nil {
		return nil, err
	}
	source, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if grant != nil {
		selected, ok := grant.MatchingSource(agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier})
		if !ok || len(selected.IncomingSenderKeys) != 0 {
			return nil, emailtags.Failure("message_not_found", "Message not found", nil, nil)
		}
	}
	if !change.WholeChat {
		return nil, emailtags.Failure("invalid_request", "Inline archives entire chats; set whole_chat=true", nil, nil)
	}
	account := a.config.GetInlineAccount(source.Identifier)
	if account == nil || account.EffectiveTransport() != "cli" {
		return nil, emailtags.Failure("archive_unsupported", "Inline archiving requires a configured CLI transport", nil, nil)
	}
	message, err := a.store.GetMessageContext(ctx, id)
	if err != nil || message == nil {
		return nil, emailtags.Failure("message_not_found", "Message not found", nil, err)
	}
	chatID, messageID, err := inline.ParseArchiveKeys(chatKey, message.SourceMessageID)
	if err != nil {
		return nil, emailtags.Failure("stale_identity", "Inline message or chat identity changed; sync and retry", nil, err)
	}
	chatIDs, err := parseInlineChatIDs(account.ChatIDs)
	if err != nil {
		return nil, emailtags.Failure("stale_identity", "Invalid Inline chat filter", nil, err)
	}
	if len(chatIDs) != 0 && !slices.Contains(chatIDs, chatID) {
		return nil, emailtags.Failure("stale_identity", "Inline chat is outside the configured filter", nil, nil)
	}
	client := inline.NewCLIClient(account.CLIPath)
	verifyAccount := func() error {
		me, err := client.Me(ctx)
		if err != nil {
			return err
		}
		if me.Identifier() != source.Identifier {
			return errors.New("authenticated Inline account differs from configuration")
		}
		return nil
	}
	if err := verifyAccount(); err != nil {
		return nil, emailtags.Failure("provider_unavailable", "Cannot verify Inline account", nil, err)
	}
	if err := client.Message(ctx, chatID, messageID); err != nil {
		return nil, inlineReadFailure("stale_identity", "Cannot verify Inline message and chat", err)
	}
	before, err := client.Dialog(ctx, chatID)
	if err != nil {
		return nil, inlineReadFailure("archive_unsupported", "Cannot read Inline personal dialog", err)
	}
	result := &emailtags.ArchiveResult{MessageID: id, SourceID: sourceID, ChatID: chatKey, Archived: *change.Archived, Changed: before.Archived != *change.Archived, DryRun: change.DryRun}
	if change.DryRun {
		return result, nil
	}
	if !result.Changed {
		result.Verified = true
		return result, nil
	}
	if acquireWrite != nil {
		release, err := acquireWrite(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	lockedSource, lockedChat, err := a.store.MessageArchiveChatContext(ctx, id)
	if err != nil || lockedSource != sourceID || lockedChat != chatKey {
		return nil, emailtags.Failure("stale_identity", "Inline message identity changed; sync and retry", nil, err)
	}
	lockedMessage, err := a.store.GetMessageContext(ctx, id)
	if err != nil || lockedMessage == nil || lockedMessage.SourceMessageID != message.SourceMessageID {
		return nil, emailtags.Failure("stale_identity", "Inline message identity changed; sync and retry", nil, err)
	}
	lockedRecord, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil || lockedRecord == nil || lockedRecord.Identifier != source.Identifier || lockedRecord.SourceType != source.SourceType || !lockedRecord.CreatedAt.Equal(source.CreatedAt) || lockedRecord.MergedIntoSourceID != 0 {
		return nil, emailtags.Failure("stale_identity", "Inline source identity changed; sync and retry", nil, err)
	}
	if err := verifyAccount(); err != nil {
		return nil, emailtags.Failure("provider_unavailable", "Cannot verify Inline account", nil, err)
	}
	if err := client.ArchiveDialog(ctx, before, *change.Archived); err != nil {
		return nil, emailtags.Failure("remote_unknown", "Inline archive outcome is unknown; inspect the dialog in Inline and retry the desired state", nil, err)
	}
	if err := verifyAccount(); err != nil {
		return nil, emailtags.Failure("remote_unknown", "Cannot verify Inline account after archive; inspect the dialog in Inline and retry the desired state", nil, err)
	}
	after, err := client.Dialog(ctx, chatID)
	if err != nil || after.Archived != *change.Archived || after.Conversation.Type != before.Conversation.Type || after.Conversation.PeerUserID != before.Conversation.PeerUserID || after.Conversation.ParentChatID != before.Conversation.ParentChatID {
		return nil, emailtags.Failure("remote_unknown", "Cannot verify Inline dialog archive state; inspect the dialog in Inline and retry the desired state", nil, err)
	}
	result.Verified = true
	return result, nil
}

// Process failures do not establish whether the target or provider is available.
func inlineReadFailure(code, message string, err error) error {
	if errors.Is(err, inline.ErrDialogIdentity) {
		return emailtags.Failure("stale_identity", "Inline dialog identity changed; sync and retry", nil, err)
	}
	var commandErr *inline.CLICommandError
	if errors.As(err, &commandErr) {
		return emailtags.Failure("provider_unavailable", "Cannot contact Inline CLI", nil, err)
	}
	return emailtags.Failure(code, message, nil, err)
}
