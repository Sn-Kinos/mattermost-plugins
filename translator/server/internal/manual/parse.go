package manual

import (
	"errors"
	"regexp"
	"strings"
)

var (
	ErrInvalidCommand = errors.New("invalid /trans command")
	translateCommand  = regexp.MustCompile(`(?s)^/trans\s+(.+?)\s*$`)
)

// Parse returns the message to translate with its inner whitespace intact.
func Parse(command string) (string, error) {
	matches := translateCommand.FindStringSubmatch(command)
	if matches == nil || strings.TrimSpace(matches[1]) == "" {
		return "", ErrInvalidCommand
	}
	return matches[1], nil
}
