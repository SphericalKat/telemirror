package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// request records one Bot API call the adapter made.
type request struct {
	method string
	body   map[string]any
}

// fakeBotAPI answers Bot API calls and records the request bodies.
type fakeBotAPI struct {
	mu       sync.Mutex
	requests []request
	// responses maps a Bot API method to a raw JSON response body.
	responses map[string]string
	// handler overrides the default response lookup when set.
	handler func(method string, body map[string]any, w http.ResponseWriter)
}

func newFakeBotAPI() *fakeBotAPI {
	return &fakeBotAPI{responses: map[string]string{}}
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/bottest-token/")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	parsed := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, request{method: method, body: parsed})
	response, ok := f.responses[method]
	handler := f.handler
	f.mu.Unlock()

	if handler != nil {
		handler(method, parsed, w)
		return
	}
	if !ok {
		response = `{"ok": true, "result": {}}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(response))
}

func (f *fakeBotAPI) recorded() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func newTestAPI(t *testing.T, fake *fakeBotAPI) *API {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return newAPI(srv.URL, "test-token", srv.Client())
}

func TestSendMessageRepliesWithHTML(t *testing.T) {
	fake := newFakeBotAPI()
	fake.responses["sendMessage"] = `{
		"ok": true,
		"result": {
			"message_id": 31,
			"from": {"id": 7, "is_bot": true, "first_name": "mirror"},
			"chat": {"id": -100200, "type": "supergroup"},
			"text": "<b>Filename</b>: <code>file.bin</code>",
			"date": 1700000000
		}
	}`
	api := newTestAPI(t, fake)

	msg, err := api.SendMessage(context.Background(), -100200, "<b>Filename</b>: <code>file.bin</code>", 25)
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	calls := fake.recorded()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls = %+v, want one sendMessage", calls)
	}
	body := calls[0].body
	if body["chat_id"].(float64) != -100200 {
		t.Errorf("chat_id = %v, want -100200", body["chat_id"])
	}
	if body["reply_to_message_id"].(float64) != 25 {
		t.Errorf("reply_to_message_id = %v, want 25", body["reply_to_message_id"])
	}
	if body["parse_mode"] != "HTML" {
		t.Errorf("parse_mode = %v, want HTML", body["parse_mode"])
	}
	if !strings.Contains(body["text"].(string), "file.bin") {
		t.Errorf("text = %v, want the message text", body["text"])
	}

	if msg.MessageID != 31 {
		t.Errorf("MessageID = %d, want 31", msg.MessageID)
	}
	if msg.Chat.ID != -100200 || msg.Chat.Type != "supergroup" {
		t.Errorf("Chat = %+v, want id -100200 type supergroup", msg.Chat)
	}
}

func TestSendMessageWithoutReplyOmitsField(t *testing.T) {
	fake := newFakeBotAPI()
	api := newTestAPI(t, fake)

	if _, err := api.SendMessage(context.Background(), 1, "hello", 0); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	body := fake.recorded()[0].body
	if _, present := body["reply_to_message_id"]; present {
		t.Errorf("reply_to_message_id = %v, want it omitted", body["reply_to_message_id"])
	}
}

func TestEditAndDeleteMessageSendFields(t *testing.T) {
	fake := newFakeBotAPI()
	api := newTestAPI(t, fake)

	if err := api.EditMessageText(context.Background(), -100200, 31, "updated"); err != nil {
		t.Fatalf("EditMessageText() error = %v", err)
	}
	if err := api.DeleteMessage(context.Background(), -100200, 31); err != nil {
		t.Fatalf("DeleteMessage() error = %v", err)
	}

	calls := fake.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want editMessageText and deleteMessage", calls)
	}
	edit := calls[0]
	if edit.method != "editMessageText" || edit.body["message_id"].(float64) != 31 ||
		edit.body["chat_id"].(float64) != -100200 || edit.body["parse_mode"] != "HTML" {
		t.Errorf("edit call = %+v", edit)
	}
	del := calls[1]
	if del.method != "deleteMessage" || del.body["message_id"].(float64) != 31 {
		t.Errorf("delete call = %+v", del)
	}
}

func TestAPIErrorIsReported(t *testing.T) {
	fake := newFakeBotAPI()
	fake.responses["sendMessage"] = `{"ok": false, "error_code": 400, "description": "Bad Request: chat not found"}`
	api := newTestAPI(t, fake)

	_, err := api.SendMessage(context.Background(), 1, "hello", 0)
	if err == nil {
		t.Fatal("SendMessage() error = nil, want the API error")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error = %v, want it to include the API description", err)
	}
}

func TestPollDeliversUpdatesAndAdvancesOffset(t *testing.T) {
	fake := newFakeBotAPI()
	updateJSON := `{"ok": true, "result": [
		{"update_id": 10, "message": {"message_id": 1, "date": 1, "text": "/mirror http://x",
			"chat": {"id": -100200, "type": "supergroup"},
			"from": {"id": 42, "is_bot": false, "first_name": "Kat", "username": "kat"}}}
	]}`

	block := make(chan struct{})
	secondCall := make(chan struct{}, 1)
	var mu sync.Mutex
	calls := 0
	seenOffsets := []float64{}
	fake.handler = func(method string, body map[string]any, w http.ResponseWriter) {
		if method != "getUpdates" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok": true, "result": {}}`))
			return
		}
		mu.Lock()
		calls++
		first := calls == 1
		if offset, ok := body["offset"].(float64); ok {
			seenOffsets = append(seenOffsets, offset)
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if first {
			_, _ = w.Write([]byte(updateJSON))
			return
		}
		// Later calls block until the test cancels polling.
		secondCall <- struct{}{}
		<-block
		_, _ = w.Write([]byte(`{"ok": true, "result": []}`))
	}
	api := newTestAPI(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	received := make(chan Update, 1)
	done := make(chan error, 1)
	go func() {
		done <- api.Poll(ctx, func(_ context.Context, upd Update) { received <- upd })
	}()

	var upd Update
	select {
	case upd = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not deliver the update")
	}
	if upd.UpdateID != 10 || upd.Message == nil || upd.Message.MessageID != 1 {
		t.Fatalf("update = %+v, want the scripted message update", upd)
	}
	if upd.Message.From == nil || upd.Message.From.ID != 42 || upd.Message.From.Username != "kat" {
		t.Fatalf("from = %+v, want the scripted user", upd.Message.From)
	}

	// Wait until the poller issues the next long poll, which must confirm
	// the received update by advancing the offset.
	select {
	case <-secondCall:
	case <-time.After(5 * time.Second):
		t.Fatal("second getUpdates call never arrived")
	}

	cancel()
	close(block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Poll() error = %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll() did not return after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	// The first call omits the offset; the second call must confirm the
	// received update by advancing the offset past it.
	if len(seenOffsets) != 1 || seenOffsets[0] != 11 {
		t.Errorf("offsets = %v, want the next call to use offset 11", seenOffsets)
	}
}

func TestPollStopsOnRepeatedErrorsWithoutCancel(t *testing.T) {
	fake := newFakeBotAPI()
	fake.handler = func(_ string, _ map[string]any, w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok": false, "error_code": 409, "description": "Conflict"}`))
	}
	api := newTestAPI(t, fake)

	// The poller must not spin without pause: give it a tight deadline and
	// confirm it retries with delays instead of returning the error.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := api.Poll(ctx, func(context.Context, Update) {})
	if err != nil {
		t.Fatalf("Poll() error = %v, want nil while retrying", err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
	}
}
