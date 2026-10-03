package work

import "testing"

func TestSnapshotCanonicalization(t *testing.T) {
	wantBody, wantDigest := Snapshot("hello\nworld")
	for _, input := range []string{"hello\nworld", "hello\r\nworld", " \nhello\nworld\n\t"} {
		body, digest := Snapshot(input)
		if body != wantBody || digest != wantDigest {
			t.Fatalf("Snapshot(%q)=(%q,%q), want (%q,%q)", input, body, digest, wantBody, wantDigest)
		}
	}
	if wantDigest != "sha256:26c60a61d01db5836ca70fefd44a6a016620413c8ef5f259a6c5612d4f79d3b8" {
		t.Fatalf("digest=%q", wantDigest)
	}
}

func TestPromptSnapshotExcludesMetadata(t *testing.T) {
	body, digest, err := PromptSnapshot(RenderPrompt(" body ", Prompt{}))
	if err != nil {
		t.Fatal(err)
	}
	wantBody, wantDigest := Snapshot("body")
	if body != wantBody || digest != wantDigest {
		t.Fatalf("got (%q,%q), want (%q,%q)", body, digest, wantBody, wantDigest)
	}
}
