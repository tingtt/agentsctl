package chatgpt

import (
	"strings"
	"testing"
)

func TestInitialSeriesDiagnosticReportsOnlySelectionShape(t *testing.T) {
	seriesA := "raw-series-a"
	seriesB := "raw-series-b"
	captures := []capture{
		capturePage(4, seriesA, "0", "opaque-a", item(conversationA, "private title", "2026-01-01T00:00:00Z")),
		capturePage(2, seriesA, "0", "opaque-b", item(conversationA, "private title", "2026-01-01T00:00:00Z"), item(conversationB, "private title", "2026-01-02T00:00:00Z")),
		capturePage(6, seriesB, "0", "", item(conversationC, "private title", "2026-01-03T00:00:00Z")),
	}

	diagnostic := diagnoseInitialSeries(captures, scrollRegion{Found: true, ProjectLinkCount: 7})
	if diagnostic.CaptureCount != 3 || len(diagnostic.Series) != 2 {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
	formatted := diagnostic.String()
	for _, expected := range []string{
		"captures=3",
		"distinct_series=2",
		"region_found=true",
		"project_links=7",
		fingerprintSeries(seriesA) + " observations=2 first_page_items=2",
		fingerprintSeries(seriesB) + " observations=1 first_page_items=1",
	} {
		if !strings.Contains(formatted, expected) {
			t.Fatalf("diagnostic %q does not contain %q", formatted, expected)
		}
	}
	for _, forbidden := range []string{seriesA, seriesB, conversationA, conversationB, conversationC, "private title", "opaque-a", "opaque-b"} {
		if strings.Contains(formatted, forbidden) {
			t.Fatalf("diagnostic %q exposes %q", formatted, forbidden)
		}
	}
}
