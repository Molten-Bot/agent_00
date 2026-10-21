package web

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	for _, key := range []string{
		"WHISPER_SPEECH_DISABLED",
		"WHISPER_SPEECH_HOST",
		"WHISPER_SPEECH_PORT",
		"WHISPER_SPEECH_LANGUAGE",
		"WHISPER_SPEECH_TIMEOUT_SECONDS",
		"WHISPER_HOST",
		"WHISPER_PORT",
		"WHISPER_LANG",
	} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
