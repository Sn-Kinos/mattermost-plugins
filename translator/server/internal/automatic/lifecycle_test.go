package automatic

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

func TestProcessJobUsesThreadContextAndUpdatesSourceInline(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	root := automaticSourcePost("root-1", 90, "Root context")
	root.CreateAt = 90
	source := automaticSourcePost("source-1", 101, "Hello @alex")
	source.RootId = root.Id
	environment.api.seedPost(root)
	environment.api.seedPost(source)

	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	environment.configuredGlossary["배포"] = "mutated outside service"
	require.NoError(t, environment.service.ProcessJob(t.Context(), mustJob(t, environment.repository, source.Id, source.UpdateAt)))

	updated := environment.api.post(source.Id)
	require.Equal(t, automaticTestSourceID, updated.UserId)
	require.Equal(t, source.RootId, updated.RootId)
	require.Equal(t, "안녕하세요 `@alex`\n\n> Hello @alex", updated.Message)
	require.Empty(t, environment.api.visibleBotPosts())
	calls := environment.translator.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, ProtectContent(root.Message), calls[0].input.ParentContext)
	require.Equal(t, "deployment", calls[0].glossary["배포"])
	mapping := mustMapping(t, environment.repository, source.Id)
	require.Equal(t, source.Id, mapping.TranslationPostID)
	require.Equal(t, updated.UpdateAt, mapping.SourceUpdateAt)
}

func TestProcessJobUpdatesSourceInline(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-inline", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))

	require.NoError(t, environment.service.ProcessJob(
		t.Context(),
		mustJob(t, environment.repository, source.Id, source.UpdateAt),
	))

	updated := environment.api.post(source.Id)
	require.Equal(t, automaticTestSourceID, updated.UserId)
	require.Equal(t, "안녕하세요\n\n> Hello", updated.Message)
	require.Empty(t, environment.api.visibleBotPosts())
	mapping := mustMapping(t, environment.repository, source.Id)
	require.Equal(t, source.Id, mapping.TranslationPostID)
	require.Equal(t, updated.UpdateAt, mapping.SourceUpdateAt)
}

func TestProcessJobReplacesInlineTranslation(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-inline", 101, "Hello")
	withOldTranslation, ok := publication.ApplyInlineTranslation(source, "이전 번역")
	require.True(t, ok)
	environment.api.seedPost(withOldTranslation)
	require.NoError(t, environment.service.HandlePosted(withOldTranslation.Clone()))

	require.NoError(t, environment.service.ProcessJob(
		t.Context(),
		mustJob(t, environment.repository, source.Id, source.UpdateAt),
	))

	require.Equal(t, "안녕하세요\n\n> Hello", environment.api.post(source.Id).Message)
	require.Empty(t, environment.api.visibleBotPosts())
}

func TestProcessJobDoesNotOverwriteChangedSource(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-inline", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	environment.translator.translate = func(input gemini.KoreanInput) string {
		changed := source.Clone()
		changed.UpdateAt = 202
		changed.Message = "Changed while translating"
		environment.api.seedPost(changed)
		return "안녕하세요"
	}

	require.NoError(t, environment.service.ProcessJob(
		t.Context(),
		mustJob(t, environment.repository, source.Id, source.UpdateAt),
	))

	require.Equal(t, "Changed while translating", environment.api.post(source.Id).Message)
	require.Empty(t, environment.api.visibleBotPosts())
}

func TestProcessJobRecoversInlineUpdateAcknowledgement(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-inline", 101, "Hello")
	environment.api.seedPost(source)
	environment.api.updateStoreThenError = true
	require.NoError(t, environment.service.HandlePosted(source.Clone()))

	require.NoError(t, environment.service.ProcessJob(
		t.Context(),
		mustJob(t, environment.repository, source.Id, source.UpdateAt),
	))

	require.Equal(t, "안녕하세요\n\n> Hello", environment.api.post(source.Id).Message)
	require.Empty(t, environment.api.visibleBotPosts())
}

func TestProcessJobCancelsStaleVersionBeforeTranslation(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello stale")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	staleJob := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	updated := source.Clone()
	updated.UpdateAt = 202
	updated.Message = "Hello current"
	environment.api.seedPost(updated)

	require.NoError(t, environment.service.ProcessJob(t.Context(), staleJob))

	require.Empty(t, environment.translator.snapshot())
	require.Equal(t, updated.Message, environment.api.post(source.Id).Message)
}

func TestProcessJobDoesNotUpdateDeletedSource(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	environment.translator.translate = func(input gemini.KoreanInput) string {
		deleted := source.Clone()
		deleted.DeleteAt = 303
		environment.api.seedPost(deleted)
		return input.Message
	}

	require.NoError(t, environment.service.ProcessJob(t.Context(), mustJob(t, environment.repository, source.Id, source.UpdateAt)))

	require.Len(t, environment.translator.snapshot(), 1)
	require.NotZero(t, environment.api.post(source.Id).DeleteAt)
	mapping := mustMapping(t, environment.repository, source.Id)
	require.True(t, mapping.InternallyDeleting)
	require.Empty(t, mapping.TranslationPostID)
}

func TestProcessJobDoesNotUpdateSuppressedVersion(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	environment.translator.translate = func(input gemini.KoreanInput) string {
		mapping := mustMapping(t, environment.repository, source.Id)
		mapping.SuppressedUpdateAt = source.UpdateAt
		mapping.TranslationPostID = ""
		require.NoError(t, environment.repository.PutMapping(mapping))
		return input.Message
	}

	require.NoError(t, environment.service.ProcessJob(t.Context(), mustJob(t, environment.repository, source.Id, source.UpdateAt)))

	require.Len(t, environment.translator.snapshot(), 1)
	require.Equal(t, source.Message, environment.api.post(source.Id).Message)
}

func TestProcessJobMigratesLegacyBotPostToInline(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	source := automaticSourcePost("source-1", 101, "Hello")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))
	job := mustJob(t, environment.repository, source.Id, source.UpdateAt)
	mapping := mustMapping(t, environment.repository, source.Id)
	mapping.TranslationPostID = "translation-old"
	require.NoError(t, environment.repository.PutMapping(mapping))
	environment.api.seedPost(automaticMappedPost(mapping.TranslationPostID, mapping, time.Now().UnixMilli()))

	require.NoError(t, environment.service.ProcessJob(t.Context(), job))

	require.Equal(t, "안녕하세요\n\n> Hello", environment.api.post(source.Id).Message)
	require.NotZero(t, environment.api.post("translation-old").DeleteAt)
	require.Equal(t, source.Id, mustMapping(t, environment.repository, source.Id).TranslationPostID)
}

func TestProcessJobRejectsChangedProtectedContent(t *testing.T) {
	environment := newAutomaticTestEnvironment(t)
	environment.translator.translate = func(input gemini.KoreanInput) string {
		return strings.ReplaceAll(input.Message, "⟦MMT:", "⟦BROKEN:")
	}
	source := automaticSourcePost("source-1", 101, "Hello @alex")
	environment.api.seedPost(source)
	require.NoError(t, environment.service.HandlePosted(source.Clone()))

	err := environment.service.ProcessJob(t.Context(), mustJob(t, environment.repository, source.Id, source.UpdateAt))

	require.Error(t, err)
	require.Equal(t, source.Message, environment.api.post(source.Id).Message)
}
