package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestHandleSpeechStatusReportsReachableSidecar(t *testing.T) {
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		_ = conn.Close()
	})

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	t.Setenv("MOLTEN_HUB_SPEECH_HOST", host)
	t.Setenv("MOLTEN_HUB_SPEECH_PORT", port)
	t.Setenv("MOLTEN_HUB_SPEECH_LANGUAGE", "")
	t.Setenv("MOLTENHUB_SPEECH_LANGUAGE", "")
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
	t.Setenv("MOLTEN_HUB_SPEECH_HOST", host)
	t.Setenv("MOLTEN_HUB_SPEECH_PORT", port)
	t.Setenv("MOLTEN_HUB_SPEECH_LANGUAGE", "")
	t.Setenv("MOLTENHUB_SPEECH_LANGUAGE", "")
	t.Setenv("WHISPER_LANG", "")

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

	wantTypes := []string{"transcribe", "audio-start", "audio-chunk", "audio-stop"}
	if got := <-received; !reflect.DeepEqual(got.types, wantTypes) {
		t.Fatalf("Wyoming event types = %#v, want %#v", got.types, wantTypes)
	} else if got.language != defaultSpeechLanguage {
		t.Fatalf("Wyoming transcribe language = %q, want %q", got.language, defaultSpeechLanguage)
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
	t.Setenv("MOLTEN_HUB_SPEECH_HOST", host)
	t.Setenv("MOLTEN_HUB_SPEECH_PORT", port)

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

func TestLoadSpeechConfigAllowsAutomaticLanguage(t *testing.T) {
	t.Setenv("MOLTEN_HUB_SPEECH_LANGUAGE", "auto")

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
