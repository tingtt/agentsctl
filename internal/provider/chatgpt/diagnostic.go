package chatgpt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type initialSeriesDiagnostic struct {
	CaptureCount int
	Series       []seriesDiagnostic
	RegionFound  bool
	ProjectLinks int
}

type seriesDiagnostic struct {
	Fingerprint    string
	Observations   int
	FirstPageItems int
}

func diagnoseInitialSeries(captures []capture, region scrollRegion) initialSeriesDiagnostic {
	type accumulatedSeries struct {
		fingerprint    string
		observations   int
		firstCaptureID int
		firstPageItems int
	}

	relevant := make(map[string]*accumulatedSeries)
	for _, candidate := range captures {
		if candidate.CursorIn != "0" || candidate.SeriesKey == "" {
			continue
		}
		series := relevant[candidate.SeriesKey]
		if series == nil {
			series = &accumulatedSeries{
				fingerprint:    fingerprintSeries(candidate.SeriesKey),
				firstCaptureID: candidate.CaptureID,
				firstPageItems: len(candidate.Items),
			}
			relevant[candidate.SeriesKey] = series
		}
		series.observations++
		if candidate.CaptureID < series.firstCaptureID {
			series.firstCaptureID = candidate.CaptureID
			series.firstPageItems = len(candidate.Items)
		}
	}

	result := initialSeriesDiagnostic{
		CaptureCount: len(captures),
		RegionFound:  region.Found,
		ProjectLinks: region.ProjectLinkCount,
		Series:       make([]seriesDiagnostic, 0, len(relevant)),
	}
	for _, series := range relevant {
		result.Series = append(result.Series, seriesDiagnostic{
			Fingerprint:    series.fingerprint,
			Observations:   series.observations,
			FirstPageItems: series.firstPageItems,
		})
	}
	sort.Slice(result.Series, func(i, j int) bool {
		return result.Series[i].Fingerprint < result.Series[j].Fingerprint
	})
	return result
}

func (d initialSeriesDiagnostic) String() string {
	var series strings.Builder
	series.WriteByte('[')
	for i, item := range d.Series {
		if i > 0 {
			series.WriteString("; ")
		}
		fmt.Fprintf(&series, "%s observations=%d first_page_items=%d", item.Fingerprint, item.Observations, item.FirstPageItems)
	}
	series.WriteByte(']')
	return fmt.Sprintf(
		"captures=%d distinct_series=%d region_found=%t project_links=%d series=%s",
		d.CaptureCount,
		len(d.Series),
		d.RegionFound,
		d.ProjectLinks,
		series.String(),
	)
}

func fingerprintSeries(seriesKey string) string {
	sum := sha256.Sum256([]byte(seriesKey))
	return hex.EncodeToString(sum[:])[:12]
}
