package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestCalendarWindowFromRequestUsesExplicitTimezoneAndInclusiveDates(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/usage?from=2026-07-10&to=2026-07-16&tz=Asia%2FShanghai", nil)
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	start, endExclusive, explicit, err := calendarWindowFromRequest(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if !explicit || start.Format(time.RFC3339) != "2026-07-10T00:00:00+08:00" || endExclusive.Format(time.RFC3339) != "2026-07-17T00:00:00+08:00" {
		t.Fatalf("window = %s to %s explicit=%v", start.Format(time.RFC3339), endExclusive.Format(time.RFC3339), explicit)
	}
}

func TestCalendarWindowFromRequestRejectsIncompleteFutureAndOversizedRanges(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	for _, target := range []string{
		"/api/usage?from=2026-07-10",
		"/api/usage?from=2026-07-17&to=2026-07-17&tz=Asia%2FShanghai",
		"/api/usage?from=2025-01-01&to=2026-07-16&tz=Asia%2FShanghai",
		"/api/usage?from=2026-07-10&to=2026-07-09",
	} {
		request := httptest.NewRequest("GET", target, nil)
		if _, _, _, err := calendarWindowFromRequest(request, now); err == nil {
			t.Fatalf("calendarWindowFromRequest accepted %s", target)
		}
	}
}

func TestDailyActivityWindowUsesDateTimezoneAndBucket(t *testing.T) {
	now := time.Date(2026, 7, 21, 3, 0, 0, 0, time.UTC)
	request := httptest.NewRequest("GET", "/api/activity/daily?date=2026-07-20&tz=Asia%2FShanghai&bucket=30", nil)
	start, endExclusive, bucket, err := dailyActivityWindowFromRequest(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.RFC3339) != "2026-07-20T00:00:00+08:00" || endExclusive.Format(time.RFC3339) != "2026-07-21T00:00:00+08:00" || bucket != 30 {
		t.Fatalf("daily window = %s to %s bucket=%d", start.Format(time.RFC3339), endExclusive.Format(time.RFC3339), bucket)
	}
}

func TestDailyActivityWindowRejectsFutureAndUnsupportedBucket(t *testing.T) {
	now := time.Date(2026, 7, 21, 3, 0, 0, 0, time.UTC)
	for _, target := range []string{
		"/api/activity/daily?date=2026-07-22&tz=Asia%2FShanghai",
		"/api/activity/daily?date=2026-07-21&tz=Asia%2FShanghai&bucket=20",
		"/api/activity/daily?date=bad&tz=Asia%2FShanghai",
	} {
		request := httptest.NewRequest("GET", target, nil)
		if _, _, _, err := dailyActivityWindowFromRequest(request, now); err == nil {
			t.Fatalf("dailyActivityWindowFromRequest accepted %s", target)
		}
	}
}
