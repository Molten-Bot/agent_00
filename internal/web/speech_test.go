package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHandleSpeechStatusReportsReachableSidecar(t *testing.T) {
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		_ = conn.Close()
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("WHISPER_SPEECH_HOST", host)
	t.Setenv("WHISPER_SPEECH_PORT", port)
	t.Setenv("WHISPER_SPEECH_LANGUAGE", "")
	t.Setenv("WHISPER_LANG", "")

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodGet, "/api/speech/status", nil)
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/speech/status status = %d, want 200", resp.Code)
	}

	var body struct {
		OK     bool         `json:"ok"`
		Speech speechStatus `json:"speech"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode speech status: %v", err)
	}
	if !body.OK || !body.Speech.Enabled || !body.Speech.Reachable {
		t.Fatalf("speech status = %#v, want enabled reachable", body)
	}
	if body.Speech.Rate != defaultSpeechRate {
		t.Fatalf("speech rate = %d, want %d", body.Speech.Rate, defaultSpeechRate)
	}
	if body.Speech.Language != defaultSpeechLanguage {
		t.Fatalf("speech language = %q, want %q", body.Speech.Language, defaultSpeechLanguage)
	}
}

func TestHandleSpeechTranscribeUsesWyomingServer(t *testing.T) {
	type receivedSpeechEvents struct {
		types    []string
		language string
	}
	received := make(chan receivedSpeechEvents, 1)
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		var types []string
		var language string
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				t.Errorf("read Wyoming event: %v", err)
				return
			}
			types = append(types, event.Type)
			if event.Type == "transcribe" {
				language = strings.TrimSpace(fmt.Sprint(event.Data["language"]))
			}
			if event.Type == "audio-stop" {
				break
			}
		}
		received <- receivedSpeechEvents{types: types, language: language}
		if err := writeWyomingEvent(bufio.NewWriter(conn), "transcript", map[string]any{"text": "Dictated prompt"}, nil); err != nil {
			t.Errorf("write transcript: %v", err)
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("WHISPER_SPEECH_HOST", host)
	t.Setenv("WHISPER_SPEECH_PORT", port)
	t.Setenv("WHISPER_SPEECH_LANGUAGE", "")
	t.Setenv("WHISPER_LANG", "")

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader([]byte{1, 0, 2, 0}))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("POST /api/speech/transcribe status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode transcription: %v", err)
	}
	if !body.OK || body.Text != "Dictated prompt" {
		t.Fatalf("transcription body = %#v", body)
	}

	wantTypes := []string{"transcribe", "audio-start", "audio-chunk", "audio-stop"}
	if got := <-received; !reflect.DeepEqual(got.types, wantTypes) {
		t.Fatalf("Wyoming event types = %#v, want %#v", got.types, wantTypes)
	} else if got.language != defaultSpeechLanguage {
		t.Fatalf("Wyoming transcribe language = %q, want %q", got.language, defaultSpeechLanguage)
	}
}

func TestHandleSpeechTranscribeAutoLanguageOmitsWyomingLanguage(t *testing.T) {
	received := make(chan string, 1)
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		var language string
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				t.Errorf("read Wyoming event: %v", err)
				return
			}
			if event.Type == "transcribe" {
				if value, ok := event.Data["language"]; ok {
					language = strings.TrimSpace(fmt.Sprint(value))
				}
			}
			if event.Type == "audio-stop" {
				break
			}
		}
		received <- language
		if err := writeWyomingEvent(bufio.NewWriter(conn), "transcript", map[string]any{"text": "Dictated prompt"}, nil); err != nil {
			t.Errorf("write transcript: %v", err)
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("WHISPER_SPEECH_HOST", host)
	t.Setenv("WHISPER_SPEECH_PORT", port)
	t.Setenv("WHISPER_SPEECH_LANGUAGE", "en")

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe?language=auto", bytes.NewReader([]byte{1, 0, 2, 0}))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("POST /api/speech/transcribe status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := <-received; got != "" {
		t.Fatalf("Wyoming transcribe language = %q, want automatic detection", got)
	}
}

func TestHandleSpeechTranscribeEmptyTranscriptReportsReason(t *testing.T) {
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				t.Errorf("read Wyoming event: %v", err)
				return
			}
			if event.Type == "audio-stop" {
				break
			}
		}
		if err := writeWyomingEvent(bufio.NewWriter(conn), "transcript", map[string]any{"text": ""}, nil); err != nil {
			t.Errorf("write transcript: %v", err)
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("WHISPER_SPEECH_HOST", host)
	t.Setenv("WHISPER_SPEECH_PORT", port)

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader([]byte{0, 0, 0, 0}))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("POST /api/speech/transcribe status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK     bool             `json:"ok"`
		Text   string           `json:"text"`
		Reason string           `json:"reason"`
		Audio  speechAudioStats `json:"audio"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode transcription: %v", err)
	}
	if !body.OK || body.Text != "" || body.Reason != speechReasonEmptyTranscript {
		t.Fatalf("empty transcription body = %#v", body)
	}
	if !body.Audio.NearSilent {
		t.Fatalf("audio near_silent = false, want true")
	}
}

func TestHandleSpeechTranscribeWyomingEOFReportsStructuredError(t *testing.T) {
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				t.Errorf("read Wyoming event: %v", err)
				_ = conn.Close()
				return
			}
			if event.Type == "audio-stop" {
				_ = conn.Close()
				return
			}
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("WHISPER_SPEECH_HOST", host)
	t.Setenv("WHISPER_SPEECH_PORT", port)

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader([]byte{1, 0, 2, 0}))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("POST /api/speech/transcribe status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK    bool             `json:"ok"`
		Code  string           `json:"code"`
		Error string           `json:"error"`
		Audio speechAudioStats `json:"audio"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode transcription error: %v", err)
	}
	if body.OK || body.Code != speechCodeWyomingRead || !strings.Contains(body.Error, "read speech transcript") {
		t.Fatalf("transcription error body = %#v", body)
	}
	if body.Audio.Bytes != 4 {
		t.Fatalf("audio bytes = %d, want 4", body.Audio.Bytes)
	}
}

func TestHandleSpeechTranscribeUsesStreamingTranscriptFallback(t *testing.T) {
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				t.Errorf("read Wyoming event: %v", err)
				return
			}
			if event.Type == "audio-stop" {
				break
			}
		}
		writer := bufio.NewWriter(conn)
		if err := writeWyomingEvent(writer, "transcript-start", nil, nil); err != nil {
			t.Errorf("write transcript start: %v", err)
			return
		}
		if err := writeWyomingEvent(writer, "transcript-chunk", map[string]any{"text": "Dictated "}, nil); err != nil {
			t.Errorf("write transcript chunk: %v", err)
			return
		}
		if err := writeWyomingEvent(writer, "transcript-chunk", map[string]any{"text": "prompt"}, nil); err != nil {
			t.Errorf("write transcript chunk: %v", err)
			return
		}
		if err := writeWyomingEvent(writer, "transcript-stop", nil, nil); err != nil {
			t.Errorf("write transcript stop: %v", err)
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("WHISPER_SPEECH_HOST", host)
	t.Setenv("WHISPER_SPEECH_PORT", port)

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe?language=auto", bytes.NewReader([]byte{1, 0, 2, 0}))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("POST /api/speech/transcribe status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode transcription: %v", err)
	}
	if !body.OK || body.Text != "Dictated prompt" {
		t.Fatalf("transcription body = %#v", body)
	}
}

func TestWriteWyomingEventUsesCanonicalDataLength(t *testing.T) {
	var out bytes.Buffer
	if err := writeWyomingEvent(bufio.NewWriter(&out), "transcribe", map[string]any{"language": "en"}, []byte{1, 2}); err != nil {
		t.Fatalf("write Wyoming event: %v", err)
	}

	reader := bufio.NewReader(&out)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read Wyoming header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(line, &header); err != nil {
		t.Fatalf("decode Wyoming header: %v", err)
	}
	if _, ok := header["data"]; ok {
		t.Fatalf("Wyoming header data = %#v, want canonical data_length payload", header["data"])
	}
	if got := int(header["data_length"].(float64)); got == 0 {
		t.Fatalf("Wyoming data_length = %d, want non-zero", got)
	}
	if got := int(header["payload_length"].(float64)); got != 2 {
		t.Fatalf("Wyoming payload_length = %d, want 2", got)
	}
}

func TestAnalyzeSpeechPCMDetectsSilenceAndSignal(t *testing.T) {
	cfg := speechConfig{
		Rate:    defaultSpeechRate,
		Width:   defaultSpeechSampleSize,
		Channel: defaultSpeechChannels,
	}

	silent := analyzeSpeechPCM(cfg, make([]byte, defaultSpeechRate*defaultSpeechSampleSize))
	if silent.Bytes != int64(defaultSpeechRate*defaultSpeechSampleSize) {
		t.Fatalf("silent bytes = %d, want %d", silent.Bytes, defaultSpeechRate*defaultSpeechSampleSize)
	}
	if silent.DurationSeconds != 1 {
		t.Fatalf("silent duration = %f, want 1", silent.DurationSeconds)
	}
	if !silent.NearSilent || silent.Peak != 0 || silent.RMS != 0 {
		t.Fatalf("silent stats = %#v, want near silent zero signal", silent)
	}

	signal := analyzeSpeechPCM(cfg, []byte{0xff, 0x7f, 0x00, 0x00, 0x01, 0x80, 0x00, 0x00})
	if signal.NearSilent {
		t.Fatalf("signal stats = %#v, want audible signal", signal)
	}
	if signal.Peak < 0.9 || signal.RMS < 0.5 {
		t.Fatalf("signal stats = %#v, want high peak and rms", signal)
	}
}

func TestLoadSpeechConfigAllowsAutomaticLanguage(t *testing.T) {
	t.Setenv("WHISPER_SPEECH_LANGUAGE", "auto")

	cfg := loadSpeechConfig()
	if cfg.Language != "" {
		t.Fatalf("speech language = %q, want automatic detection", cfg.Language)
	}
	if lang := resolveSpeechLanguage("auto", cfg.Language); lang != "" {
		t.Fatalf("resolved speech language = %q, want automatic detection", lang)
	}
}

func listenFakeSpeechServer(t *testing.T, handle func(net.Conn)) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake speech server: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		handle(conn)
	}()
	return listener
}

func TestSpeechTranscribeErrorUnwrap(t *testing.T) {
	t.Parallel()

	var nilErr *speechTranscribeError
	if got := nilErr.Unwrap(); got != nil {
		t.Fatalf("(*speechTranscribeError)(nil).Unwrap() = %v, want nil", got)
	}
	if got := nilErr.Error(); got != "" {
		t.Fatalf("(*speechTranscribeError)(nil).Error() = %q, want empty", got)
	}

	wrapped := errors.New("boom")
	withCause := &speechTranscribeError{Code: speechCodeWyomingError, Err: wrapped}
	if got := withCause.Unwrap(); got != wrapped {
		t.Fatalf("Unwrap() = %v, want %v", got, wrapped)
	}
	if got := withCause.Error(); got != "boom" {
		t.Fatalf("Error() = %q, want %q", got, "boom")
	}
	if !errors.Is(withCause, wrapped) {
		t.Fatal("errors.Is(withCause, wrapped) = false, want true (Unwrap must expose the cause)")
	}

	noErr := &speechTranscribeError{Code: speechCodeUnavailable}
	if got := noErr.Error(); got != "" {
		t.Fatalf("Error() with nil cause = %q, want empty", got)
	}
}

func TestEnvDuration(t *testing.T) {
	fallback := 42 * time.Second

	tests := []struct {
		name  string
		value string
		unset bool
		want  time.Duration
	}{
		{name: "unset env uses fallback", unset: true, want: fallback},
		{name: "blank env uses fallback", value: "   ", want: fallback},
		{name: "non numeric env uses fallback", value: "soon", want: fallback},
		{name: "zero seconds uses fallback", value: "0", want: fallback},
		{name: "negative seconds uses fallback", value: "-5", want: fallback},
		{name: "positive seconds overrides fallback", value: "7", want: 7 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "WEB_TEST_ENV_DURATION"
			if tt.unset {
				t.Setenv(key, "")
				os.Unsetenv(key)
			} else {
				t.Setenv(key, tt.value)
			}
			if got := envDuration(key, fallback); got != tt.want {
				t.Fatalf("envDuration(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

func TestSpeechErrorCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil error falls back to wyoming read code", err: nil, want: speechCodeWyomingRead},
		{
			name: "structured speech error preserves its code",
			err:  &speechTranscribeError{Code: speechCodeWyomingWrite, Err: errors.New("write failed")},
			want: speechCodeWyomingWrite,
		},
		{
			name: "structured speech error with blank code falls back",
			err:  &speechTranscribeError{Code: "  ", Err: errors.New("write failed")},
			want: speechCodeWyomingRead,
		},
		{
			name: "context deadline exceeded maps to timeout",
			err:  context.DeadlineExceeded,
			want: speechCodeTimeout,
		},
		{
			name: "generic error falls back to wyoming read code",
			err:  errors.New("some unexpected failure"),
			want: speechCodeWyomingRead,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := speechErrorCode(tt.err); got != tt.want {
				t.Fatalf("speechErrorCode(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestHandleSpeechTranscribeMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodGet, "/api/speech/transcribe", nil)
	resp := httptest.NewRecorder()
	srv.handleSpeechTranscribe(resp, req)

	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/speech/transcribe status = %d, want 405", resp.Code)
	}
	if got := resp.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow header = %q, want %q", got, http.MethodPost)
	}
}

func TestHandleSpeechTranscribeDisabledReturnsServiceUnavailable(t *testing.T) {
	t.Setenv("WHISPER_SPEECH_DISABLED", "true")

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader([]byte{1, 2, 3, 4}))
	resp := httptest.NewRecorder()
	srv.handleSpeechTranscribe(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled speech status = %d, want 503", resp.Code)
	}

	var body struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode disabled response: %v", err)
	}
	if body.OK || body.Code != speechCodeDisabled {
		t.Fatalf("disabled response = %#v", body)
	}
}

func TestHandleSpeechTranscribeEmptySampleReturnsBadRequest(t *testing.T) {
	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader(nil))
	resp := httptest.NewRecorder()
	srv.handleSpeechTranscribe(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("empty sample status = %d, want 400, body=%s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode empty sample response: %v", err)
	}
	if body.OK || body.Code != speechCodeSampleEmpty {
		t.Fatalf("empty sample response = %#v", body)
	}
}

func TestHandleSpeechTranscribeOverLimitSampleReturnsRequestEntityTooLarge(t *testing.T) {
	srv := NewServer("", NewBroker())
	cfg := loadSpeechConfig()

	// One byte past the configured limit triggers the MaxBytesReader error path.
	oversized := bytes.Repeat([]byte{0x01}, int(cfg.MaxBytes)+2)
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader(oversized))
	resp := httptest.NewRecorder()
	srv.handleSpeechTranscribe(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized sample status = %d, want 413, body=%s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode oversized sample response: %v", err)
	}
	if body.OK || body.Code != speechCodeSampleTooLarge {
		t.Fatalf("oversized sample response = %#v", body)
	}
}

func TestHandleSpeechTranscribeOverLimitAfterReadReturnsRequestEntityTooLarge(t *testing.T) {
	srv := NewServer("", NewBroker())
	cfg := loadSpeechConfig()

	// Exactly MaxBytes+1 is accepted by MaxBytesReader but still exceeds the
	// configured sample size once read, hitting the post-read size check.
	oversized := bytes.Repeat([]byte{0x01}, int(cfg.MaxBytes)+1)
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", bytes.NewReader(oversized))
	resp := httptest.NewRecorder()
	srv.handleSpeechTranscribe(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit-after-read sample status = %d, want 413, body=%s", resp.Code, resp.Body.String())
	}

	var body struct {
		OK    bool             `json:"ok"`
		Code  string           `json:"code"`
		Audio speechAudioStats `json:"audio"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode over-limit-after-read response: %v", err)
	}
	if body.OK || body.Code != speechCodeSampleTooLarge {
		t.Fatalf("over-limit-after-read response = %#v", body)
	}
	if body.Audio.Bytes != cfg.MaxBytes+1 {
		t.Fatalf("audio bytes = %d, want %d", body.Audio.Bytes, cfg.MaxBytes+1)
	}
}
