package publication

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestApplyInlineTranslationFormatsAndReplacesOnlyOwnedTrailingQuote(t *testing.T) {
	example, ok := ApplyInlineTranslation(&model.Post{Message: "안녕하세요.\n내일 봐요."}, "Hello.\nSee you tomorrow")
	require.True(t, ok)
	require.Equal(t, "Hello.\nSee you tomorrow\n\n> 안녕하세요.\n> 내일 봐요.", example.Message)

	post := &model.Post{Message: "안녕하세요.\n내일 봐요.\n\n> 사용자가 쓴 인용"}
	updated, ok := ApplyInlineTranslation(post, "Hello.\nSee you tomorrow")
	require.True(t, ok)
	require.Equal(t, "Hello.\nSee you tomorrow\n\n> 안녕하세요.\n> 내일 봐요.\n>\n> > 사용자가 쓴 인용", updated.Message)
	require.Equal(t, post.Message, "안녕하세요.\n내일 봐요.\n\n> 사용자가 쓴 인용")

	replaced, ok := ApplyInlineTranslation(updated, "Hi.\nSee you tomorrow.")
	require.True(t, ok)
	require.Equal(t, "Hi.\nSee you tomorrow.\n\n> 안녕하세요.\n> 내일 봐요.\n>\n> > 사용자가 쓴 인용", replaced.Message)
	require.True(t, HasInlineTranslation(replaced, "Hi.\nSee you tomorrow."))

	withFiles := &model.Post{Message: "안녕하세요.", FileIds: model.StringArray{"file1"}}
	preserved, ok := ApplyInlineTranslation(withFiles, "Hello.")
	require.True(t, ok)
	require.Equal(t, model.StringArray{"file1"}, preserved.FileIds)
	require.Equal(t, "Hello.\n\n> 안녕하세요.", preserved.Message)
}
