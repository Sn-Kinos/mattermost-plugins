package automatic

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestFormatAutomaticPostRendersMentionsAsCodeAndAppendsStablePermalink(t *testing.T) {
	formatted := FormatAutomaticPost(
		"안녕하세요 @sam 및 @channel. `@already-code`와 https://example.com/@path 는 유지합니다.",
		"source-post-id",
		"https://chat.example.com/",
	)

	require.Equal(t,
		"🇰🇷 안녕하세요 `@sam` 및 `@channel`. `@already-code`와 https://example.com/@path 는 유지합니다.\n\nhttps://chat.example.com/_redirect/pl/source-post-id",
		formatted,
	)
}

func TestFormatAutomaticPostPreservesMentionsInsideFencedCode(t *testing.T) {
	formatted := FormatAutomaticPost("before @outside\n```text\n@inside\n```", "post-id", "https://chat.example.com")

	require.Equal(t,
		"🇰🇷 before `@outside`\n```text\n@inside\n```\n\nhttps://chat.example.com/_redirect/pl/post-id",
		formatted,
	)
}

func TestAutomaticPostPropsSuppressNotificationsOnlyWhenExplicitlyApplied(t *testing.T) {
	automaticPost := &model.Post{}
	automaticPost.SetProps(AutomaticPostProps())
	selectedManualPost := &model.Post{}

	require.Equal(t, true, automaticPost.GetProp(model.PostPropsSilentNotification))
	require.True(t, automaticPost.HasSilentNotification())
	require.True(t, automaticPost.IsNotificationSuppressed())
	require.Nil(t, selectedManualPost.GetProp(model.PostPropsSilentNotification))
	require.False(t, selectedManualPost.IsNotificationSuppressed())
}
