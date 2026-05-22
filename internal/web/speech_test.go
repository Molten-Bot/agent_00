package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
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
}

func TestHandleSpeechTranscribeUsesWyomingServer(t *testing.T) {
	received := make(chan []string, 1)
	listener := listenFakeSpeechServer(t, func(conn net.Conn) {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		var types []string
		for {
			event, err := readWyomingEvent(reader)
			if err != nil {
				t.Errorf("read Wyoming event: %v", err)
				return
			}
			types = append(types, event.Type)
			if event.Type == "audio-stop" {
				break
			}
		}
		received <- types
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

	srv := NewServer("", NewBroker())
	req := httptest.NewRequest(http.MethodPost, "/api/speech/transcribe?language=en", bytes.NewReader([]byte{1, 0, 2, 0}))
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
	if got := <-received; !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("Wyoming event types = %#v, want %#v", got, wantTypes)
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
