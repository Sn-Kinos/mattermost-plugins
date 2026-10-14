package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertSetting(t *testing.T, settings []*model.PluginSetting, key, settingType string, secret bool, defaultValue any) {
	t.Helper()
	for _, setting := range settings {
		if setting.Key == key {
			assert.Equal(t, settingType, setting.Type)
			assert.Equal(t, secret, setting.Secret)
			assert.Equal(t, defaultValue, setting.Default)
			return
		}
	}
	t.Fatalf("setting %q was not found", key)
}

func TestManifestTargetsMattermost113WithConsoleEditor(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	require.NoError(t, err)

	var manifest model.Manifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	assert.Equal(t, "io.github.sn-kinos.translator", manifest.Id)
	assert.Equal(t, "11.3.0", manifest.MinServerVersion)
	require.NotNil(t, manifest.Server)
	require.NotNil(t, manifest.Webapp)
	_, err = os.Stat("../" + manifest.Webapp.BundlePath)
	require.NoError(t, err, "the declared webapp bundle must exist")
	assert.Equal(t, "server/dist/plugin-linux-amd64", manifest.GetExecutableForRuntime("linux", "amd64"))
	assert.Equal(t, "server/dist/plugin-linux-arm64", manifest.GetExecutableForRuntime("linux", "arm64"))
	require.NotNil(t, manifest.SettingsSchema)
	assertSetting(t, manifest.SettingsSchema.Settings, "GeminiAPIKey", "text", true, nil)
	assertSetting(t, manifest.SettingsSchema.Settings, "PrimaryModel", "text", false, "gemini-3.7-flash")
	assertSetting(t, manifest.SettingsSchema.Settings, "FallbackModel", "text", false, "gemini-3.6-flash")
}
