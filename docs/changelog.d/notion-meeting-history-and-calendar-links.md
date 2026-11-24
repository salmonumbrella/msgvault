### Improvements

- Notion meeting sync now finds meetings older than the newest 50, up to 5,000
  per sync. Run `msgvault sync-notion-meetings` to import the older meetings.
- Notion meeting notes now link to their archived Google Calendar event when
  the Notion source's `account_email` is that calendar's mailbox.
