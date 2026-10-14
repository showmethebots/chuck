package notify

// The Socket Mode path is exercised by the live checklist
// (docs/shadow-checklist.md); this pins the Poster contract at compile time.
var _ Poster = (*Slack)(nil)
