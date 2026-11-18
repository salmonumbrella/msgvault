package emailtags

// IMAPCopy identifies one mailbox generation and UID.
type IMAPCopy struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uidvalidity" format:"int64" minimum:"1"`
	UID         uint32 `json:"uid" format:"int64" minimum:"1"`
}

// Valid reports whether all components identify an IMAP copy.
func (c *IMAPCopy) Valid() bool {
	return c != nil && c.Mailbox != "" && c.UIDValidity != 0 && c.UID != 0
}

// ArchiveChange requires an explicit desired state, including false for unarchive.
type ArchiveChange struct {
	IMAPCopy  *IMAPCopy `json:"imap_copy,omitempty"`
	Archived  *bool     `json:"archived" nullable:"false"`
	DryRun    bool      `json:"dry_run,omitempty"`
	WholeChat bool      `json:"whole_chat,omitempty"`
}

// ArchiveResult describes the verified provider state or the dry-run projection.
type ArchiveResult struct {
	IMAPCopy       *IMAPCopy  `json:"imap_copy,omitempty"`
	IMAPCandidates []IMAPCopy `json:"imap_candidates,omitempty"`
	ChatID         string     `json:"chat_id,omitempty"`
	MessageID      int64      `json:"message_id"`
	SourceID       int64      `json:"source_id"`
	Archived       bool       `json:"archived"`
	Changed        bool       `json:"changed"`
	DryRun         bool       `json:"dry_run"`
	Verified       bool       `json:"verified"`
}
