package automatic

import (
	"net/url"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

func FormatAutomaticPost(translation, sourcePostID, siteURL string) string {
	permalink := strings.TrimRight(strings.TrimSpace(siteURL), "/") + "/_redirect/pl/" + url.PathEscape(sourcePostID)
	return "🇰🇷 " + renderMentionsAsInlineCode(strings.TrimSpace(translation)) + "\n\n" + permalink
}

func AutomaticPostProps() model.StringInterface {
	return model.StringInterface{model.PostPropsSilentNotification: true}
}

func renderMentionsAsInlineCode(text string) string {
	var rendered strings.Builder
	rendered.Grow(len(text))
	for offset := 0; offset < len(text); {
		segment, size := protectedSegmentAt(text, offset)
		if size == 0 {
			rendered.WriteByte(text[offset])
			offset++
			continue
		}
		if segment.kind == "mention" {
			rendered.WriteByte('`')
			rendered.WriteString(segment.value)
			rendered.WriteByte('`')
		} else {
			rendered.WriteString(segment.value)
		}
		offset += size
	}
	return rendered.String()
}
