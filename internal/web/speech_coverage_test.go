package web

import (
	"bufio"
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// speechcovErrReader always fails to read, simulating a client whose upload
// connection drops before the body is fully received.
type speechcovErrReader struct{}

func (speechcovErrReader) Read([]byte) (int, error) {
	return 0, errors.New("speechcov: simulated body read failure")
}

type speechcovNopCloserReader struct {
	speechcovErrReader
}

func (speechcovNopCloserReader) Close() error { return nil }

func TestSpeechcovHandleSpeechTranscribeBodyReadErrorIsNotMaxBytesError(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe", nil)
	req.Body = speechcovNopCloserReader{}
	rec := httptest.NewRecorder()
	srv.handleSpeechTranscribe(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), speechCodeSampleRead) {
		t.Fatalf("body = %s, want code %q", rec.Body.String(), speechCodeSampleRead)
	}
}

func TestSpeechcovLoadSpeechConfigDisabledHostForcesDisabled(t *testing.T) {
	t.Setenv("WHISPER_SPEECH_HOST", "off")
	t.Setenv("WHISPER_SPEECH_DISABLED", "")

	cfg := loadSpeechConfig()
	if cfg.Enabled {
		t.Fatalf("loadSpeechConfig(disabled host) Enabled = true, want false")
	}
}

func TestSpeechcovTranscribeSpeechPCMReturnsErrSpeechDisabled(t *testing.T) {
	t.Parallel()

	cfg := speechConfig{Enabled: false}
	_, err := transcribeSpeechPCM(context.Background(), cfg, []byte{1, 2, 3, 4}, "")
	if !errors.Is(err, errSpeechDisabled) {
		t.Fatalf("transcribeSpeechPCM(disabled) error = %v, want errSpeechDisabled", err)
	}
}

func TestSpeechcovTranscribeSpeechPCMDialFailureReportsUnavailable(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	_ = listener.Close() // nobody is listening now: dial must be refused.

	cfg := speechConfig{
		Enabled: true,
		Host:    host,
		Port:    port,
		Rate:    defaultSpeechRate,
		Width:   defaultSpeechSampleSize,
		Channel: defaultSpeechChannels,
		Timeout: 2 * time.Second,
	}
	_, err = transcribeSpeechPCM(context.Background(), cfg, []byte{1, 2, 3, 4}, "")
	if err == nil {
		t.Fatal("transcribeSpeechPCM(unreachable server) error = nil, want dial error")
	}
	if got := speechErrorCode(err); got != speechCodeUnavailable {
		t.Fatalf("speechErrorCode() = %q, want %q", got, speechCodeUnavailable)
	}
	if !strings.Contains(err.Error(), "speech server unavailable") {
		t.Fatalf("error = %v, want unavailable message", err)
	}
}

func TestSpeechcovTranscribeSpeechPCMReturnsStreamedTextOnAbruptEOF(t *testing.T) {
	t.Parallel()

	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				return
			}
			if event.Type == "audio-stop" {
				break
			}
		}
		// Stream one chunk of transcript text, then close without a
		// "transcript-stop" event: the client must still surface the
		// text it accumulated once it observes EOF.
		writer := bufio.NewWriter(conn)
		if err := writeWyomingEvent(writer, "transcript-chunk", map[string]any{"text": "partial text"}, nil); err != nil {
			t.Errorf("write transcript chunk: %v", err)
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	cfg := speechConfig{
		Enabled: true,
		Host:    host,
		Port:    port,
		Rate:    defaultSpeechRate,
		Width:   defaultSpeechSampleSize,
		Channel: defaultSpeechChannels,
		Timeout: 5 * time.Second,
	}
	text, err := transcribeSpeechPCM(context.Background(), cfg, []byte{1, 0, 2, 0}, "")
	if err != nil {
		t.Fatalf("transcribeSpeechPCM(abrupt EOF with streamed text) error = %v", err)
	}
	if text != "partial text" {
		t.Fatalf("transcribeSpeechPCM() text = %q, want %q", text, "partial text")
	}
}

func TestSpeechcovTranscribeSpeechPCMReadTimeoutReportsTimeoutCode(t *testing.T) {
	t.Parallel()

	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		// Read everything the client sends but never respond and never
		// close: the client's read deadline must fire.
		reader := bufio.NewReader(conn)
		for {
			if _, err := readWyomingEvent(reader); err != nil {
				return
			}
		}
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	cfg := speechConfig{
		Enabled: true,
		Host:    host,
		Port:    port,
		Rate:    defaultSpeechRate,
		Width:   defaultSpeechSampleSize,
		Channel: defaultSpeechChannels,
		Timeout: 150 * time.Millisecond,
	}
	_, err = transcribeSpeechPCM(context.Background(), cfg, []byte{1, 0, 2, 0}, "")
	if err == nil {
		t.Fatal("transcribeSpeechPCM(no response) error = nil, want timeout error")
	}
	if got := speechErrorCode(err); got != speechCodeTimeout {
		t.Fatalf("speechErrorCode() = %q, want %q (err=%v)", got, speechCodeTimeout, err)
	}
}

func TestSpeechcovTranscribeSpeechPCMErrorEventMessageFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{name: "uses text field directly", data: map[string]any{"text": "boom from text"}, want: "boom from text"},
		{name: "falls back to message field when text is blank", data: map[string]any{"text": "", "message": "boom from message"}, want: "boom from message"},
		{name: "falls back to default message when both are blank", data: map[string]any{"text": "", "message": ""}, want: "speech server returned an error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener := listenFakeSpeechServer(t, func(conn net.Conn) {
				defer conn.Close()

				reader := bufio.NewReader(conn)
				for {
					event, err := readWyomingEvent(reader)
					if err != nil {
						return
					}
					if event.Type == "audio-stop" {
						break
					}
				}
				if err := writeWyomingEvent(bufio.NewWriter(conn), "error", tt.data, nil); err != nil {
					t.Errorf("write error event: %v", err)
				}
			})

			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatalf("split addr: %v", err)
			}
			cfg := speechConfig{
				Enabled: true,
				Host:    host,
				Port:    port,
				Rate:    defaultSpeechRate,
				Width:   defaultSpeechSampleSize,
				Channel: defaultSpeechChannels,
				Timeout: 5 * time.Second,
			}
			_, err = transcribeSpeechPCM(context.Background(), cfg, []byte{1, 0, 2, 0}, "")
			if err == nil {
				t.Fatal("transcribeSpeechPCM(error event) error = nil, want error")
			}
			if got := speechErrorCode(err); got != speechCodeWyomingError {
				t.Fatalf("speechErrorCode() = %q, want %q", got, speechCodeWyomingError)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// speechcovFailingWriter fails its Nth Write call (1-indexed); 0 disables
// failure. Pairing it with a tiny bufio.Writer buffer forces each logical
// writeWyomingEvent write (header, data, payload) to hit the underlying
// writer directly instead of being coalesced, so we can deterministically
// target a single failure point.
type speechcovFailingWriter struct {
	failOnCall int
	calls      int
}

func (w *speechcovFailingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.failOnCall > 0 && w.calls == w.failOnCall {
		return 0, errors.New("speechcov: simulated write failure")
	}
	return len(p), nil
}

func TestSpeechcovWriteWyomingEventMarshalDataError(t *testing.T) {
	t.Parallel()

	fw := &speechcovFailingWriter{}
	writer := bufio.NewWriterSize(fw, 1)
	err := writeWyomingEvent(writer, "transcribe", map[string]any{"bad": math.Inf(1)}, nil)
	if err == nil {
		t.Fatal("writeWyomingEvent(unmarshalable data) error = nil, want json marshal error")
	}
}

func TestSpeechcovWriteWyomingEventHeaderEncodeError(t *testing.T) {
	t.Parallel()

	fw := &speechcovFailingWriter{failOnCall: 1}
	writer := bufio.NewWriterSize(fw, 1)
	err := writeWyomingEvent(writer, "audio-stop", nil, nil)
	if err == nil {
		t.Fatal("writeWyomingEvent(header write failure) error = nil, want write error")
	}
}

func TestSpeechcovWriteWyomingEventDataWriteError(t *testing.T) {
	t.Parallel()

	fw := &speechcovFailingWriter{failOnCall: 2}
	writer := bufio.NewWriterSize(fw, 1)
	err := writeWyomingEvent(writer, "transcribe", map[string]any{"language": "en"}, nil)
	if err == nil {
		t.Fatal("writeWyomingEvent(data write failure) error = nil, want write error")
	}
}

func TestSpeechcovWriteWyomingEventPayloadWriteError(t *testing.T) {
	t.Parallel()

	fw := &speechcovFailingWriter{failOnCall: 3}
	writer := bufio.NewWriterSize(fw, 1)
	err := writeWyomingEvent(writer, "audio-chunk", map[string]any{"rate": 16000}, []byte{1, 2, 3})
	if err == nil {
		t.Fatal("writeWyomingEvent(payload write failure) error = nil, want write error")
	}
}

func TestSpeechcovReadWyomingEventMalformedHeaderJSON(t *testing.T) {
	t.Parallel()

	reader := bufio.NewReader(strings.NewReader("not valid json\n"))
	if _, err := readWyomingEvent(reader); err == nil {
		t.Fatal("readWyomingEvent(malformed header) error = nil, want decode error")
	}
}

func TestSpeechcovReadWyomingEventTruncatedDataPayload(t *testing.T) {
	t.Parallel()

	reader := bufio.NewReader(strings.NewReader(`{"type":"x","data_length":10}` + "\n" + "abc"))
	if _, err := readWyomingEvent(reader); err == nil {
		t.Fatal("readWyomingEvent(truncated data payload) error = nil, want read error")
	}
}

func TestSpeechcovReadWyomingEventMalformedDataJSON(t *testing.T) {
	t.Parallel()

	reader := bufio.NewReader(strings.NewReader(`{"type":"x","data_length":5}` + "\n" + "abcde"))
	if _, err := readWyomingEvent(reader); err == nil {
		t.Fatal("readWyomingEvent(malformed data json) error = nil, want decode error")
	}
}

func TestSpeechcovReadWyomingEventTruncatedPayloadDiscard(t *testing.T) {
	t.Parallel()

	reader := bufio.NewReader(strings.NewReader(`{"type":"x","payload_length":10}` + "\n" + "abc"))
	if _, err := readWyomingEvent(reader); err == nil {
		t.Fatal("readWyomingEvent(truncated payload) error = nil, want read error")
	}
}

func TestSpeechcovAnalyzeSpeechPCMSkipsAmplitudeAnalysisForUnsupportedFormat(t *testing.T) {
	t.Parallel()

	cfg := speechConfig{Rate: defaultSpeechRate, Width: 1, Channel: defaultSpeechChannels}
	stats := analyzeSpeechPCM(cfg, []byte{1, 2, 3, 4})
	if stats.Bytes != 4 {
		t.Fatalf("Bytes = %d, want 4", stats.Bytes)
	}
	if stats.Peak != 0 || stats.RMS != 0 || stats.Samples != 0 {
		t.Fatalf("stats = %#v, want amplitude fields left unset for non-16-bit-mono audio", stats)
	}
}

func TestSpeechcovIsDisabledSpeechHostRecognizesAllDisabledSpellings(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"", "0", "off", "OFF", "false", "False", "disabled", "DISABLED", "none", "None", "  off  "} {
		if !isDisabledSpeechHost(host) {
			t.Fatalf("isDisabledSpeechHost(%q) = false, want true", host)
		}
	}
	if isDisabledSpeechHost("faster-whisper") {
		t.Fatal("isDisabledSpeechHost(real host) = true, want false")
	}
}

func TestSpeechcovMinDuration(t *testing.T) {
	t.Parallel()

	if got := minDuration(100*time.Millisecond, 750*time.Millisecond); got != 100*time.Millisecond {
		t.Fatalf("minDuration(smaller, larger) = %s, want 100ms", got)
	}
	if got := minDuration(900*time.Millisecond, 750*time.Millisecond); got != 750*time.Millisecond {
		t.Fatalf("minDuration(larger, smaller) = %s, want 750ms", got)
	}
}

func TestSpeechcovNormalizeSpeechLanguageRejectsOverlongAndInvalidValues(t *testing.T) {
	t.Parallel()

	if got := normalizeSpeechLanguage("this-code-is-too-long-to-be-real"); got != "" {
		t.Fatalf("normalizeSpeechLanguage(overlong) = %q, want empty", got)
	}
	if got := normalizeSpeechLanguage("en1"); got != "" {
		t.Fatalf("normalizeSpeechLanguage(invalid character) = %q, want empty", got)
	}
	if got := normalizeSpeechLanguage("EN-us"); got != "en-us" {
		t.Fatalf("normalizeSpeechLanguage(mixed case) = %q, want %q", got, "en-us")
	}
}

func TestSpeechcovResolveSpeechLanguagePrefersExplicitRequestedLanguage(t *testing.T) {
	t.Parallel()

	if got := resolveSpeechLanguage("fr", "en"); got != "fr" {
		t.Fatalf("resolveSpeechLanguage(explicit valid request) = %q, want %q", got, "fr")
	}
}

func TestSpeechcovNewSpeechTranscribeErrorSynthesizesErrorFromCodeWhenNil(t *testing.T) {
	t.Parallel()

	err := newSpeechTranscribeError(speechCodeUnavailable, nil)
	var speechErr *speechTranscribeError
	if !errors.As(err, &speechErr) {
		t.Fatalf("newSpeechTranscribeError(nil err) = %v, want *speechTranscribeError", err)
	}
	if speechErr.Code != speechCodeUnavailable {
		t.Fatalf("Code = %q, want %q", speechErr.Code, speechCodeUnavailable)
	}
	if speechErr.Error() != speechCodeUnavailable {
		t.Fatalf("Error() = %q, want synthesized error using the code %q", speechErr.Error(), speechCodeUnavailable)
	}
}
