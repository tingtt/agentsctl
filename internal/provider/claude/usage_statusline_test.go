package claude

import "testing"

// TestParseStatusLinePayloadMapsFiveHourAndSevenDay fixes the core mapping
// confirmed against the installed CLI's own statusLine documentation:
// rate_limits.five_hour -> FiveHour, rate_limits.seven_day -> Weekly.
func TestParseStatusLinePayloadMapsFiveHourAndSevenDay(t *testing.T) {
	raw := []byte(`{"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1738425600},"seven_day":{"used_percentage":41.2,"resets_at":1738857600}}}`)
	snap, err := parseStatusLinePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.FiveHour.Available || snap.FiveHour.Percent != 24 || snap.FiveHour.ResetAt.Unix() != 1738425600 {
		t.Fatalf("FiveHour=%+v, want Available/24%%(rounded)/reset 1738425600", snap.FiveHour)
	}
	if !snap.Weekly.Available || snap.Weekly.Percent != 41 || snap.Weekly.ResetAt.Unix() != 1738857600 {
		t.Fatalf("Weekly=%+v, want Available/41%%/reset 1738857600", snap.Weekly)
	}
}

// TestParseStatusLinePayloadMissingRateLimitsIsUnavailableNotZero fixes
// that a payload with no rate_limits object at all (Free plan account, or
// before the session's first API response, per the CLI's own docs) yields
// both windows unavailable, not a parse error and not a false 0%.
func TestParseStatusLinePayloadMissingRateLimitsIsUnavailableNotZero(t *testing.T) {
	snap, err := parseStatusLinePayload([]byte(`{"model":{"display_name":"Opus"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if snap.FiveHour.Available || snap.Weekly.Available {
		t.Fatalf("snap=%+v, want both windows unavailable when rate_limits is absent", snap)
	}
}

// TestParseStatusLinePayloadWindowIndependentlyAbsent fixes that each
// window is independently optional (the CLI's docs: "Claude Code drops a
// window once its resets_at time passes") -- one window reporting must not
// force-report the other.
func TestParseStatusLinePayloadWindowIndependentlyAbsent(t *testing.T) {
	raw := []byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1000}}}`)
	snap, err := parseStatusLinePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.FiveHour.Available {
		t.Fatalf("FiveHour=%+v, want Available", snap.FiveHour)
	}
	if snap.Weekly.Available {
		t.Fatalf("Weekly=%+v, want Available=false when seven_day is absent", snap.Weekly)
	}
}

// TestParseStatusLinePayloadZeroPercentIsAvailable fixes that a genuinely
// reported 0% used_percentage is Available=true, Percent=0 -- distinct
// from an absent window, never conflated.
func TestParseStatusLinePayloadZeroPercentIsAvailable(t *testing.T) {
	raw := []byte(`{"rate_limits":{"five_hour":{"used_percentage":0,"resets_at":1000}}}`)
	snap, err := parseStatusLinePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.FiveHour.Available || snap.FiveHour.Percent != 0 {
		t.Fatalf("FiveHour=%+v, want Available=true/Percent=0 for a genuinely reported 0%%", snap.FiveHour)
	}
}

// TestParseStatusLinePayloadRejectsMalformedJSON fixes that unparseable
// stdin surfaces as an error rather than a silently-empty snapshot.
func TestParseStatusLinePayloadRejectsMalformedJSON(t *testing.T) {
	if _, err := parseStatusLinePayload([]byte("not json")); err == nil {
		t.Fatal("malformed payload was accepted")
	}
}
