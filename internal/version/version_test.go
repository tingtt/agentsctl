package version

import "testing"

func TestDefaultVersionIsDevelopment(t *testing.T) {
	if Version != "dev" || Development != "dev" {
		t.Fatalf("Version = %q, Development = %q, want dev", Version, Development)
	}
}
