package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inline"
	"go.kenn.io/msgvault/internal/store"
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
	if change.Archived == nil {
		return nil, emailtags.Failure("invalid_request", "archived is required", nil, nil)
	}
	chatID, err := strconv.ParseInt(strings.TrimPrefix(chatKey, "chat:"), 10, 64)
	if err != nil || chatID <= 0 || fmt.Sprintf("chat:%d", chatID) != chatKey {
		return nil, emailtags.Failure("stale_identity", "Inline chat identity changed; sync and retry", nil, err)
	}
	account := a.config.GetInlineAccount(source.Identifier)
	if account == nil || account.EffectiveTransport() != "cli" {
		return nil, emailtags.Failure("archive_unsupported", "Inline archiving requires a configured CLI transport", nil, nil)
	}
	opts, err := inlineImportOptions(a.config, *account)
	if err != nil {
		return nil, emailtags.Failure("stale_identity", "Invalid Inline account configuration", nil, err)
	}
	if len(opts.ChatIDs) != 0 && !slices.Contains(opts.ChatIDs, chatID) {
		return nil, emailtags.Failure("stale_identity", "Inline chat is outside the configured filter", nil, nil)
	}
	message, err := a.store.GetMessageContext(ctx, id)
	if err != nil || message == nil {
		return nil, emailtags.Failure("message_not_found", "Message not found", nil, err)
	}
	prefix := fmt.Sprintf("chat:%d:message:", chatID)
	messageID, err := strconv.ParseInt(strings.TrimPrefix(message.SourceMessageID, prefix), 10, 64)
	if err != nil || messageID <= 0 || fmt.Sprintf("%s%d", prefix, messageID) != message.SourceMessageID {
		return nil, emailtags.Failure("stale_identity", "Inline message identity changed; sync and retry", nil, err)
	}
	client := inline.NewCLIClient(account.CLIPath)
	verifyAccount := func() error {
		me, err := client.Me(ctx)
		if err != nil {
			return err
		}
		if me.Identifier() != source.Identifier || me.Origin != inline.ProductionOrigin {
			return errors.New("authenticated Inline account differs from configuration")
		}
		return nil
	}
	if err := verifyAccount(); err != nil {
		return nil, emailtags.Failure("provider_unavailable", "Cannot verify Inline account", nil, err)
	}
	if err := client.Message(ctx, chatID, messageID); err != nil {
		return nil, emailtags.Failure("stale_identity", "Cannot verify Inline message and chat", nil, err)
	}
	before, err := client.Dialog(ctx, chatID)
	if err != nil {
		return nil, emailtags.Failure("archive_unsupported", "Cannot read Inline personal dialog", nil, err)
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
	execution, err := a.store.AcquireSyncExecutionContext(ctx, sourceID)
	if err != nil {
		code := "local_failed"
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			code = "sync_active"
		}
		return nil, emailtags.Failure(code, "Cannot acquire Inline source ownership", nil, err)
	}
	defer func() { _ = execution.Release() }()
	lockedRecord, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil || lockedRecord == nil || lockedRecord.SourceType != source.SourceType || lockedRecord.Identifier != source.Identifier || !lockedRecord.CreatedAt.Equal(source.CreatedAt) || lockedRecord.MergedIntoSourceID != 0 {
		return nil, emailtags.Failure("stale_identity", "Inline source identity changed; sync and retry", nil, err)
	}
	lockedSource, lockedChat, err := a.store.MessageArchiveChatContext(ctx, id)
	if err != nil || lockedSource != sourceID || lockedChat != chatKey {
		return nil, emailtags.Failure("stale_identity", "Inline message identity changed; sync and retry", nil, err)
	}
	lockedMessage, err := a.store.GetMessageContext(ctx, id)
	if err != nil || lockedMessage == nil || lockedMessage.SourceMessageID != message.SourceMessageID {
		return nil, emailtags.Failure("stale_identity", "Inline message identity changed; sync and retry", nil, err)
	}
	if err := verifyAccount(); err != nil {
		return nil, emailtags.Failure("provider_unavailable", "Cannot verify Inline account", nil, err)
	}
	if err := client.Message(ctx, chatID, messageID); err != nil {
		return nil, emailtags.Failure("stale_identity", "Cannot verify Inline message and chat", nil, err)
	}
	current, err := client.Dialog(ctx, chatID)
	if err != nil || current.Conversation.Type != before.Conversation.Type || current.Conversation.PeerUserID != before.Conversation.PeerUserID || current.Conversation.ParentChatID != before.Conversation.ParentChatID {
		return nil, emailtags.Failure("stale_identity", "Inline dialog identity changed; sync and retry", nil, err)
	}
	if current.Archived == *change.Archived {
		result.Changed = false
		result.Verified = true
		return result, nil
	}
	if err := client.ArchiveDialog(ctx, current, *change.Archived); err != nil {
		return nil, emailtags.Failure("remote_unknown", "Inline archive outcome is unknown; sync and inspect the dialog", nil, err)
	}
	if err := verifyAccount(); err != nil {
		return nil, emailtags.Failure("remote_unknown", "Cannot verify Inline account after archive", nil, err)
	}
	after, err := client.Dialog(ctx, chatID)
	if err != nil || after.Archived != *change.Archived || after.Conversation.Type != current.Conversation.Type || after.Conversation.PeerUserID != current.Conversation.PeerUserID || after.Conversation.ParentChatID != current.Conversation.ParentChatID {
		return nil, emailtags.Failure("remote_unknown", "Cannot verify Inline dialog archive state", nil, err)
	}
	result.Verified = true
	return result, nil
}
