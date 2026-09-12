// Package convref holds the grammar for naming a Slack conversation or person
// in a tool's arguments. It lives outside internal/slack because a tool that
// only documents the grammar must not drag a Slack client into a binary that
// may not have one.
package convref

// Conversation is shared by every tool schema that takes a conversation, so the
// documented grammar cannot drift from the resolver that enforces it.
const Conversation = "#channel-name, a channel ID (C…/G…), a DM ID (D…), or a person — @handle, a user ID (U…) or <@U…> — which resolves to your DM with them."

// User is shared by every tool schema that takes a person, for the same reason.
const User = "@handle (matched against username, display name and real name), a user ID (U…/W…), or <@U…>."
