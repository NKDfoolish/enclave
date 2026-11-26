package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHideUnreadable(t *testing.T) {
	readableDoc, lostDoc := uuid.New(), uuid.New()
	messages := []Message{
		{Role: RoleUser, Content: "question"},
		{Role: RoleAssistant, Content: "from readable", SourceDocumentIDs: []uuid.UUID{readableDoc}, Citations: []Citation{{DocumentID: readableDoc}}},
		{Role: RoleAssistant, Content: "mixed", SourceDocumentIDs: []uuid.UUID{readableDoc, lostDoc}, Citations: []Citation{{DocumentID: readableDoc}}},
		{Role: RoleAssistant, Content: "no sources"},
	}

	got := HideUnreadable(messages, map[uuid.UUID]bool{readableDoc: true})

	hidden := [4]bool{got[0].Hidden, got[1].Hidden, got[2].Hidden, got[3].Hidden}
	if hidden != [4]bool{false, false, true, false} {
		t.Fatalf("hidden = %v; only an answer citing a lost document is hidden", hidden)
	}
	if messages[2].Hidden {
		t.Fatal("the input must not be modified")
	}
}

func TestCiteSources(t *testing.T) {
	sources := []Citation{{Number: 1, Title: "A"}, {Number: 2, Title: "B"}, {Number: 3, Title: "C"}}

	got := CiteSources(sources, []int{3, 1, 9})

	if len(got) != 2 || got[0].Title != "C" || got[1].Title != "A" {
		t.Fatalf("got %+v; cited sources in citation order, unknown numbers dropped", got)
	}
	if CiteSources(sources, nil) != nil {
		t.Fatal("an answer citing nothing has no citations")
	}
}

func TestCleanQuestionAndTitle(t *testing.T) {
	if _, err := CleanQuestion("   "); !errors.Is(err, ErrInvalidQuestion) {
		t.Fatalf("blank question: err = %v", err)
	}
	if _, err := CleanQuestion(strings.Repeat("a", MaxQuestionLength+1)); !errors.Is(err, ErrInvalidQuestion) {
		t.Fatalf("long question: err = %v", err)
	}
	if q, _ := CleanQuestion("  Hoàn tiền?  "); q != "Hoàn tiền?" {
		t.Fatalf("question = %q", q)
	}

	long := strings.Repeat("chính sách ", 20)
	title := TitleFrom(long)
	if !strings.HasSuffix(title, "…") || len([]rune(title)) > titleLength+1 {
		t.Fatalf("title = %q; long questions are cut on characters, not bytes", title)
	}
	if TitleFrom("Refunds\n  in   March?") != "Refunds in March?" {
		t.Fatal("whitespace in titles is collapsed")
	}
}
