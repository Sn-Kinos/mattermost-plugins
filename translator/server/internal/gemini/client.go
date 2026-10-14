package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

const (
	totalRequestBudget     = 10 * time.Second
	firstAttemptBudget     = 4 * time.Second
	secondAttemptBudget    = 3 * time.Second
	koreanMaxOutputTokens  = int32(1024)
	englishMaxOutputTokens = int32(2048)
)

type ErrorCategory string

const (
	ErrorCategoryRetryable        ErrorCategory = "retryable"
	ErrorCategoryAuthentication   ErrorCategory = "authentication"
	ErrorCategoryConfiguration    ErrorCategory = "configuration"
	ErrorCategoryStructuredOutput ErrorCategory = "structured_output"
)

// Error deliberately carries only operational metadata safe for logs and UI
// categorization. Provider bodies and wrapped errors are never retained.
type Error struct {
	Category ErrorCategory
	Status   int
	Model    string
	Attempt  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("gemini request failed: category=%s status=%d model=%s attempt=%d", e.Category, e.Status, e.Model, e.Attempt)
}

type BackendPart struct {
	Text           string
	FunctionCall   bool
	ExecutableCode bool
}

type BackendCandidate struct {
	FinishReason string
	Parts        []BackendPart
}

type BackendResponse struct {
	Candidates []BackendCandidate
}

type Backend interface {
	Generate(
		ctx context.Context,
		model string,
		systemInstruction string,
		prompt string,
		schema Schema,
		maxOutputTokens int32,
	) (BackendResponse, error)
}

type backendFactory func(ctx context.Context, apiKey string) (Backend, error)

type Client struct {
	primaryModel   string
	fallbackModel  string
	backend        Backend
	backendFactory backendFactory
}

func NewClient(ctx context.Context, apiKey, primary, fallback string) (*Client, error) {
	backend, err := newSDKBackend(ctx, apiKey)
	if err != nil {
		return nil, &Error{Category: ErrorCategoryConfiguration}
	}
	return &Client{
		primaryModel:   primary,
		fallbackModel:  fallback,
		backend:        backend,
		backendFactory: newSDKBackend,
	}, nil
}

// NewClientWithBackend assembles the translation client around an alternate
// Gemini transport. It keeps retry, fallback, prompt, schema, and response
// validation behavior identical while allowing callers to replace only the
// network boundary.
func NewClientWithBackend(primary, fallback string, backend Backend) *Client {
	return &Client{
		primaryModel:  primary,
		fallbackModel: fallback,
		backend:       backend,
	}
}

func (c *Client) TranslateToKorean(ctx context.Context, input KoreanInput, glossary map[string]string) (domain.KoreanTranslation, error) {
	prompt, err := koreanPrompt(input, glossary)
	if err != nil {
		return domain.KoreanTranslation{}, &Error{Category: ErrorCategoryConfiguration}
	}
	generated, err := c.generate(ctx, koreanSystemInstruction, prompt, koreanTranslationSchema(), koreanMaxOutputTokens)
	if err != nil {
		return domain.KoreanTranslation{}, err
	}

	text, err := validatedText(generated.response)
	if err != nil {
		return domain.KoreanTranslation{}, generated.structuredOutputError()
	}
	var output struct {
		Korean string `json:"korean"`
	}
	if err := decodeStrictJSON(text, &output); err != nil || strings.TrimSpace(output.Korean) == "" {
		return domain.KoreanTranslation{}, generated.structuredOutputError()
	}
	return domain.KoreanTranslation{Text: strings.TrimSpace(output.Korean)}, nil
}

func (c *Client) TranslateToEnglish(ctx context.Context, input EnglishInput, glossary map[string]string) (string, error) {
	prompt, err := englishPrompt(input, glossary)
	if err != nil {
		return "", &Error{Category: ErrorCategoryConfiguration}
	}
	generated, err := c.generate(ctx, englishSystemInstruction, prompt, englishTranslationSchema(), englishMaxOutputTokens)
	if err != nil {
		return "", err
	}

	text, err := validatedText(generated.response)
	if err != nil {
		return "", generated.structuredOutputError()
	}
	var output struct {
		English string `json:"english"`
	}
	if err := decodeStrictJSON(text, &output); err != nil || strings.TrimSpace(output.English) == "" {
		return "", generated.structuredOutputError()
	}
	return strings.TrimSpace(output.English), nil
}

func (c *Client) ValidateModels(ctx context.Context, apiKey, primary, fallback string) error {
	factory := c.backendFactory
	if factory == nil {
		factory = newSDKBackend
	}
	backend, err := factory(ctx, apiKey)
	if err != nil {
		return &Error{Category: ErrorCategoryConfiguration}
	}
	lookupBackend, ok := backend.(interface {
		GetModel(context.Context, string) (bool, error)
	})
	if !ok {
		return &Error{Category: ErrorCategoryConfiguration}
	}

	for _, model := range []string{primary, fallback} {
		supported, lookupErr := lookupBackend.GetModel(ctx, model)
		if lookupErr != nil {
			category, status := classifyBackendError(lookupErr)
			return &Error{Category: category, Status: status, Model: model, Attempt: 1}
		}
		if !supported {
			return &Error{Category: ErrorCategoryConfiguration, Model: model, Attempt: 1}
		}
	}
	return nil
}

type generatedResponse struct {
	response BackendResponse
	model    string
	attempt  int
}

func (r generatedResponse) structuredOutputError() error {
	return &Error{
		Category: ErrorCategoryStructuredOutput,
		Model:    r.model,
		Attempt:  r.attempt,
	}
}

func (c *Client) generate(
	ctx context.Context,
	systemInstruction string,
	prompt string,
	schema Schema,
	maxOutputTokens int32,
) (generatedResponse, error) {
	if c == nil || c.backend == nil {
		return generatedResponse{}, &Error{Category: ErrorCategoryConfiguration}
	}

	budgetContext, cancelBudget := context.WithTimeout(ctx, totalRequestBudget)
	defer cancelBudget()
	parentDeadline, _ := budgetContext.Deadline()
	attempts := []struct {
		model string
		cap   time.Duration
	}{
		{model: c.primaryModel, cap: firstAttemptBudget},
		{model: c.primaryModel, cap: secondAttemptBudget},
		{model: c.fallbackModel},
	}

	var lastError *Error
	for index, attempt := range attempts {
		attemptNumber := index + 1
		if budgetContext.Err() != nil {
			return generatedResponse{}, &Error{
				Category: ErrorCategoryRetryable,
				Model:    attempt.model,
				Attempt:  attemptNumber,
			}
		}

		var attemptContext context.Context
		var cancelAttempt context.CancelFunc
		if attempt.cap > 0 {
			attemptContext, cancelAttempt = context.WithTimeout(budgetContext, attempt.cap)
		} else {
			attemptContext, cancelAttempt = context.WithDeadline(budgetContext, parentDeadline)
		}
		response, err := c.backend.Generate(
			attemptContext,
			attempt.model,
			systemInstruction,
			prompt,
			schema,
			maxOutputTokens,
		)
		cancelAttempt()
		if err == nil {
			return generatedResponse{response: response, model: attempt.model, attempt: attemptNumber}, nil
		}

		category, status := classifyBackendError(err)
		lastError = &Error{
			Category: category,
			Status:   status,
			Model:    attempt.model,
			Attempt:  attemptNumber,
		}
		if category != ErrorCategoryRetryable {
			return generatedResponse{}, lastError
		}
	}
	return generatedResponse{}, lastError
}

func validatedText(response BackendResponse) (string, error) {
	if len(response.Candidates) != 1 {
		return "", errors.New("invalid candidate count")
	}
	candidate := response.Candidates[0]
	if candidate.FinishReason != "STOP" {
		return "", errors.New("invalid finish reason")
	}

	var text strings.Builder
	for _, part := range candidate.Parts {
		if part.FunctionCall || part.ExecutableCode {
			return "", errors.New("executable response part")
		}
		text.WriteString(part.Text)
	}
	result := text.String()
	if !utf8.ValidString(result) || strings.TrimSpace(result) == "" {
		return "", errors.New("invalid response text")
	}
	return result, nil
}

func decodeStrictJSON(text string, destination any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

type httpStatusError interface {
	HTTPStatusCode() int
}

func classifyBackendError(err error) (ErrorCategory, int) {
	var statusError httpStatusError
	if !errors.As(err, &statusError) {
		return ErrorCategoryRetryable, 0
	}
	status := statusError.HTTPStatusCode()
	switch status {
	case 401, 403:
		return ErrorCategoryAuthentication, status
	case 400, 404:
		return ErrorCategoryConfiguration, status
	case 408, 429, 500, 502, 503, 504:
		return ErrorCategoryRetryable, status
	default:
		return ErrorCategoryConfiguration, status
	}
}

type sdkBackend struct {
	client *genai.Client
}

func (b *sdkBackend) GetModel(ctx context.Context, model string) (bool, error) {
	metadata, err := b.client.Models.Get(ctx, model, nil)
	if err != nil {
		var apiError genai.APIError
		if errors.As(err, &apiError) {
			return false, backendHTTPError{status: apiError.Code}
		}
		return false, err
	}
	for _, action := range metadata.SupportedActions {
		if action == "generateContent" {
			return true, nil
		}
	}
	return false, nil
}

func newSDKBackend(ctx context.Context, apiKey string) (Backend, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &sdkBackend{client: client}, nil
}

func (b *sdkBackend) Generate(
	ctx context.Context,
	model string,
	systemInstruction string,
	prompt string,
	schema Schema,
	maxOutputTokens int32,
) (BackendResponse, error) {
	response, err := b.client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(prompt),
		newGenerateContentConfig(systemInstruction, schema, maxOutputTokens),
	)
	if err != nil {
		var apiError genai.APIError
		if errors.As(err, &apiError) {
			return BackendResponse{}, backendHTTPError{status: apiError.Code}
		}
		return BackendResponse{}, err
	}

	mapped := BackendResponse{Candidates: make([]BackendCandidate, 0, len(response.Candidates))}
	for _, candidate := range response.Candidates {
		mappedCandidate := BackendCandidate{}
		if candidate != nil {
			mappedCandidate.FinishReason = string(candidate.FinishReason)
			if candidate.Content != nil {
				mappedCandidate.Parts = make([]BackendPart, 0, len(candidate.Content.Parts))
				for _, part := range candidate.Content.Parts {
					if part == nil {
						mappedCandidate.Parts = append(mappedCandidate.Parts, BackendPart{})
						continue
					}
					mappedCandidate.Parts = append(mappedCandidate.Parts, BackendPart{
						Text:           part.Text,
						FunctionCall:   part.FunctionCall != nil || part.FunctionResponse != nil,
						ExecutableCode: part.ExecutableCode != nil || part.CodeExecutionResult != nil || part.ToolCall != nil || part.ToolResponse != nil,
					})
				}
			}
		}
		mapped.Candidates = append(mapped.Candidates, mappedCandidate)
	}
	return mapped, nil
}

func newGenerateContentConfig(systemInstruction string, schema Schema, maxOutputTokens int32) *genai.GenerateContentConfig {
	temperature := float32(0.2)
	return &genai.GenerateContentConfig{
		HTTPOptions:        &genai.HTTPOptions{RetryOptions: nil},
		SystemInstruction:  &genai.Content{Parts: []*genai.Part{{Text: systemInstruction}}},
		Temperature:        &temperature,
		CandidateCount:     1,
		MaxOutputTokens:    maxOutputTokens,
		ResponseMIMEType:   "application/json",
		ResponseJsonSchema: schema,
		Tools:              nil,
		ToolConfig:         nil,
	}
}

type backendHTTPError struct {
	status int
}

func (e backendHTTPError) Error() string       { return "provider request failed" }
func (e backendHTTPError) HTTPStatusCode() int { return e.status }
