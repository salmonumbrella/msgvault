// Package imapkey encodes mailbox and UID identities used by IMAP sync.
package imapkey

import "strconv"

func Format(mailbox string, uid uint32) string {
	return mailbox + "|" + strconv.FormatUint(uint64(uid), 10)
}
