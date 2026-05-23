package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSpeechHost       = "faster-whisper"
	defaultSpeechPort       = "10300"
	defaultSpeechLanguage   = "en"
	defaultSpeechRate       = 16000
	defaultSpeechSampleSize = 2
	defaultSpeechChannels   = 1
	defaultSpeechTimeout    = 120 * time.Second
	maxSpeechSeconds        = 120
)

var errSpeechDisabled = errors.New("speech dictation is disabled")

type speechConfig struct {
	Enabled  bool
	Host     string
	Port     string
	Language string
	Rate     int
	Width    int
	Channel  int
	Timeout  time.Duration
	MaxBytes int64
}

type speechStatus struct {
	Enabled   bool   `json:"enabled"`
	Reachable bool   `json:"reachable"`
	Host      string `json:"host,omitempty"`
	Port      string `json:"port,omitempty"`
	Language  string `json:"language,omitempty"`
	Rate      int    `json:"rate"`
}

type wyomingEventHeader struct {
	Type          string         `json:"type"`
	Data          map[string]any `json:"data,omitempty"`
	DataLength    int            `json:"data_length,omitempty"`
	PayloadLength int            `json:"payload_length,omitempty"`
}

func (s Server) handleSpeechStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := loadSpeechConfig()
	status := speechStatusFromConfig(cfg)
	if cfg.Enabled {
		status.Reachable = canReachSpeechServer(r.Context(), cfg)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"speech": status,
	})
}

func (s Server) handleSpeechTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := loadSpeechConfig()
	if !cfg.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": errSpeechDisabled.Error(),
		})
		return
	}

	pcm, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.MaxBytes+1))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok":    false,
				"error": "speech sample is too large",
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "could not read speech sample",
		})
		return
	}
	if int64(len(pcm)) > cfg.MaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"ok":    false,
			"error": "speech sample is too large",
		})
		return
	}
	if len(pcm) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "speech sample is empty",
		})
		return
	}

	text, err := transcribeSpeechPCM(r.Context(), cfg, pcm, r.URL.Query().Get("language"))
	if err != nil {
		s.logf("hub.ui status=warn endpoint=speech_transcribe err=%q", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"text": text,
	})
}

func loadSpeechConfig() speechConfig {
	host := firstNonEmptyEnv("MOLTEN_HUB_SPEECH_HOST", "MOLTENHUB_SPEECH_HOST", "WHISPER_HOST")
	if strings.TrimSpace(host) == "" {
		host = defaultSpeechHost
	}
	port := firstNonEmptyEnv("MOLTEN_HUB_SPEECH_PORT", "MOLTENHUB_SPEECH_PORT", "WHISPER_PORT")
	if strings.TrimSpace(port) == "" {
		port = defaultSpeechPort
	}
	language := configuredSpeechLanguage(firstNonEmptyEnv("MOLTEN_HUB_SPEECH_LANGUAGE", "MOLTENHUB_SPEECH_LANGUAGE", "WHISPER_LANG"))
	timeout := envDuration("MOLTEN_HUB_SPEECH_TIMEOUT_SECONDS", defaultSpeechTimeout)
	maxBytes := int64(defaultSpeechRate * defaultSpeechSampleSize * defaultSpeechChannels * maxSpeechSeconds)
	enabled := !envBool("MOLTEN_HUB_SPEECH_DISABLED") && !envBool("MOLTENHUB_SPEECH_DISABLED")
	if isDisabledSpeechHost(host) {
		enabled = false
	}

	return speechConfig{
		Enabled:  enabled,
		Host:     strings.TrimSpace(host),
		Port:     strings.TrimSpace(port),
		Language: language,
		Rate:     defaultSpeechRate,
		Width:    defaultSpeechSampleSize,
		Channel:  defaultSpeechChannels,
		Timeout:  timeout,
		MaxBytes: maxBytes,
	}
}

func speechStatusFromConfig(cfg speechConfig) speechStatus {
	return speechStatus{
		Enabled:  cfg.Enabled,
		Host:     cfg.Host,
		Port:     cfg.Port,
		Language: cfg.Language,
		Rate:     cfg.Rate,
	}
}

func canReachSpeechServer(ctx context.Context, cfg speechConfig) bool {
	ctx, cancel := context.WithTimeout(ctx, minDuration(cfg.Timeout, 750*time.Millisecond))
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func transcribeSpeechPCM(ctx context.Context, cfg speechConfig, pcm []byte, language string) (string, error) {
	if !cfg.Enabled {
		return "", errSpeechDisabled
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return "", fmt.Errorf("speech server unavailable at %s:%s", cfg.Host, cfg.Port)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))

	writer := bufio.NewWriter(conn)
	transcribeData := map[string]any{}
	if lang := resolveSpeechLanguage(language, cfg.Language); lang != "" {
		transcribeData["language"] = lang
	}
	if err := writeWyomingEvent(writer, "transcribe", transcribeData, nil); err != nil {
		return "", fmt.Errorf("start speech transcription: %w", err)
	}
	if err := writeWyomingEvent(writer, "audio-start", speechAudioFormat(cfg), nil); err != nil {
		return "", fmt.Errorf("start speech audio: %w", err)
	}
	chunkSize := cfg.Rate * cfg.Width * cfg.Channel
	for offset := 0; offset < len(pcm); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := writeWyomingEvent(writer, "audio-chunk", speechAudioFormat(cfg), pcm[offset:end]); err != nil {
			return "", fmt.Errorf("send speech audio: %w", err)
		}
	}
	if err := writeWyomingEvent(writer, "audio-stop", nil, nil); err != nil {
		return "", fmt.Errorf("stop speech audio: %w", err)
	}

	reader := bufio.NewReader(conn)
	var streamedTranscript strings.Builder
	for {
		event, err := readWyomingEvent(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if text := strings.TrimSpace(streamedTranscript.String()); text != "" {
					return text, nil
				}
			}
			return "", fmt.Errorf("read speech transcript: %w", err)
		}
		switch event.Type {
		case "transcript":
			return strings.TrimSpace(fmt.Sprint(event.Data["text"])), nil
		case "transcript-chunk":
			streamedTranscript.WriteString(fmt.Sprint(event.Data["text"]))
		case "transcript-stop":
			if text := strings.TrimSpace(streamedTranscript.String()); text != "" {
				return text, nil
			}
		case "error":
			message := strings.TrimSpace(fmt.Sprint(event.Data["text"]))
			if message == "" {
				message = strings.TrimSpace(fmt.Sprint(event.Data["message"]))
			}
			if message == "" {
				message = "speech server returned an error"
			}
			return "", errors.New(message)
		}
	}
}

func writeWyomingEvent(writer *bufio.Writer, eventType string, data map[string]any, payload []byte) error {
	header := wyomingEventHeader{Type: eventType}
	var dataPayload []byte
	if len(data) > 0 {
		var err error
		dataPayload, err = json.Marshal(data)
		if err != nil {
			return err
		}
		header.DataLength = len(dataPayload)
	}
	if len(payload) > 0 {
		header.PayloadLength = len(payload)
	}
	if err := json.NewEncoder(writer).Encode(header); err != nil {
		return err
	}
	if len(dataPayload) > 0 {
		if _, err := writer.Write(dataPayload); err != nil {
			return err
		}
	}
	if len(payload) > 0 {
		if _, err := writer.Write(payload); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func readWyomingEvent(reader *bufio.Reader) (wyomingEventHeader, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return wyomingEventHeader{}, err
	}

	var header wyomingEventHeader
	if err := json.Unmarshal(line, &header); err != nil {
		return wyomingEventHeader{}, err
	}
	if header.Data == nil {
		header.Data = map[string]any{}
	}
	if header.DataLength > 0 {
		dataPayload := make([]byte, header.DataLength)
		if _, err := io.ReadFull(reader, dataPayload); err != nil {
			return wyomingEventHeader{}, err
		}
		var data map[string]any
		if err := json.Unmarshal(dataPayload, &data); err != nil {
			return wyomingEventHeader{}, err
		}
		for key, value := range data {
			header.Data[key] = value
		}
	}
	if header.PayloadLength > 0 {
		if _, err := io.CopyN(io.Discard, reader, int64(header.PayloadLength)); err != nil {
			return wyomingEventHeader{}, err
		}
	}
	return header, nil
}

func speechAudioFormat(cfg speechConfig) map[string]any {
	return map[string]any{
		"rate":     cfg.Rate,
		"width":    cfg.Width,
		"channels": cfg.Channel,
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func isDisabledSpeechHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "0", "off", "false", "disabled", "none":
		return true
	default:
		return false
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func normalizeSpeechLanguage(language string) string {
	value := strings.ToLower(strings.TrimSpace(language))
	if value == "" || value == "auto" {
		return ""
	}
	if len(value) > 16 {
		return ""
	}
	for _, ch := range value {
		if (ch < 'a' || ch > 'z') && ch != '-' && ch != '_' {
			return ""
		}
	}
	return value
}

func configuredSpeechLanguage(language string) string {
	value := strings.ToLower(strings.TrimSpace(language))
	if value == "auto" {
		return ""
	}
	if lang := normalizeSpeechLanguage(value); lang != "" {
		return lang
	}
	return defaultSpeechLanguage
}

func resolveSpeechLanguage(requested, fallback string) string {
	if lang := normalizeSpeechLanguage(requested); lang != "" {
		return lang
	}
	return normalizeSpeechLanguage(fallback)
}
