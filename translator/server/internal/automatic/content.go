package automatic

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
)

var protectedTokenPattern = regexp.MustCompile(`⟦MMT:([0-9]{6}):(code|url|mention|literal):([A-Za-z0-9_-]+)⟧`)

type protectedSegment struct {
	kind  string
	value string
}

func BuildTranslationInput(post, threadRoot *model.Post) (gemini.KoreanInput, bool, error) {
	if post == nil {
		return gemini.KoreanInput{}, false, errors.New("source post is unavailable")
	}
	message, err := extractPostText(post)
	if err != nil {
		return gemini.KoreanInput{}, false, err
	}
	if message == "" {
		return gemini.KoreanInput{}, false, nil
	}

	input := gemini.KoreanInput{Message: ProtectContent(message)}
	if post.RootId != "" {
		if threadRoot == nil || threadRoot.Id != post.RootId {
			return gemini.KoreanInput{}, false, errors.New("thread root is unavailable")
		}
		parentContext, contextErr := extractPostText(threadRoot)
		if contextErr != nil {
			return gemini.KoreanInput{}, false, contextErr
		}
		input.ParentContext = ProtectContent(parentContext)
	}
	return input, true, nil
}

func extractPostText(post *model.Post) (string, error) {
	parts := make([]string, 0, 8)
	appendTextPart(&parts, post.Message)

	attachmentsValue := post.GetProp(model.PostPropsAttachments)
	if attachmentsValue == nil {
		return strings.Join(parts, "\n"), nil
	}
	encoded, err := json.Marshal(attachmentsValue)
	if err != nil {
		return "", errors.New("attachment properties could not be normalized")
	}
	var attachments []model.MessageAttachment
	if err := json.Unmarshal(encoded, &attachments); err != nil {
		return "", errors.New("attachment properties could not be normalized")
	}
	for i := range attachments {
		attachment := &attachments[i]
		appendTextPart(&parts, attachment.Pretext)
		appendTextPart(&parts, attachment.Title)
		appendTextPart(&parts, attachment.Text)
		for _, field := range attachment.Fields {
			if field == nil {
				continue
			}
			appendTextPart(&parts, field.Title)
			switch value := field.Value.(type) {
			case string:
				appendTextPart(&parts, value)
			case float64:
				appendTextPart(&parts, strconv.FormatFloat(value, 'f', -1, 64))
			}
		}
	}
	return strings.Join(parts, "\n"), nil
}

func appendTextPart(parts *[]string, value string) {
	if strings.TrimSpace(value) != "" {
		*parts = append(*parts, value)
	}
}

func ProtectContent(text string) string {
	var protected strings.Builder
	protected.Grow(len(text))
	tokenIndex := 0
	for offset := 0; offset < len(text); {
		segment, size := protectedSegmentAt(text, offset)
		if size == 0 {
			protected.WriteByte(text[offset])
			offset++
			continue
		}
		protected.WriteString(encodeProtectedToken(tokenIndex, segment))
		tokenIndex++
		offset += size
	}
	return protected.String()
}

func RestoreProtectedContent(translated, protectedSource string) (string, error) {
	sourceTokens := protectedTokenPattern.FindAllString(protectedSource, -1)
	translatedTokens := protectedTokenPattern.FindAllString(translated, -1)
	if len(sourceTokens) != len(translatedTokens) {
		return "", errors.New("protected token count changed")
	}

	originals := make(map[string]string, len(sourceTokens))
	for index, token := range sourceTokens {
		parsedIndex, original, err := decodeProtectedToken(token)
		if err != nil || parsedIndex != index {
			return "", errors.New("protected source token sequence is invalid")
		}
		if token != translatedTokens[index] {
			return "", errors.New("protected token sequence changed")
		}
		originals[token] = original
	}

	restored := protectedTokenPattern.ReplaceAllStringFunc(translated, func(token string) string {
		return originals[token]
	})
	return restored, nil
}

func protectedSegmentAt(text string, offset int) (protectedSegment, int) {
	remaining := text[offset:]
	if location := protectedTokenPattern.FindStringIndex(remaining); location != nil && location[0] == 0 {
		return protectedSegment{kind: "literal", value: remaining[:location[1]]}, location[1]
	}
	if size := fencedCodeSize(remaining); size > 0 {
		return protectedSegment{kind: "code", value: remaining[:size]}, size
	}
	if remaining[0] == '`' {
		delimiterSize := 1
		for delimiterSize < len(remaining) && remaining[delimiterSize] == '`' {
			delimiterSize++
		}
		if delimiterSize < 3 {
			delimiter := remaining[:delimiterSize]
			if closing := strings.Index(remaining[delimiterSize:], delimiter); closing >= 0 {
				size := delimiterSize + closing + len(delimiter)
				return protectedSegment{kind: "code", value: remaining[:size]}, size
			}
		}
	}
	if strings.HasPrefix(remaining, "https://") || strings.HasPrefix(remaining, "http://") {
		if size := urlTokenSize(remaining); size > 0 {
			return protectedSegment{kind: "url", value: remaining[:size]}, size
		}
	}
	if remaining[0] == '@' && mentionStartsAt(text, offset) {
		if size := mentionTokenSize(remaining); size > 1 {
			return protectedSegment{kind: "mention", value: remaining[:size]}, size
		}
	}
	return protectedSegment{}, 0
}

func fencedCodeSize(remaining string) int {
	if len(remaining) < 3 || remaining[0] != '`' && remaining[0] != '~' {
		return 0
	}
	marker := remaining[0]
	openingLength := 1
	for openingLength < len(remaining) && remaining[openingLength] == marker {
		openingLength++
	}
	if openingLength < 3 {
		return 0
	}

	openingLineEnd := strings.IndexByte(remaining[openingLength:], '\n')
	if openingLineEnd < 0 {
		return 0
	}
	lineStart := openingLength + openingLineEnd + 1
	for lineStart <= len(remaining) {
		lineLength := strings.IndexByte(remaining[lineStart:], '\n')
		lineEnd := len(remaining)
		if lineLength >= 0 {
			lineEnd = lineStart + lineLength
		}
		if isClosingFenceLine(remaining[lineStart:lineEnd], marker, openingLength) {
			return lineEnd
		}
		if lineLength < 0 {
			break
		}
		lineStart = lineEnd + 1
	}
	return 0
}

func isClosingFenceLine(line string, marker byte, minimumLength int) bool {
	offset := 0
	for offset < len(line) && offset < 3 && line[offset] == ' ' {
		offset++
	}
	markerStart := offset
	for offset < len(line) && line[offset] == marker {
		offset++
	}
	if offset-markerStart < minimumLength {
		return false
	}
	for offset < len(line) {
		if line[offset] != ' ' && line[offset] != '\t' && line[offset] != '\r' {
			return false
		}
		offset++
	}
	return true
}

func encodeProtectedToken(index int, segment protectedSegment) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(segment.value))
	return fmt.Sprintf("⟦MMT:%06d:%s:%s⟧", index, segment.kind, payload)
}

func decodeProtectedToken(token string) (int, string, error) {
	parts := protectedTokenPattern.FindStringSubmatch(token)
	if len(parts) != 4 || parts[0] != token {
		return 0, "", errors.New("invalid protected token")
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", errors.New("invalid protected token index")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !utf8.Valid(payload) {
		return 0, "", errors.New("invalid protected token payload")
	}
	return index, string(payload), nil
}

func urlTokenSize(remaining string) int {
	end := 0
	for end < len(remaining) {
		r, width := utf8.DecodeRuneInString(remaining[end:])
		if unicode.IsSpace(r) || strings.ContainsRune("<>\"'", r) {
			break
		}
		end += width
	}
	for end > 0 && strings.ContainsRune(".,;:!?", rune(remaining[end-1])) {
		end--
	}
	return end
}

func mentionStartsAt(text string, offset int) bool {
	if offset == 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(text[:offset])
	return !unicode.IsLetter(previous) && !unicode.IsNumber(previous) && !strings.ContainsRune("._%+-@/\\", previous)
}

func mentionTokenSize(remaining string) int {
	if len(remaining) < 2 || !isMentionStart(remaining[1]) {
		return 0
	}
	end := 2
	for end < len(remaining) && isMentionContinue(remaining[end]) {
		end++
	}
	for end > 1 && remaining[end-1] == '.' {
		end--
	}
	return end
}

func isMentionStart(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func isMentionContinue(value byte) bool {
	return isMentionStart(value) || value == '.' || value == '_' || value == '-'
}
