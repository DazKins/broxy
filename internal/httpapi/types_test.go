package httpapi

import "testing"

func TestNormalizeMessages(t *testing.T) {
	req := []ChatMessage{
		{Role: "system", Content: []byte(`"You are terse."`)},
		{Role: "user", Content: []byte(`"hello"`)}}
	chat, system, err := normalizeMessages(req)
	if err != nil {
		t.Fatalf("normalizeMessages() error = %v", err)
	}
	if len(system) != 1 || system[0] != "You are terse." {
		t.Fatalf("system = %#v", system)
	}
	if len(chat) != 1 || chat[0].Content != "hello" {
		t.Fatalf("chat = %#v", chat)
	}
}

func TestMessageTextArray(t *testing.T) {
	got, err := messageText([]byte(`[{"type":"text","text":"hello "},{"type":"text","text":"world"}]`))
	if err != nil {
		t.Fatalf("messageText() error = %v", err)
	}
	if got != "hello world" {
		t.Fatalf("messageText() = %q", got)
	}
}
