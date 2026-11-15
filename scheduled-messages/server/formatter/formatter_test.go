package formatter

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/server/constants"
)

func TestFormatScheduleSuccess(t *testing.T) {
	ts := time.Date(2025, time.January, 2, 15, 4, 0, 0, time.UTC)
	tz := "UTC"
	channel := "in channel: ~town-square"

	t.Run("top-level message", func(t *testing.T) {
		expected := fmt.Sprintf("%s Scheduled message for %s (%s) %s", constants.EmojiSuccess, ts.Format(constants.TimeLayout), tz, channel)

		got := FormatScheduleSuccess(ts, tz, channel, false)
		if got != expected {
			t.Fatalf("FormatScheduleSuccess() = %q, want %q", got, expected)
		}
	})

	t.Run("threaded message", func(t *testing.T) {
		expected := fmt.Sprintf("%s Scheduled message for %s (%s) %s (thread)", constants.EmojiSuccess, ts.Format(constants.TimeLayout), tz, channel)

		got := FormatScheduleSuccess(ts, tz, channel, true)
		if got != expected {
			t.Fatalf("FormatScheduleSuccess() = %q, want %q", got, expected)
		}
	})
}

func TestFormatEmptyCommandError(t *testing.T) {
	helpCommand := fmt.Sprintf("/%s %s", constants.CommandTrigger, constants.SubcommandHelp)
	expected := fmt.Sprintf(constants.EmptyScheduleMessage, helpCommand)

	got := FormatEmptyCommandError()
	if got != expected {
		t.Fatalf("FormatEmptyCommandError() = %q, want %q", got, expected)
	}
}

func TestFormatScheduleValidationError(t *testing.T) {
	errVal := errors.New("validation failure")
	expected := fmt.Sprintf("%s Error scheduling message: %v", constants.EmojiError, errVal)

	got := FormatScheduleValidationError(errVal)
	if got != expected {
		t.Fatalf("FormatScheduleValidationError() = %q, want %q", got, expected)
	}
}

func TestFormatScheduleError(t *testing.T) {
	ts := time.Date(2025, time.January, 2, 15, 4, 0, 0, time.UTC)
	tz := "UTC"
	channel := "in channel: ~town-square"
	errVal := errors.New("store failure")

	expected := fmt.Sprintf("%s Error scheduling message for %s (%s) %s:  %v", constants.EmojiError, ts.Format(constants.TimeLayout), tz, channel, errVal)

	got := FormatScheduleError(ts, tz, channel, errVal)
	if got != expected {
		t.Fatalf("FormatScheduleError() = %q, want %q", got, expected)
	}
}

func TestFormatSchedulerFailure(t *testing.T) {
	channel := "in channel: ~town-square"
	postErr := errors.New("post failure")
	orig := "hello world"

	expected := fmt.Sprintf("%s Error scheduling message %s: %v -- original message: %s", constants.EmojiError, channel, postErr, orig)

	got := FormatSchedulerFailure(channel, postErr, orig)
	if got != expected {
		t.Fatalf("FormatSchedulerFailure() = %q, want %q", got, expected)
	}
}

func TestFormatListAttachmentHeader(t *testing.T) {
	ts := time.Date(2025, time.January, 2, 15, 4, 0, 0, time.UTC)
	channel := "in channel: ~town-square"
	msg := "hello world"

	t.Run("top-level message", func(t *testing.T) {
		expected := fmt.Sprintf("##### %s\n%s\n\n%s", ts.Format(constants.TimeLayout), channel, msg)

		got := FormatListAttachmentHeader(ts, channel, msg, false)
		if got != expected {
			t.Fatalf("FormatListAttachmentHeader() = %q, want %q", got, expected)
		}
	})

	t.Run("threaded message", func(t *testing.T) {
		expected := fmt.Sprintf("##### %s\n%s (thread)\n\n%s", ts.Format(constants.TimeLayout), channel, msg)

		got := FormatListAttachmentHeader(ts, channel, msg, true)
		if got != expected {
			t.Fatalf("FormatListAttachmentHeader() = %q, want %q", got, expected)
		}
	})
}

func TestFormatListItemTitle(t *testing.T) {
	if got := FormatListItemTitle(1, "aaaa1111-0000-4000-8000-000000000001"); got != "#1 · aaaa1111" {
		t.Fatalf("FormatListItemTitle() = %q", got)
	}
	if got := FormatListItemTitle(12, "id3"); got != "#12 · id3" {
		t.Fatalf("FormatListItemTitle() = %q", got)
	}
}

func TestFormatDeleteSuccess(t *testing.T) {
	ts := time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
	got := FormatDeleteSuccess(ts, "in channel: ~town-square")
	want := "✅ Message scheduled for **Nov 17, 2026 9:00 AM** in channel: ~town-square has been deleted."
	if got != want {
		t.Fatalf("FormatDeleteSuccess() = %q, want %q", got, want)
	}
}
