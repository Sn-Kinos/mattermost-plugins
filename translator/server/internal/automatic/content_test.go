package automatic

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestBuildTranslationInputUsesOnlyMessageAndTextualAttachmentFields(t *testing.T) {
	post := &model.Post{
		Message:   "Ship this release.",
		Filenames: model.StringArray{"private-plan.pdf"},
		FileIds:   model.StringArray{"file-id-secret"},
		Props: model.StringInterface{
			model.PostPropsAttachments: []map[string]any{
				{
					"id":          443,
					"fallback":    "excluded fallback",
					"pretext":     "Release pretext",
					"author_name": "excluded author",
					"author_link": "https://excluded.example/author",
					"title":       "Release title",
					"title_link":  "https://excluded.example/title",
					"text":        "Release details",
					"fields": []map[string]any{
						{"title": "Owner", "value": "@alice", "short": true},
						{"title": "Count", "value": 42, "short": true},
					},
					"image_url":   "https://excluded.example/image.png",
					"thumb_url":   "https://excluded.example/thumb.png",
					"footer":      "excluded footer",
					"footer_icon": "https://excluded.example/footer.png",
				},
			},
		},
	}

	input, ok, err := BuildTranslationInput(post, nil)

	require.NoError(t, err)
	require.True(t, ok)
	restored, err := RestoreProtectedContent(input.Message, input.Message)
	require.NoError(t, err)
	require.Equal(t, "Ship this release.\nRelease pretext\nRelease title\nRelease details\nOwner\n@alice\nCount\n42", restored)
	require.Empty(t, input.ParentContext)
	for _, excluded := range []string{
		"private-plan.pdf",
		"file-id-secret",
		"excluded fallback",
		"excluded author",
		"https://excluded.example/author",
		"https://excluded.example/title",
		"https://excluded.example/image.png",
		"https://excluded.example/thumb.png",
		"excluded footer",
		"https://excluded.example/footer.png",
	} {
		require.NotContains(t, restored, excluded)
	}
}

func TestBuildTranslationInputPreservesMarkdownWhitespace(t *testing.T) {
	post := &model.Post{
		Message: "  indented source\nline with trailing spaces  ",
		Props: model.StringInterface{
			model.PostPropsAttachments: []map[string]any{{
				"text": "  attachment indentation  ",
			}},
		},
	}

	input, ok, err := BuildTranslationInput(post, nil)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "  indented source\nline with trailing spaces  \n  attachment indentation  ", input.Message)
}

func TestBuildTranslationInputFileOnlyPostHasNoTranslatableContent(t *testing.T) {
	post := &model.Post{
		Filenames: model.StringArray{"diagram.png"},
		FileIds:   model.StringArray{"file-id"},
		Props: model.StringInterface{
			model.PostPropsAttachments: []map[string]any{{
				"image_url": "https://files.example/diagram.png",
				"thumb_url": "https://files.example/thumb.png",
			}},
		},
	}

	input, ok, err := BuildTranslationInput(post, nil)

	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, input.Message)
	require.Empty(t, input.ParentContext)
}

func TestBuildTranslationInputReplyTranslatesReplyAndUsesOnlyRootAsContext(t *testing.T) {
	root := &model.Post{
		Id:      "root-post-id",
		Message: "Root message",
		Props: model.StringInterface{
			model.PostPropsAttachments: []map[string]any{{"title": "Root attachment"}},
		},
	}
	reply := &model.Post{
		Id:      "reply-post-id",
		RootId:  root.Id,
		Message: "Reply message",
		Props: model.StringInterface{
			model.PostPropsAttachments: []map[string]any{{"text": "Reply attachment"}},
		},
	}

	input, ok, err := BuildTranslationInput(reply, root)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Reply message\nReply attachment", input.Message)
	require.Equal(t, "Root message\nRoot attachment", input.ParentContext)
}

func TestBuildTranslationInputIgnoresThreadRootForRootPost(t *testing.T) {
	post := &model.Post{Id: "ordinary-post", Message: "Translate me"}
	unrelated := &model.Post{Id: "unrelated", Message: "Do not include me"}

	input, ok, err := BuildTranslationInput(post, unrelated)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Translate me", input.Message)
	require.Empty(t, input.ParentContext)
}

func TestBuildTranslationInputReplyRequiresMatchingThreadRoot(t *testing.T) {
	reply := &model.Post{RootId: "expected-root", Message: "Reply"}

	for _, root := range []*model.Post{nil, {Id: "different-root", Message: "Wrong context"}} {
		input, ok, err := BuildTranslationInput(reply, root)

		require.Error(t, err)
		require.False(t, ok)
		require.Empty(t, input)
	}
}

func TestBuildTranslationInputRejectsMalformedAttachmentProperties(t *testing.T) {
	post := &model.Post{
		Message: "Valid message",
		Props: model.StringInterface{
			model.PostPropsAttachments: func() {},
		},
	}

	_, ok, err := BuildTranslationInput(post, nil)

	require.Error(t, err)
	require.False(t, ok)
}

func TestProtectAndRestoreContentPreservesCodeURLsAndMentions(t *testing.T) {
	source := "Ping @sam at https://example.com/a?q=1. Run `go test ./...`.\n```go\nfmt.Println(\"@inside https://inside.example\")\n```"
	protected := ProtectContent(source)

	for _, original := range []string{
		"@sam",
		"https://example.com/a?q=1",
		"`go test ./...`",
		"```go\nfmt.Println(\"@inside https://inside.example\")\n```",
	} {
		require.NotContains(t, protected, original)
	}

	translated := strings.ReplaceAll(protected, "Ping ", "호출 ")
	translated = strings.ReplaceAll(translated, " at ", " 위치 ")
	restored, err := RestoreProtectedContent(translated, protected)

	require.NoError(t, err)
	require.Contains(t, restored, "호출 @sam 위치 https://example.com/a?q=1.")
	require.Contains(t, restored, "`go test ./...`")
	require.Contains(t, restored, "```go\nfmt.Println(\"@inside https://inside.example\")\n```")
}

func TestProtectContentPreservesCompleteFencedCodeBlocks(t *testing.T) {
	tests := []struct {
		name   string
		source string
		marker string
	}{
		{
			name: "four backticks",
			source: "before\n````go\nfmt.Println(\"opening\")\n```\n" +
				"four-backtick body must stay @inside\n````\nafter",
			marker: "four-backtick body must stay",
		},
		{
			name: "four tildes",
			source: "before\n~~~~text\nopening\n~~~\n" +
				"four-tilde body must stay https://inside.example\n~~~~\nafter",
			marker: "four-tilde body must stay",
		},
		{
			name: "embedded delimiter on non-closing line",
			source: "before\n```text\ninline ``` is still fenced code\n" +
				"embedded-delimiter body must stay @inside\n```\nafter",
			marker: "embedded-delimiter body must stay",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protected := ProtectContent(tt.source)
			translated := strings.Replace(protected, tt.marker, "MODEL_CHANGED_THIS", 1)

			restored, err := RestoreProtectedContent(translated, protected)

			require.NoError(t, err)
			require.Equal(t, tt.source, restored)
		})
	}
}

func TestProtectRestoreRejectsMissingDuplicatedAndReorderedTokens(t *testing.T) {
	protected := ProtectContent("@alpha https://example.com `code`")
	tokenPattern := regexp.MustCompile(`⟦MMT:[^⟧]+⟧`)
	tokens := tokenPattern.FindAllString(protected, -1)
	require.Len(t, tokens, 3)

	tests := []struct {
		name       string
		translated string
	}{
		{name: "missing", translated: strings.Replace(protected, tokens[1], "", 1)},
		{name: "duplicated", translated: protected + " " + tokens[1]},
		{name: "reordered as another token", translated: strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(protected, tokens[0], "SWAP"), tokens[1], tokens[0]), "SWAP", tokens[1])},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RestoreProtectedContent(tt.translated, protected)
			require.Error(t, err)
		})
	}
}

func TestProtectContentNeutralizesLiteralPlaceholderLookalikes(t *testing.T) {
	source := "literal ⟦MMT:000000:mention:QGFsaWNl⟧ text"
	protected := ProtectContent(source)

	restored, err := RestoreProtectedContent(protected, protected)

	require.NoError(t, err)
	require.Equal(t, source, restored)
}
