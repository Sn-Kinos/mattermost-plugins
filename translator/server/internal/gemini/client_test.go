package gemini

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

const (
	testAPIKey      = "api-key-must-not-leak"
	testSource      = "source-message-must-not-leak"
	testRawResponse = "raw-provider-response-must-not-leak"
)

type backendCall struct {
	model             string
	systemInstruction string
	prompt            string
	schema            Schema
	maxOutputTokens   int32
	deadline          time.Time
}

type backendResult struct {
	response BackendResponse
	err      error
}

type modelResult struct {
	supportsGenerateContent bool
	err                     error
}

type fakeBackend struct {
	mu           sync.Mutex
	results      []backendResult
	calls        []backendCall
	modelResults []modelResult
	modelCalls   []string
}

func (b *fakeBackend) Generate(
	ctx context.Context,
	model string,
	systemInstruction string,
	prompt string,
	schema Schema,
	maxOutputTokens int32,
) (BackendResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	deadline, _ := ctx.Deadline()
	b.calls = append(b.calls, backendCall{
		model:             model,
		systemInstruction: systemInstruction,
		prompt:            prompt,
		schema:            schema,
		maxOutputTokens:   maxOutputTokens,
		deadline:          deadline,
	})
	index := len(b.calls) - 1
	if index >= len(b.results) {
		return BackendResponse{}, errors.New("unexpected backend call")
	}
	return b.results[index].response, b.results[index].err
}

func (b *fakeBackend) recordedCalls() []backendCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backendCall(nil), b.calls...)
}

func (b *fakeBackend) GetModel(_ context.Context, model string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.modelCalls = append(b.modelCalls, model)
	index := len(b.modelCalls) - 1
	if index >= len(b.modelResults) {
		return false, errors.New("unexpected model lookup")
	}
	return b.modelResults[index].supportsGenerateContent, b.modelResults[index].err
}

func (b *fakeBackend) recordedModelCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.modelCalls...)
}

type fakeStatusError struct {
	status int
	raw    string
}

func (e fakeStatusError) Error() string       { return e.raw }
func (e fakeStatusError) HTTPStatusCode() int { return e.status }

func newTestClient(backend Backend) *Client {
	return &Client{
		primaryModel:  "primary-model",
		fallbackModel: "fallback-model",
		backend:       backend,
	}
}

func validKoreanResponse(text string) BackendResponse {
	return BackendResponse{Candidates: []BackendCandidate{{
		FinishReason: "STOP",
		Parts:        []BackendPart{{Text: `{"korean":"` + text + `"}`}},
	}}}
}

func validEnglishResponse() BackendResponse {
	return englishResponse(`{"english":"Could you review this?"}`)
}

func englishResponse(text string, extraParts ...BackendPart) BackendResponse {
	parts := append([]BackendPart{{Text: text}}, extraParts...)
	return BackendResponse{Candidates: []BackendCandidate{{FinishReason: "STOP", Parts: parts}}}
}

func TestTranslateToKoreanUsesOneJSONResponseAndNoTools(t *testing.T) {
	backend := &fakeBackend{results: []backendResult{{response: validKoreanResponse("검토해 주세요.")}}}
	client := newTestClient(backend)

	translation, err := client.TranslateToKorean(context.Background(), KoreanInput{
		Message:       "Please review this.",
		ParentContext: "We are discussing the release.",
	}, nil)

	require.NoError(t, err)
	assert.Equal(t, domain.KoreanTranslation{Text: "검토해 주세요."}, translation)
	calls := backend.recordedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, int32(1024), calls[0].maxOutputTokens)
	assert.Equal(t, "object", calls[0].schema["type"])
	assert.Equal(t, []string{"korean"}, calls[0].schema["required"])
	assert.NotContains(t, calls[0].systemInstruction, "Please review this.")
	assert.Contains(t, calls[0].prompt, "<untrusted_data>")
	assert.Contains(t, calls[0].prompt, "</untrusted_data>")

	config := newGenerateContentConfig(calls[0].systemInstruction, calls[0].schema, calls[0].maxOutputTokens)
	assert.Equal(t, int32(1), config.CandidateCount)
	assert.Equal(t, "application/json", config.ResponseMIMEType)
	assert.Nil(t, config.Tools)
	assert.Nil(t, config.ToolConfig)
	require.NotNil(t, config.Temperature)
	assert.InDelta(t, 0.2, *config.Temperature, 0.0001)
	require.NotNil(t, config.HTTPOptions)
	assert.Nil(t, config.HTTPOptions.RetryOptions)
}

func TestTranslateToKoreanAppliesGlossaryEnglishToKorean(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	backend := &fakeBackend{results: []backendResult{{response: validKoreanResponse("아틀라스 출시")}}}
	client := newTestClient(backend)
	glossary := map[string]string{"아틀라스": "Atlas", "출시": "launch"}

	_, err := client.TranslateToKorean(context.Background(), KoreanInput{Message: testSource}, glossary)

	require.NoError(t, err)
	calls := backend.recordedCalls()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].prompt, `"english_source":"Atlas"`)
	assert.Contains(t, calls[0].prompt, `"korean_required":"아틀라스"`)
	assert.Contains(t, calls[0].prompt, `"english_source":"launch"`)
	assert.Contains(t, calls[0].prompt, `"korean_required":"출시"`)
	assert.NotContains(t, calls[0].systemInstruction, "Atlas")
	assert.NotContains(t, calls[0].systemInstruction, "아틀라스")
	assert.NotContains(t, logs.String(), "Atlas")
	assert.NotContains(t, logs.String(), "아틀라스")
}

func TestTranslateToKoreanRejectsWhitespaceOnlyOutput(t *testing.T) {
	backend := &fakeBackend{results: []backendResult{{response: validKoreanResponse("   ")}}}

	_, err := newTestClient(backend).TranslateToKorean(
		context.Background(),
		KoreanInput{Message: "Please review this."},
		nil,
	)

	require.Error(t, err)
	assertErrorCategory(t, err, ErrorCategoryStructuredOutput)
}

func TestTranslateToEnglishAppliesGlossaryKoreanToEnglish(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	backend := &fakeBackend{results: []backendResult{{response: validEnglishResponse()}}}
	client := newTestClient(backend)

	english, err := client.TranslateToEnglish(context.Background(), EnglishInput{Message: testSource}, map[string]string{
		"아틀라스": "Atlas",
		"출시":   "launch",
	})

	require.NoError(t, err)
	assert.Equal(t, "Could you review this?", english)
	calls := backend.recordedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, int32(2048), calls[0].maxOutputTokens)
	assert.Contains(t, calls[0].prompt, `"korean_source":"아틀라스"`)
	assert.Contains(t, calls[0].prompt, `"english_required":"Atlas"`)
	assert.Contains(t, calls[0].prompt, `"korean_source":"출시"`)
	assert.Contains(t, calls[0].prompt, `"english_required":"launch"`)
	assert.NotContains(t, calls[0].systemInstruction, "Atlas")
	assert.NotContains(t, calls[0].systemInstruction, "아틀라스")
	assert.NotContains(t, logs.String(), "Atlas")
	assert.NotContains(t, logs.String(), "아틀라스")
	assertEnglishSchema(t, calls[0].schema)
	config := newGenerateContentConfig(calls[0].systemInstruction, calls[0].schema, calls[0].maxOutputTokens)
	assert.Equal(t, int32(1), config.CandidateCount)
	assert.Nil(t, config.Tools)
	assert.Nil(t, config.ToolConfig)
}

func TestEnglishPromptCarriesMessageAndParentContextAsUntrustedData(t *testing.T) {
	backend := &fakeBackend{results: []backendResult{{response: validEnglishResponse()}}}
	client := newTestClient(backend)
	input := EnglishInput{
		Message:       "@alex 배포를 검토해 주세요.",
		ParentContext: "Earlier thread root",
	}

	_, err := client.TranslateToEnglish(context.Background(), input, nil)

	require.NoError(t, err)
	calls := backend.recordedCalls()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].prompt, `<untrusted_data>`)
	assert.Contains(t, calls[0].prompt, `"message":"@alex 배포를 검토해 주세요."`)
	assert.NotContains(t, calls[0].prompt, `"target"`)
	assert.Contains(t, calls[0].prompt, `"parent_context":"Earlier thread root"`)
	assert.Contains(t, calls[0].systemInstruction, "Preserve Mattermost mentions")
	assert.Contains(t, calls[0].systemInstruction, "natural English")
	assert.Contains(t, calls[0].systemInstruction, "Preserve the Markdown formatting")
	assert.NotContains(t, calls[0].systemInstruction, "Earlier thread root")
}

func TestTranslateToEnglishRejectsExecutableParts(t *testing.T) {
	for _, test := range []struct {
		name string
		part BackendPart
	}{
		{name: "function call", part: BackendPart{FunctionCall: true}},
		{name: "executable code", part: BackendPart{ExecutableCode: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{results: []backendResult{{
				response: englishResponse(validEnglishResponse().Candidates[0].Parts[0].Text, test.part),
			}}}

			_, err := newTestClient(backend).TranslateToEnglish(context.Background(), EnglishInput{Message: "검토"}, nil)

			require.Error(t, err)
			assertErrorCategory(t, err, ErrorCategoryStructuredOutput)
		})
	}
}

func TestRetrySequenceIsPrimaryPrimaryFallbackWithinSharedBudget(t *testing.T) {
	backend := &fakeBackend{results: []backendResult{
		{err: fakeStatusError{status: 503, raw: testRawResponse}},
		{err: fakeStatusError{status: 429, raw: testRawResponse}},
		{response: validKoreanResponse("완료")},
	}}
	client := newTestClient(backend)
	started := time.Now()

	translation, err := client.TranslateToKorean(context.Background(), KoreanInput{Message: testSource}, nil)

	require.NoError(t, err)
	assert.Equal(t, "완료", translation.Text)
	calls := backend.recordedCalls()
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"primary-model", "primary-model", "fallback-model"}, []string{
		calls[0].model, calls[1].model, calls[2].model,
	})
	assertDeadlineCap(t, calls[0].deadline, started, 4*time.Second)
	assertDeadlineCap(t, calls[1].deadline, started, 3*time.Second)
	assertDeadlineCap(t, calls[2].deadline, started, 10*time.Second)
	assert.False(t, calls[2].deadline.After(started.Add(10*time.Second+100*time.Millisecond)))
}

func TestRetryableStatusAndTransportErrorsReachFallback(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "408", err: fakeStatusError{status: 408, raw: testRawResponse}},
		{name: "429", err: fakeStatusError{status: 429, raw: testRawResponse}},
		{name: "500", err: fakeStatusError{status: 500, raw: testRawResponse}},
		{name: "502", err: fakeStatusError{status: 502, raw: testRawResponse}},
		{name: "503", err: fakeStatusError{status: 503, raw: testRawResponse}},
		{name: "504", err: fakeStatusError{status: 504, raw: testRawResponse}},
		{name: "transport", err: errors.New(testRawResponse)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{results: []backendResult{
				{err: test.err},
				{err: test.err},
				{response: validKoreanResponse("완료")},
			}}

			translation, err := newTestClient(backend).TranslateToKorean(
				context.Background(),
				KoreanInput{Message: testSource},
				nil,
			)

			require.NoError(t, err)
			assert.Equal(t, "완료", translation.Text)
			assert.Len(t, backend.recordedCalls(), 3)
		})
	}
}

func TestAuthAndConfigurationErrorsDoNotRetryOrFallback(t *testing.T) {
	tests := []struct {
		status   int
		category ErrorCategory
	}{
		{status: 400, category: ErrorCategoryConfiguration},
		{status: 401, category: ErrorCategoryAuthentication},
		{status: 403, category: ErrorCategoryAuthentication},
		{status: 404, category: ErrorCategoryConfiguration},
	}

	for _, test := range tests {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			backend := &fakeBackend{results: []backendResult{{
				err: fakeStatusError{status: test.status, raw: testRawResponse + testAPIKey + testSource},
			}}}

			_, err := newTestClient(backend).TranslateToKorean(context.Background(), KoreanInput{Message: testSource}, nil)

			require.Error(t, err)
			assertErrorCategory(t, err, test.category)
			assert.Len(t, backend.recordedCalls(), 1)
			assertSanitized(t, err)
		})
	}
}

func TestMalformedStructuredOutputIsCategorizedWithoutRawBody(t *testing.T) {
	tests := []struct {
		name     string
		response BackendResponse
	}{
		{name: "invalid JSON", response: englishResponse(`{"english":` + testRawResponse)},
		{name: "invalid UTF-8", response: englishResponse(string([]byte{0xff, 0xfe}))},
		{name: "empty output", response: englishResponse("  ")},
		{name: "blank english", response: englishResponse(`{"english":" "}`)},
		{name: "non-STOP finish", response: BackendResponse{Candidates: []BackendCandidate{{
			FinishReason: "MAX_TOKENS",
			Parts:        []BackendPart{{Text: testRawResponse}},
		}}}},
		{name: "multiple provider candidates", response: BackendResponse{Candidates: []BackendCandidate{
			{FinishReason: "STOP", Parts: []BackendPart{{Text: testRawResponse}}},
			{FinishReason: "STOP", Parts: []BackendPart{{Text: testRawResponse}}},
		}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{results: []backendResult{{response: test.response}}}

			_, err := newTestClient(backend).TranslateToEnglish(context.Background(), EnglishInput{Message: testSource}, nil)

			require.Error(t, err)
			assertErrorCategory(t, err, ErrorCategoryStructuredOutput)
			assertSanitized(t, err)
		})
	}
}

func TestValidateModelsChecksPrimaryAndFallbackWithSuppliedAPIKey(t *testing.T) {
	backend := &fakeBackend{
		modelResults: []modelResult{
			{supportsGenerateContent: true},
			{supportsGenerateContent: true},
		},
	}
	var receivedAPIKey string
	client := &Client{backendFactory: func(_ context.Context, apiKey string) (Backend, error) {
		receivedAPIKey = apiKey
		return backend, nil
	}}

	err := client.ValidateModels(context.Background(), testAPIKey, "configured-primary", "configured-fallback")

	require.NoError(t, err)
	assert.Equal(t, testAPIKey, receivedAPIKey)
	assert.Equal(t, []string{"configured-primary", "configured-fallback"}, backend.recordedModelCalls())
	assert.Empty(t, backend.recordedCalls())
}

func assertEnglishSchema(t *testing.T, schema Schema) {
	t.Helper()
	assert.Equal(t, "object", schema["type"])
	assert.Equal(t, false, schema["additionalProperties"])
	assert.Equal(t, []string{"english"}, schema["required"])
}

func assertDeadlineCap(t *testing.T, deadline, started time.Time, cap time.Duration) {
	t.Helper()
	require.False(t, deadline.IsZero())
	remaining := deadline.Sub(started)
	assert.LessOrEqual(t, remaining, cap+100*time.Millisecond)
	assert.Greater(t, remaining, cap-500*time.Millisecond)
}

func assertErrorCategory(t *testing.T, err error, category ErrorCategory) {
	t.Helper()
	var clientErr *Error
	require.ErrorAs(t, err, &clientErr)
	assert.Equal(t, category, clientErr.Category)
}

func assertSanitized(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{testAPIKey, testSource, testRawResponse, strings.TrimSpace(testRawResponse)} {
		assert.NotContains(t, err.Error(), secret)
	}
}
