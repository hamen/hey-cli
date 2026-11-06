package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/basecamp/hey-sdk/go/pkg/generated"
	hey "github.com/basecamp/hey-sdk/go/pkg/hey"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/gateway"

	"github.com/basecamp/hey-cli/internal/timezone"
)

// create_event: file a new calendar event.
//
// hey-sdk's OpenAPI model has no create or update for Calendar Events (they are the
// "phase-2: calendar events" excluded routes), so the generated catalog cannot serve them
// and the tool is written by hand, like hey_send, on top of the SDK's CalendarEvents service.
//
// Deliberately not here yet: update and move (HEY clears whatever an update leaves out, so
// a safe one needs a read first), repeats, reminders, countdowns and guests. Guests are the
// reason for the last one: submitting a guest list sends iCal invitations to other people,
// which belongs behind a confirmation like send_draft, not inside a plain write.

// Events is the slice of hey-sdk create_event drives.
type Events interface {
	Create(ctx context.Context, params hey.CreateCalendarEventParams) (*generated.Recording, error)
	ListCalendars(ctx context.Context) ([]generated.Calendar, error)
}

// EventsConfig serves create_event. Nil serves none.
type EventsConfig struct {
	Events Events
}

const (
	eventsToolName      = "hey_events"
	createEventName     = "create_event"
	calendarKindNormal  = "normal"
	eventsDescription   = "HEY Calendar events: create_event files a new event. Without start_time the event is all-day. With start_time, time_zone (an IANA name such as Europe/Rome, the zone the clock times are written in) is required; end_time defaults to an hour after the start; an event that runs past midnight needs ends_on set to the next date. calendar_id defaults to the calendar HEY itself files new events on. It does not invite guests, set repeats or reminders, or edit existing events."
	maxEventTitleLength = 500
	clockLayout         = "15:04"
)

var createEventParams = []string{"title", "starts_on", "ends_on", "start_time", "end_time", "time_zone", "calendar_id", "notes", "location", "link"}

type eventer struct {
	cfg EventsConfig
}

func newEventer(cfg EventsConfig) (*eventer, error) {
	if cfg.Events == nil {
		return nil, errors.New("create_event needs HEY's calendar events")
	}
	return &eventer{cfg: cfg}, nil
}

func (e *eventer) tool() *mcp.Tool {
	destructive, openWorld := false, false
	return &mcp.Tool{
		Name:        eventsToolName,
		Title:       "HEY calendar events",
		Description: eventsDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": []any{createEventName, gateway.DescribeAction}},
				"params": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title":       map[string]any{"type": "string", "description": "The event's title."},
						"starts_on":   map[string]any{"type": "string", "description": "Start date, YYYY-MM-DD."},
						"ends_on":     map[string]any{"type": "string", "description": "End date, YYYY-MM-DD. Defaults to starts_on."},
						"start_time":  map[string]any{"type": "string", "description": "Start clock time, HH:MM. Leave out for an all-day event."},
						"end_time":    map[string]any{"type": "string", "description": "End clock time, HH:MM. Defaults to an hour after start_time. For an end after midnight, also set ends_on to the next date."},
						"time_zone":   map[string]any{"type": "string", "description": "IANA zone the clock times are written in. Required with start_time."},
						"calendar_id": map[string]any{"type": []any{"integer", "string"}, "description": "Calendar to file the event on (an integer id). Defaults to HEY's own default."},
						"notes":       map[string]any{"type": "string", "description": "Notes for the event. Plain text."},
						"location":    map[string]any{"type": "string", "description": "Where it happens."},
						"link":        map[string]any{"type": "string", "description": "A URL for the event."},
					},
				},
			},
			"required": []any{"action"},
		},
		Annotations: &mcp.ToolAnnotations{
			Title:           "HEY calendar events",
			ReadOnlyHint:    false,
			DestructiveHint: &destructive,
			IdempotentHint:  false,
			OpenWorldHint:   &openWorld,
		},
	}
}

func (e *eventer) handle(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Action string         `json:"action"`
		Params map[string]any `json:"params"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return gateway.ErrorResult("invalid arguments: %v", err), nil
		}
	}
	switch args.Action {
	case gateway.DescribeAction:
		return gateway.JSONResult(map[string]any{
			"action":      createEventName,
			"description": eventsDescription,
			"params": map[string]any{
				"title":       "string (required)",
				"starts_on":   "YYYY-MM-DD (required)",
				"ends_on":     "YYYY-MM-DD",
				"start_time":  "HH:MM; leave out for an all-day event",
				"end_time":    "HH:MM",
				"time_zone":   "IANA zone name; required with start_time",
				"calendar_id": "integer or numeric string",
				"notes":       "string",
				"location":    "string",
				"link":        "string (URL)",
			},
		})
	case createEventName:
	default:
		return gateway.ErrorResult("unknown action %q in %s (actions: %s, %s)", args.Action, eventsToolName, createEventName, gateway.DescribeAction), nil
	}

	params, err := e.createParams(ctx, args.Params)
	if err != nil {
		return gateway.ErrorResult("not created: %v", err), nil
	}
	event, err := e.cfg.Events.Create(ctx, params)
	if err != nil {
		// The write may have reached HEY before the call failed, so a retry could file a second
		// event. The caller has to look before it tries again.
		return gateway.ErrorResult("create_event failed and the outcome is unknown: %v. Check the calendar before you try again, so the event is not filed twice", err), nil
	}
	return gateway.JSONResult(event)
}

// createParams validates the model's input and turns it into the SDK's. Everything a
// guess would otherwise fill in is refused instead: a wrong zone files the event at the
// wrong hour without any error.
func (e *eventer) createParams(ctx context.Context, in map[string]any) (hey.CreateCalendarEventParams, error) {
	var p hey.CreateCalendarEventParams

	for key, value := range in {
		if !slices.Contains(createEventParams, key) {
			return p, fmt.Errorf("create_event takes no param %q (params: %s)", key, strings.Join(createEventParams, ", "))
		}
		if _, isString := value.(string); !isString && value != nil && key != "calendar_id" {
			return p, fmt.Errorf("%s must be a string", key)
		}
	}
	title := strings.TrimSpace(stringParam(in, "title"))
	switch {
	case title == "":
		return p, errors.New("title is required")
	case utf8.RuneCountInString(title) > maxEventTitleLength:
		return p, fmt.Errorf("title is over %d characters", maxEventTitleLength)
	}
	startsOn := stringParam(in, "starts_on")
	if _, err := time.Parse(time.DateOnly, startsOn); err != nil {
		return p, errors.New("starts_on must be a date, YYYY-MM-DD")
	}
	endsOn := stringParam(in, "ends_on")
	if endsOn == "" {
		endsOn = startsOn
	}
	endDate, err := time.Parse(time.DateOnly, endsOn)
	if err != nil {
		return p, errors.New("ends_on must be a date, YYYY-MM-DD")
	}
	startDate, _ := time.Parse(time.DateOnly, startsOn)
	if endDate.Before(startDate) {
		return p, errors.New("ends_on is before starts_on")
	}

	p.Title = title
	p.StartsAt = startsOn
	p.EndsAt = endsOn
	p.Content = hey.EventContentParams{
		Notes:    notesHTML(stringParam(in, "notes")),
		Location: stringParam(in, "location"),
		Link:     stringParam(in, "link"),
	}

	if link := stringParam(in, "link"); link != "" && !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		return p, errors.New("link must be an http or https URL")
	}

	startTime, endTime, zone := stringParam(in, "start_time"), stringParam(in, "end_time"), stringParam(in, "time_zone")
	if startTime == "" {
		if endTime != "" || zone != "" {
			return p, errors.New("end_time and time_zone need a start_time; leave all three out for an all-day event")
		}
		p.AllDay = true
	} else {
		if zone == "" {
			return p, errors.New("time_zone is required with start_time (an IANA name such as Europe/Rome)")
		}
		loc, zoneErr := timezone.Load(zone)
		if zoneErr != nil {
			return p, fmt.Errorf("time_zone %q is not an IANA zone name", zone)
		}
		clock, clockErr := time.Parse(clockLayout, startTime)
		if clockErr != nil {
			return p, errors.New("start_time must be a clock time, HH:MM")
		}
		// The instants are placed the way HEY places a wall time, not the way Go does: the
		// two differ across a change of the clocks.
		start := timezone.WallClock(startDate, clock, loc)
		if start.In(loc).Format(clockLayout) != startTime || start.In(loc).Format(time.DateOnly) != startsOn {
			return p, fmt.Errorf("%s %s does not exist in %s (the clocks skip it); pick another start_time", startsOn, startTime, zone)
		}
		switch {
		case endTime != "":
			if _, endErr := time.Parse(clockLayout, endTime); endErr != nil {
				return p, errors.New("end_time must be a clock time, HH:MM")
			}
		case stringParam(in, "ends_on") != "":
			// A caller-supplied end date stays; only the clock time defaults.
			endTime = clock.Add(time.Hour).Format(clockLayout)
		default:
			end := start.Add(time.Hour).In(loc)
			if placed := timezone.WallClock(end, end, loc); !placed.Equal(end) {
				return p, errors.New("an hour after the start falls on a clock time that repeats as the clocks go back; pass end_time")
			}
			p.EndsAt, endTime = end.Format(time.DateOnly), end.Format(clockLayout)
		}
		end, ok := timezone.Placed(p.EndsAt, endTime, loc)
		if !ok || !end.After(start) {
			return p, errors.New("the event ends before it starts; for an end after midnight, pass ends_on as the next date")
		}
		p.StartTime, p.EndTime = startTime, endTime
		p.StartTimeZone, p.EndTimeZone = zone, zone
	}

	if raw, ok := in["calendar_id"]; ok && raw != nil {
		id, idErr := idParam(raw)
		if idErr != nil {
			return p, fmt.Errorf("calendar_id: %w", idErr)
		}
		p.CalendarID = id
		return p, nil
	}
	calendars, err := e.cfg.Events.ListCalendars(ctx)
	if err != nil {
		return p, fmt.Errorf("look up the default calendar: %w", err)
	}
	for _, c := range calendars {
		if isDefaultEventCalendar(c) {
			p.CalendarID = c.Id
			return p, nil
		}
	}
	return p, errors.New("no calendar to file the event on; pass calendar_id (list_calendars lists them)")
}

// isDefaultEventCalendar is whether HEY would file a new event here when none is named: an
// owned, ordinary calendar that is neither the personal one (it answers 404) nor a
// subscription. The first owned calendar is the wrong one, because every account has Maybe
// before it.
func isDefaultEventCalendar(c generated.Calendar) bool {
	return c.Owned && !c.Personal && !c.External && c.Kind == calendarKindNormal
}

// notesHTML turns plain text into the HTML HEY's Trix field takes, so "<budget>" and line
// breaks survive.
func notesHTML(text string) string {
	if text == "" {
		return ""
	}
	return strings.ReplaceAll(html.EscapeString(text), "\n", "<br>")
}

func stringParam(in map[string]any, key string) string {
	s, _ := in[key].(string)
	return strings.TrimSpace(s)
}

func idParam(raw any) (int64, error) {
	var id int64
	switch v := raw.(type) {
	case float64:
		id = int64(v)
		if float64(id) != v {
			id = 0
		}
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("must be a calendar's id, got %v", raw)
		}
		id = parsed
	}
	if id <= 0 {
		return 0, fmt.Errorf("must be a calendar's id, got %v", raw)
	}
	return id, nil
}
