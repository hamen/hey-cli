package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/hey-sdk/go/pkg/generated"
	hey "github.com/basecamp/hey-sdk/go/pkg/hey"
)

type fakeEvents struct {
	calendars []generated.Calendar
	listErr   error
	created   []hey.CreateCalendarEventParams
	createErr error
}

func (f *fakeEvents) Create(_ context.Context, p hey.CreateCalendarEventParams) (*generated.Recording, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, p)
	return &generated.Recording{}, nil
}

func (f *fakeEvents) ListCalendars(context.Context) ([]generated.Calendar, error) {
	return f.calendars, f.listErr
}

// Maybe comes first, as it does in HEY; only the second calendar is a default target.
func defaultCalendars() []generated.Calendar {
	return []generated.Calendar{
		{Id: 1, Name: "Maybe", Owned: true, Kind: "tentative"},
		{Id: 2, Name: "Personal", Owned: true, Personal: true, Kind: "normal"},
		{Id: 3, Name: "Subscribed", Owned: true, External: true, Kind: "normal"},
		{Id: 4, Name: "Work", Owned: true, Kind: "normal"},
	}
}

func newTestEventer(t *testing.T, f *fakeEvents) *eventer {
	t.Helper()
	e, err := newEventer(EventsConfig{Events: f})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCreateEventParams(t *testing.T) {
	tests := []struct {
		name    string
		in      map[string]any
		wantErr string
		check   func(t *testing.T, p hey.CreateCalendarEventParams)
	}{
		{
			name: "no time is an all-day event on the default calendar",
			in:   map[string]any{"title": "Birthday", "starts_on": "2026-12-01"},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if !p.AllDay || p.CalendarID != 4 || p.EndsAt != "2026-12-01" || p.StartTime != "" {
					t.Fatalf("got %+v", p)
				}
			},
		},
		{
			name: "end defaults to an hour after the start, across midnight too",
			in:   map[string]any{"title": "Late", "starts_on": "2026-12-01", "start_time": "23:30", "time_zone": "Europe/Rome"},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if p.AllDay || p.StartTime != "23:30" || p.EndTime != "00:30" || p.EndsAt != "2026-12-02" || p.StartTimeZone != "Europe/Rome" || p.EndTimeZone != "Europe/Rome" {
					t.Fatalf("got %+v", p)
				}
			},
		},
		{
			name: "an explicit calendar skips the lookup",
			in:   map[string]any{"title": "X", "starts_on": "2026-12-01", "calendar_id": float64(9)},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if p.CalendarID != 9 {
					t.Fatalf("got %+v", p)
				}
			},
		},
		{
			name: "an explicit ends_on stays when only the end time is left out",
			in:   map[string]any{"title": "Retreat", "starts_on": "2026-12-01", "ends_on": "2026-12-03", "start_time": "10:00", "time_zone": "Europe/Rome"},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if p.EndsAt != "2026-12-03" || p.EndTime != "11:00" {
					t.Fatalf("got %+v", p)
				}
			},
		},
		{
			name: "an end after midnight works with the next date",
			in:   map[string]any{"title": "Late", "starts_on": "2026-12-01", "ends_on": "2026-12-02", "start_time": "23:30", "end_time": "00:30", "time_zone": "Europe/Rome"},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if p.EndsAt != "2026-12-02" || p.EndTime != "00:30" {
					t.Fatalf("got %+v", p)
				}
			},
		},
		{
			name:    "an end after midnight without the next date is refused",
			in:      map[string]any{"title": "Late", "starts_on": "2026-12-01", "start_time": "23:30", "end_time": "00:30", "time_zone": "Europe/Rome"},
			wantErr: "pass ends_on as the next date",
		},
		{
			name:    "a default end on a clock time that repeats is refused",
			in:      map[string]any{"title": "Fall back", "starts_on": "2026-11-01", "start_time": "01:30", "time_zone": "America/New_York"},
			wantErr: "repeats as the clocks go back",
		},
		{
			name: "notes are escaped into HTML",
			in:   map[string]any{"title": "X", "starts_on": "2026-12-01", "notes": "Discuss <budget>\nBring slides"},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if p.Content.Notes != "Discuss &lt;budget&gt;<br>Bring slides" {
					t.Fatalf("got %q", p.Content.Notes)
				}
			},
		},
		{name: "a non-string note is refused", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "notes": float64(5)}, wantErr: "notes must be a string"},
		{name: "Local is not a zone", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "start_time": "10:00", "time_zone": "Local"}, wantErr: "not an IANA zone"},
		{
			name: "a string calendar id with spaces is trimmed",
			in:   map[string]any{"title": "X", "starts_on": "2026-12-01", "calendar_id": " 9 "},
			check: func(t *testing.T, p hey.CreateCalendarEventParams) {
				if p.CalendarID != 9 {
					t.Fatalf("got %+v", p)
				}
			},
		},
		{name: "title is required", in: map[string]any{"starts_on": "2026-12-01"}, wantErr: "title is required"},
		{name: "bad date", in: map[string]any{"title": "X", "starts_on": "01/12/2026"}, wantErr: "starts_on must be a date"},
		{name: "end date before start", in: map[string]any{"title": "X", "starts_on": "2026-12-02", "ends_on": "2026-12-01"}, wantErr: "ends_on is before"},
		{name: "a clock time needs a zone", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "start_time": "10:00"}, wantErr: "time_zone is required"},
		{name: "an unknown zone is refused", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "start_time": "10:00", "time_zone": "Mars/Base"}, wantErr: "not an IANA zone"},
		{name: "a bad clock time", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "start_time": "25:00", "time_zone": "UTC"}, wantErr: "start_time must be a clock time"},
		{name: "end before start", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "start_time": "10:00", "end_time": "09:00", "time_zone": "UTC"}, wantErr: "ends before it starts"},
		{name: "zone without a start time", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "time_zone": "UTC"}, wantErr: "need a start_time"},
		{name: "an unknown param is refused", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "attendees": []any{"a@b.c"}}, wantErr: `no param "attendees"`},
		{name: "a bad calendar id", in: map[string]any{"title": "X", "starts_on": "2026-12-01", "calendar_id": "abc"}, wantErr: "calendar_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEventer(t, &fakeEvents{calendars: defaultCalendars()})
			p, err := e.createParams(context.Background(), tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, p)
		})
	}
}

func TestCreateEventNeedsADefaultCalendar(t *testing.T) {
	in := map[string]any{"title": "X", "starts_on": "2026-12-01"}

	e := newTestEventer(t, &fakeEvents{calendars: defaultCalendars()[:3]})
	if _, err := e.createParams(context.Background(), in); err == nil || !strings.Contains(err.Error(), "no calendar to file") {
		t.Fatalf("err = %v", err)
	}

	e = newTestEventer(t, &fakeEvents{listErr: errors.New("boom")})
	if _, err := e.createParams(context.Background(), in); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewEventerNeedsEvents(t *testing.T) {
	if _, err := newEventer(EventsConfig{}); err == nil {
		t.Fatal("want an error")
	}
}

// With two calendars that qualify, the first one wins, as in `hey event add`.
func TestCreateEventDefaultsToTheFirstQualifyingCalendar(t *testing.T) {
	cals := append(defaultCalendars(), generated.Calendar{Id: 5, Name: "Side", Owned: true, Kind: "normal"})
	e := newTestEventer(t, &fakeEvents{calendars: cals})
	p, err := e.createParams(context.Background(), map[string]any{"title": "X", "starts_on": "2026-12-01"})
	if err != nil || p.CalendarID != 4 {
		t.Fatalf("calendar = %d, err = %v", p.CalendarID, err)
	}
}

func callEvents(t *testing.T, e *eventer, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.handle(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHandleCreateEvent(t *testing.T) {
	f := &fakeEvents{calendars: defaultCalendars()}
	e := newTestEventer(t, f)

	res := callEvents(t, e, map[string]any{"action": "create_event", "params": map[string]any{"title": "Dentist", "starts_on": "2026-12-01"}})
	if res.IsError || len(f.created) != 1 || f.created[0].Title != "Dentist" || f.created[0].CalendarID != 4 {
		t.Fatalf("error = %v, created = %+v, text = %s", res.IsError, f.created, resultText(t, res))
	}

	res = callEvents(t, e, map[string]any{"action": "create_event", "params": map[string]any{"starts_on": "2026-12-01"}})
	if !res.IsError || len(f.created) != 1 || !strings.Contains(resultText(t, res), "not created") {
		t.Fatalf("bad input must not write: %s", resultText(t, res))
	}

	res = callEvents(t, e, map[string]any{"action": "describe"})
	if res.IsError || !strings.Contains(resultText(t, res), "time_zone") {
		t.Fatalf("describe: %s", resultText(t, res))
	}

	res = callEvents(t, e, map[string]any{"action": "update_event"})
	if !res.IsError || !strings.Contains(resultText(t, res), "unknown action") {
		t.Fatalf("unknown action: %s", resultText(t, res))
	}
}

func TestHandleCreateEventFailureSaysTheOutcomeIsUnknown(t *testing.T) {
	f := &fakeEvents{calendars: defaultCalendars(), createErr: errors.New("timeout")}
	e := newTestEventer(t, f)
	res := callEvents(t, e, map[string]any{"action": "create_event", "params": map[string]any{"title": "Dentist", "starts_on": "2026-12-01"}})
	text := resultText(t, res)
	if !res.IsError || !strings.Contains(text, "outcome is unknown") || !strings.Contains(text, "Check the calendar") {
		t.Fatalf("got %s", text)
	}
}
