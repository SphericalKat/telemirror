// Package telegram defines the Telegram boundary Telemirror depends on.
//
// The mirror service works with the Client interface and the value types in
// this package. The botapi adapter implements the interface against the
// Telegram Bot API; tests replace it with a fake.
package telegram

import "context"

// User identifies a Telegram user.
type User struct {
	// ID is the Telegram user ID.
	ID int64 `json:"id"`

	// Username is the user name without the leading @. It is empty when the
	// user has no public user name.
	Username string `json:"username"`

	// FirstName is the display name used when Username is empty.
	FirstName string `json:"first_name"`
}

// Chat identifies a Telegram chat.
// Type is one of "private", "group", "supergroup", or "channel".
type Chat struct {
	// ID is the Telegram chat ID.
	ID int64 `json:"id"`

	// Type names the chat type.
	Type string `json:"type"`

	// AllMembersAreAdministrators reports a basic group where every member
	// holds administrator rights. Telegram fills it for "group" chats only.
	AllMembersAreAdministrators bool `json:"all_members_are_administrators"`
}

// Message is a Telegram message the bot can act on.
type Message struct {
	// MessageID identifies the message inside its chat.
	MessageID int64 `json:"message_id"`

	// From is the sender. It is nil for channel posts.
	From *User `json:"from"`

	// Chat is the chat that holds the message.
	Chat Chat `json:"chat"`

	// Text is the message text.
	Text string `json:"text"`

	// ReplyToMessage is the message this message replies to, if any.
	ReplyToMessage *Message `json:"reply_to_message"`
}

// Update is one Telegram update.
// Only message updates carry work for the bot.
type Update struct {
	// UpdateID identifies the update and orders the update stream.
	UpdateID int64 `json:"update_id"`

	// Message is the message inside this update, if any.
	Message *Message `json:"message"`
}

// Client is the Telegram behavior the mirror service needs.
// Implementations send every message with HTML parse mode.
type Client interface {
	// SendMessage sends text to chatID and returns the sent message.
	// A non-zero replyToMessageID makes the sent message a reply.
	SendMessage(ctx context.Context, chatID int64, text string, replyToMessageID int64) (Message, error)

	// EditMessageText replaces the text of one message.
	EditMessageText(ctx context.Context, chatID int64, messageID int64, text string) error

	// DeleteMessage removes one message.
	DeleteMessage(ctx context.Context, chatID int64, messageID int64) error

	// ChatAdministrators returns the users who administer chatID.
	ChatAdministrators(ctx context.Context, chatID int64) ([]User, error)
}
