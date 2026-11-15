package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigurationDefaultTimezone(t *testing.T) {
	assert.Equal(t, "Asia/Seoul", (&configuration{}).defaultTimezone())
	assert.Equal(t, "Asia/Seoul", (&configuration{DefaultTimezone: "  "}).defaultTimezone())
	assert.Equal(t, "America/New_York", (&configuration{DefaultTimezone: " America/New_York "}).defaultTimezone())
}

func TestPluginDefaultTimezoneFollowsConfiguration(t *testing.T) {
	p := &Plugin{}
	assert.Equal(t, "Asia/Seoul", p.defaultTimezone())

	p.setConfiguration(&configuration{DefaultTimezone: "Europe/London"})
	assert.Equal(t, "Europe/London", p.defaultTimezone())
}
