package webhook

import (
	"errors"
	"testing"

	"github.com/matryer/is"
)

func TestEventsListsEveryEventOnce(t *testing.T) {
	is := is.New(t)

	events := Events()
	is.Equal(len(events), 6)
	seen := map[Event]bool{}
	for _, ev := range events {
		is.True(!seen[ev])
		seen[ev] = true
		is.True(ev.String() != "")
	}
}

func TestEventStringRoundTrip(t *testing.T) {
	is := is.New(t)

	for _, ev := range Events() {
		parsed, err := ParseEvent(ev.String())
		is.NoErr(err)
		is.Equal(parsed, ev)
	}
}

func TestParseEventRejectsUnknown(t *testing.T) {
	is := is.New(t)

	ev, err := ParseEvent("not_an_event")
	is.True(errors.Is(err, ErrInvalidEvent))
	is.Equal(ev, Event(-1))
}

func TestEventUnmarshalText(t *testing.T) {
	is := is.New(t)

	var ev Event
	is.NoErr(ev.UnmarshalText([]byte("push")))
	is.Equal(ev, EventPush)

	is.True(errors.Is(ev.UnmarshalText([]byte("bogus")), ErrInvalidEvent))
	// A failed unmarshal leaves the value unchanged.
	is.Equal(ev, EventPush)
}

func TestEventMarshalText(t *testing.T) {
	is := is.New(t)

	text, err := EventRepositoryVisibilityChange.MarshalText()
	is.NoErr(err)
	is.Equal(string(text), "repository_visibility_change")

	_, err = Event(0).MarshalText()
	is.True(errors.Is(err, ErrInvalidEvent))
	_, err = Event(99).MarshalText()
	is.True(errors.Is(err, ErrInvalidEvent))
}
