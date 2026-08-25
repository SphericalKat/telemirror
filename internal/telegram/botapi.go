package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// DefaultBase is the Telegram Bot API base URL.
const DefaultBase = "https://api.telegram.org"

// pollLongWait is the server-side long-poll wait in seconds.
const pollLongWait = 50

// pollErrorDelay is the pause between two getUpdates attempts after an error.
const pollErrorDelay = 3 * time.Second

// API talks to the Telegram Bot API over HTTP. It implements Client.
type API struct {
	base   string
	token  string
	client *http.Client
}

var _ Client = (*API)(nil)

// NewAPI returns a Bot API client for one bot token.
// A nil client uses the default HTTP client.
func NewAPI(token string, client *http.Client) *API {
	return newAPI(DefaultBase, token, client)
}

func newAPI(base, token string, client *http.Client) *API {
	if client == nil {
		client = &http.Client{}
	}
	return &API{base: base, token: token, client: client}
}

// apiResponse is the common Bot API response envelope.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

// call performs one Bot API method call and decodes its result.
func (a *API) call(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}

	url := fmt.Sprintf("%s/bot%s/%s", a.base, a.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram %s: read response: %w", method, err)
	}

	var envelope apiResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("telegram %s: decode response: %w", method, err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram %s: %d %s", method, envelope.ErrorCode, envelope.Description)
	}
	if result == nil || len(envelope.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// SendMessage sends text with HTML parse mode and returns the sent message.
// A zero replyToMessageID sends a message that replies to nothing.
func (a *API) SendMessage(ctx context.Context, chatID int64, text string, replyToMessageID int64) (Message, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if replyToMessageID != 0 {
		payload["reply_to_message_id"] = replyToMessageID
	}

	var sent Message
	if err := a.call(ctx, "sendMessage", payload, &sent); err != nil {
		return Message{}, err
	}
	return sent, nil
}

// EditMessageText replaces the text of one message, keeping HTML parse mode.
func (a *API) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	}
	return a.call(ctx, "editMessageText", payload, nil)
}

// DeleteMessage removes one message.
func (a *API) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	return a.call(ctx, "deleteMessage", payload, nil)
}

// Poll long-polls for updates and hands each one to handler. Poll blocks
// until ctx is cancelled and then returns nil. Failed getUpdates calls are
// logged and retried after a pause.
func (a *API) Poll(ctx context.Context, handler func(context.Context, Update)) error {
	var offset int64
	for {
		if err := a.pollOnce(ctx, &offset, handler); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("telegram: getUpdates: %v", err)
			select {
			case <-time.After(pollErrorDelay):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// pollOnce performs one long-poll request and dispatches its updates.
func (a *API) pollOnce(ctx context.Context, offset *int64, handler func(context.Context, Update)) error {
	payload := map[string]any{
		"timeout":         pollLongWait,
		"allowed_updates": []string{"message"},
	}
	if *offset != 0 {
		payload["offset"] = *offset
	}

	// Give the request more time than the server-side long-poll wait.
	ctx, cancel := context.WithTimeout(ctx, (pollLongWait+10)*time.Second)
	defer cancel()

	var updates []Update
	if err := a.call(ctx, "getUpdates", payload, &updates); err != nil {
		return err
	}
	for _, upd := range updates {
		if upd.UpdateID >= *offset {
			*offset = upd.UpdateID + 1
		}
		handler(ctx, upd)
	}
	return nil
}
