package session

import "testing"

func TestChatGPTKeyUsesProviderQualifiedConversationID(t *testing.T) {
	key := Key{Provider: ProviderChatGPT, ID: "conversation-id"}
	if got, want := key.String(), "chatgpt:conversation-id"; got != want {
		t.Fatalf("Key.String() = %q, want %q", got, want)
	}
}
