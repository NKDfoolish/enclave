package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	chatapp "enclave/api/internal/chat/app"
	chatdomain "enclave/api/internal/chat/domain"
)

// fakeAnswerer stands in for the RAG service. Each test scripts what it answers and reads
// back what it was asked.
type fakeAnswerer struct {
	mu       sync.Mutex
	requests []chatapp.AnswerRequest
	// answer builds the events for a request; nil answers "I don't know" with no sources.
	answer func(req chatapp.AnswerRequest) []chatapp.AnswerEvent
	// failAfter, when set, makes Answer return err after that many events.
	failAfter int
	err       error
}

func (f *fakeAnswerer) Answer(_ context.Context, req chatapp.AnswerRequest, onEvent func(chatapp.AnswerEvent) error) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	answer, failAfter, failure := f.answer, f.failAfter, f.err
	f.mu.Unlock()

	events := []chatapp.AnswerEvent{
		{Kind: chatapp.EventSources, Sources: []chatdomain.Citation{}},
		{Kind: chatapp.EventDelta, Delta: "Not enough information."},
		{Kind: chatapp.EventDone},
	}
	if answer != nil {
		events = answer(req)
	}
	for i, event := range events {
		if failure != nil && i == failAfter {
			return failure
		}
		if err := onEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeAnswerer) script(answer func(chatapp.AnswerRequest) []chatapp.AnswerEvent, failAfter int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer, f.failAfter, f.err = answer, failAfter, err
}

func (f *fakeAnswerer) last() chatapp.AnswerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// citing answers with one source, the given document, and cites it.
func citing(documentID string) func(chatapp.AnswerRequest) []chatapp.AnswerEvent {
	return func(chatapp.AnswerRequest) []chatapp.AnswerEvent {
		source := chatdomain.Citation{Number: 1, DocumentID: uuid.MustParse(documentID), Title: "Refund policy", Page: 2, Snippet: "Refunds take 14 days."}
		return []chatapp.AnswerEvent{
			{Kind: chatapp.EventSources, Sources: []chatdomain.Citation{source}},
			{Kind: chatapp.EventDelta, Delta: "Refunds take "},
			{Kind: chatapp.EventDelta, Delta: "14 days [1]."},
			{Kind: chatapp.EventDone, Cited: []int{1}},
		}
	}
}

type sseEvent struct {
	name string
	data map[string]any
}

// ask posts a question and reads the Server-Sent Events, or the JSON error if the answer
// never started.
func (u *testUser) ask(workspaceID, conversationID, question string) (int, []sseEvent, map[string]any) {
	u.t.Helper()
	payload, _ := json.Marshal(map[string]string{"content": question})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		u.env.server.URL+conversationPath(workspaceID, conversationID)+"/messages", bytes.NewReader(payload))
	if err != nil {
		u.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, nil, body
	}
	var (
		events  []sseEvent
		current sseEvent
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			current.name = name
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if err := json.Unmarshal([]byte(data), &current.data); err != nil {
				u.t.Fatalf("decode event %s: %v", current.name, err)
			}
			events = append(events, current)
			current = sseEvent{}
		}
	}
	return resp.StatusCode, events, nil
}

func conversationPath(workspaceID, conversationID string) string {
	return "/v1/workspaces/" + workspaceID + "/conversations/" + conversationID
}

func (u *testUser) startConversation(workspaceID string) string {
	u.t.Helper()
	status, body := u.do(http.MethodPost, "/v1/workspaces/"+workspaceID+"/conversations", nil)
	wantStatus(u.t, "start conversation", status, http.StatusCreated, body)
	return body["id"].(string)
}

func (u *testUser) conversation(workspaceID, conversationID string) []map[string]any {
	u.t.Helper()
	status, body := u.do(http.MethodGet, conversationPath(workspaceID, conversationID), nil)
	wantStatus(u.t, "get conversation", status, http.StatusOK, body)
	var messages []map[string]any
	for _, m := range body["messages"].([]any) {
		messages = append(messages, m.(map[string]any))
	}
	return messages
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.name
	}
	return names
}

func TestConversations(t *testing.T) {
	env := newTestEnv(t)
	admin := env.newUser(t, "admin")
	staff := env.newUser(t, "staff")
	client := env.newUser(t, "client")
	outsider := env.newUser(t, "outsider")
	workspaceID := admin.createWorkspace("Q&A")
	admin.addMember(workspaceID, staff, "staff")
	admin.addMember(workspaceID, client, "client")

	doc := admin.uploadDocument(workspaceID, "refunds.md", []byte("# Refunds\nRefunds take 14 days."))
	documentID := doc["id"].(string)
	status, body := admin.do(http.MethodPatch, documentPath(workspaceID, documentID), map[string]string{"visibility": "client_common"})
	wantStatus(t, "share document", status, http.StatusOK, body)

	t.Run("an answer streams as sources, deltas, done and is stored with its citations", func(t *testing.T) {
		env.answerer.script(citing(documentID), 0, nil)
		conversationID := client.startConversation(workspaceID)

		status, events, _ := client.ask(workspaceID, conversationID, "  How long do refunds take?  ")

		if status != http.StatusOK || strings.Join(eventNames(events), ",") != "sources,delta,delta,done" {
			t.Fatalf("status %d events %v", status, eventNames(events))
		}
		done := events[3].data["message"].(map[string]any)
		if done["content"] != "Refunds take 14 days [1]." || done["grounded"] != true {
			t.Fatalf("stored answer = %v", done)
		}
		messages := client.conversation(workspaceID, conversationID)
		if len(messages) != 2 || messages[0]["content"] != "How long do refunds take?" {
			t.Fatalf("messages = %v", messages)
		}
		citation := messages[1]["citations"].([]any)[0].(map[string]any)
		if citation["document_id"] != documentID || citation["page"] != float64(2) {
			t.Fatalf("citation = %v", citation)
		}
		_, body := client.do(http.MethodGet, "/v1/workspaces/"+workspaceID+"/conversations", nil)
		if title := body["conversations"].([]any)[0].(map[string]any)["title"]; title != "How long do refunds take?" {
			t.Fatalf("title = %v; the first question names the conversation", title)
		}
	})

	t.Run("each role searches only the visibilities it may read", func(t *testing.T) {
		env.answerer.script(nil, 0, nil)
		client.ask(workspaceID, client.startConversation(workspaceID), "Salaries?")
		if got := env.answerer.last().Visibilities; len(got) != 1 || got[0] != "client_common" {
			t.Fatalf("client visibilities = %v", got)
		}
		staff.ask(workspaceID, staff.startConversation(workspaceID), "Salaries?")
		if got := env.answerer.last().Visibilities; len(got) != 2 {
			t.Fatalf("staff visibilities = %v", got)
		}
	})

	t.Run("conversations are private to their owner", func(t *testing.T) {
		conversationID := client.startConversation(workspaceID)
		for _, other := range []*testUser{admin, staff, outsider} {
			status, _ := other.do(http.MethodGet, conversationPath(workspaceID, conversationID), nil)
			if status != http.StatusNotFound {
				t.Fatalf("%s read another member's conversation: status %d", other.Email, status)
			}
			status, _, _ = other.ask(workspaceID, conversationID, "hi")
			if status != http.StatusNotFound {
				t.Fatalf("%s asked in another member's conversation: status %d", other.Email, status)
			}
		}
		status, _ := outsider.do(http.MethodPost, "/v1/workspaces/"+workspaceID+"/conversations", nil)
		if status != http.StatusNotFound {
			t.Fatalf("outsider started a conversation: status %d", status)
		}
	})

	t.Run("earlier turns are sent as history", func(t *testing.T) {
		env.answerer.script(citing(documentID), 0, nil)
		conversationID := client.startConversation(workspaceID)
		client.ask(workspaceID, conversationID, "How long do refunds take?")
		client.ask(workspaceID, conversationID, "And for sale items?")

		history := env.answerer.last().History
		if len(history) != 2 || history[0].Content != "How long do refunds take?" || history[1].Content != "Refunds take 14 days [1]." {
			t.Fatalf("history = %+v", history)
		}
	})

	t.Run("answers citing a document the reader lost access to are hidden and not sent as history", func(t *testing.T) {
		env.answerer.script(citing(documentID), 0, nil)
		conversationID := client.startConversation(workspaceID)
		client.ask(workspaceID, conversationID, "How long do refunds take?")

		status, body := admin.do(http.MethodPatch, documentPath(workspaceID, documentID), map[string]string{"visibility": "internal"})
		wantStatus(t, "unshare", status, http.StatusOK, body)
		t.Cleanup(func() {
			admin.do(http.MethodPatch, documentPath(workspaceID, documentID), map[string]string{"visibility": "client_common"})
		})

		messages := client.conversation(workspaceID, conversationID)
		answer := messages[1]
		if answer["hidden"] != true || strings.Contains(answer["content"].(string), "14 days") || len(answer["citations"].([]any)) != 0 {
			t.Fatalf("answer after unshare = %v; its text quotes a document the client can no longer read", answer)
		}

		env.answerer.script(nil, 0, nil)
		client.ask(workspaceID, conversationID, "Anything else?")
		for _, turn := range env.answerer.last().History {
			if strings.Contains(turn.Content, "14 days") {
				t.Fatal("a hidden answer was sent to the model as history")
			}
		}
	})

	t.Run("uncited sources and inherited sources are checked on reread", func(t *testing.T) {
		second := admin.uploadDocument(workspaceID, "terms.md", []byte("# Terms"))
		secondID := second["id"].(string)
		status, body := admin.do(http.MethodPatch, documentPath(workspaceID, secondID), map[string]string{"visibility": "client_common"})
		wantStatus(t, "share second document", status, http.StatusOK, body)
		conversationID := client.startConversation(workspaceID)
		env.answerer.script(func(req chatapp.AnswerRequest) []chatapp.AnswerEvent {
			events := citing(documentID)(req)
			events[2].Delta = "14 days."
			events[3].Cited = nil
			return events
		}, 0, nil)
		client.ask(workspaceID, conversationID, "Refunds?")
		env.answerer.script(citing(secondID), 0, nil)
		client.ask(workspaceID, conversationID, "What other terms apply?")
		status, body = admin.do(http.MethodPatch, documentPath(workspaceID, documentID), map[string]string{"visibility": "internal"})
		wantStatus(t, "unshare original", status, http.StatusOK, body)
		t.Cleanup(func() {
			admin.do(http.MethodPatch, documentPath(workspaceID, documentID), map[string]string{"visibility": "client_common"})
		})
		messages := client.conversation(workspaceID, conversationID)
		if len(messages) != 4 || messages[1]["hidden"] != true || messages[3]["hidden"] != true {
			t.Fatalf("expected both replies to be hidden: %v", messages)
		}
	})

	t.Run("RAG failure before the answer starts is a JSON error", func(t *testing.T) {
		env.answerer.script(nil, 0, chatdomain.ErrAnswerUnavailable)
		conversationID := client.startConversation(workspaceID)

		status, events, body := client.ask(workspaceID, conversationID, "Refunds?")

		if status != http.StatusServiceUnavailable || events != nil || errorCode(body) != "answer_unavailable" {
			t.Fatalf("status %d body %v", status, body)
		}
		if messages := client.conversation(workspaceID, conversationID); len(messages) != 1 {
			t.Fatalf("messages = %v; only the question is stored", messages)
		}
	})

	t.Run("failure after the answer started ends the stream with an error event", func(t *testing.T) {
		env.answerer.script(citing(documentID), 2, chatdomain.ErrAnswerUnavailable)
		conversationID := client.startConversation(workspaceID)

		status, events, _ := client.ask(workspaceID, conversationID, "Refunds?")

		if status != http.StatusOK || strings.Join(eventNames(events), ",") != "sources,delta,error" {
			t.Fatalf("status %d events %v", status, eventNames(events))
		}
		if messages := client.conversation(workspaceID, conversationID); len(messages) != 1 {
			t.Fatalf("messages = %v; a half answer is not stored", messages)
		}
	})

	t.Run("empty questions are rejected", func(t *testing.T) {
		status, _, body := client.ask(workspaceID, client.startConversation(workspaceID), "   ")
		if status != http.StatusBadRequest || errorCode(body) != "invalid_question" {
			t.Fatalf("status %d body %v", status, body)
		}
	})

	t.Run("deleting a conversation", func(t *testing.T) {
		conversationID := client.startConversation(workspaceID)
		if status, _ := staff.do(http.MethodDelete, conversationPath(workspaceID, conversationID), nil); status != http.StatusNotFound {
			t.Fatalf("another member deleted it: status %d", status)
		}
		if status, _ := client.do(http.MethodDelete, conversationPath(workspaceID, conversationID), nil); status != http.StatusNoContent {
			t.Fatalf("owner delete: status %d", status)
		}
	})
}

func TestSearchIndexFollowsDocuments(t *testing.T) {
	env := newTestEnv(t)
	admin := env.newUser(t, "admin")
	workspaceID := admin.createWorkspace("Index")

	doc := admin.uploadDocument(workspaceID, "notes.md", []byte("# Notes\nSome text."))
	documentID := doc["id"].(string)
	id := uuid.MustParse(documentID)

	if doc["index_status"] != "pending" || env.searchIndex.count(id) != 1 {
		t.Fatalf("after upload: index_status %v, sync requests %d; a ready document is queued for indexing",
			doc["index_status"], env.searchIndex.count(id))
	}

	// The RAG service records its result; the API shows it.
	if _, err := env.pool.Exec(context.Background(),
		`INSERT INTO document_index (document_id, status, embedding_model, chunk_count) VALUES ($1, 'indexed', 'embed-local', 3)`, id); err != nil {
		t.Fatal(err)
	}
	_, body := admin.do(http.MethodGet, documentPath(workspaceID, documentID), nil)
	if body["index_status"] != "indexed" {
		t.Fatalf("index_status = %v", body["index_status"])
	}

	admin.do(http.MethodPatch, documentPath(workspaceID, documentID), map[string]string{"visibility": "client_common"})
	env.runJobs(t)
	if env.searchIndex.count(id) != 2 {
		t.Fatalf("sync requests after sharing = %d; a visibility change must reach the index", env.searchIndex.count(id))
	}

	admin.do(http.MethodDelete, documentPath(workspaceID, documentID), nil)
	env.runJobs(t)
	if env.searchIndex.count(id) != 3 {
		t.Fatalf("sync requests after delete = %d; a delete must reach the index", env.searchIndex.count(id))
	}
}
