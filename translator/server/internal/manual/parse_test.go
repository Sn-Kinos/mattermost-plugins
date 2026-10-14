package manual

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTranslatePreservesUnicodeMultilineMessageWhitespace(t *testing.T) {
	message, err := Parse("/trans   @jane.dev  안녕하세요,  \n\t검토 부탁드립니다.  \n다시 한번 감사합니다.   ")

	require.NoError(t, err)
	require.Equal(t, "@jane.dev  안녕하세요,  \n\t검토 부탁드립니다.  \n다시 한번 감사합니다.", message)
}

func TestParseTranslateAcceptsOneCharacterKoreanMessage(t *testing.T) {
	message, err := Parse("/trans 가")

	require.NoError(t, err)
	require.Equal(t, "가", message)
}

func TestParseTranslateRejectsMalformedCommands(t *testing.T) {
	for _, command := range []string{"/trans", "/trans \n\t  "} {
		_, err := Parse(command)
		require.Error(t, err, command)
	}
}
