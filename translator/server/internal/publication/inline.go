package publication

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

const InlineBlockHashProp = "translator_inline_block_sha256"

// InlineLayoutProp records the translation-first layout: the plain head is
// the translation and the trailing quote block is the original source, so
// readers see the translation before the original.
const InlineLayoutProp = "translator_inline_layout"

// InlineLayoutTranslationFirst marks translation-first messages.
const InlineLayoutTranslationFirst = "translation_first"

// ApplyInlineTranslation rebuilds post as translation-first: the trimmed
// translation becomes the plain head and the stripped source becomes the
// trailing quote block. StripInlineTranslation recovers the source from
// either layout, so retranslation converges onto the new layout. Uploaded
// files stay on the post: the message layout never carries FileIds, and
// clearing them here would drop attachments from the visible message.
func ApplyInlineTranslation(post *model.Post, translation string) (*model.Post, bool) {
	trimmed := strings.TrimSpace(translation)
	if post == nil || trimmed == "" {
		return nil, false
	}
	source, _ := StripInlineTranslation(post)
	block := quoteLines(source)
	updated := post.Clone()
	updated.FileIds = append(model.StringArray(nil), post.FileIds...)
	updated.Message = trimmed + "\n\n" + block
	updated.AddProp(InlineBlockHashProp, inlineBlockHash(block))
	updated.AddProp(InlineLayoutProp, InlineLayoutTranslationFirst)
	return updated, true
}

func StripInlineTranslation(post *model.Post) (string, bool) {
	if post == nil {
		return "", false
	}
	expected, _ := post.GetProp(InlineBlockHashProp).(string)
	if expected == "" {
		return post.Message, false
	}
	for end := len(post.Message); end > 0; {
		separator := strings.LastIndex(post.Message[:end], "\n\n>")
		if separator < 0 {
			break
		}
		block := post.Message[separator+2:]
		if quotedBlock(block) && inlineBlockHash(block) == expected {
			if TranslationFirst(post) {
				source, ok := unquoteLines(block)
				if !ok {
					return post.Message, false
				}
				return source, true
			}
			// Legacy source-first messages carry no layout prop: the prefix is
			// the source and the trailing quote is the translation.
			return post.Message[:separator], true
		}
		end = separator
	}
	return post.Message, false
}

func HasInlineTranslation(post *model.Post, translation string) bool {
	trimmed := strings.TrimSpace(translation)
	if post == nil || trimmed == "" {
		return false
	}
	expected, _ := post.GetProp(InlineBlockHashProp).(string)
	if expected == "" || !TranslationFirst(post) {
		return false
	}
	head := trimmed + "\n\n"
	if !strings.HasPrefix(post.Message, head) {
		return false
	}
	block := post.Message[len(head):]
	return quotedBlock(block) && inlineBlockHash(block) == expected
}

// TranslationFirst reports whether post carries a translation-first owned
// block. Legacy messages without the layout prop read as source-first.
func TranslationFirst(post *model.Post) bool {
	if post == nil {
		return false
	}
	layout, _ := post.GetProp(InlineLayoutProp).(string)
	return layout == InlineLayoutTranslationFirst
}

// DamagedInlineTranslation reports a translation-first message whose owned
// source quote is gone. The original is unrecoverable, so callers must leave
// the message alone instead of translating the translation.
func DamagedInlineTranslation(post *model.Post) bool {
	if !TranslationFirst(post) {
		return false
	}
	_, owned := StripInlineTranslation(post)
	return !owned
}

// quoteLines preserves source whitespace exactly except trailing newlines, so
// indented Markdown survives the translation-first rebuild. Blank lines
// become a bare ">" so the block stays detectable.
func quoteLines(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + line
		}
	}
	return strings.Join(lines, "\n")
}

func unquoteLines(block string) (string, bool) {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		switch {
		case line == ">":
			lines[i] = ""
		case strings.HasPrefix(line, "> "):
			lines[i] = strings.TrimPrefix(line, "> ")
		default:
			return "", false
		}
	}
	return strings.Join(lines, "\n"), true
}

func quotedBlock(block string) bool {
	if block == "" {
		return false
	}
	for _, line := range strings.Split(block, "\n") {
		if line != ">" && !strings.HasPrefix(line, "> ") {
			return false
		}
	}
	return true
}

func inlineBlockHash(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}
