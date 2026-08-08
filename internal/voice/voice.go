package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

type Transcriber struct {
	APIKey     string
	HTTPClient *http.Client
}

func New(apiKey string) *Transcriber {
	return &Transcriber{
		APIKey: apiKey,
		HTTPClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

func (t *Transcriber) Enabled() bool {
	return t != nil && t.APIKey != ""
}

func (t *Transcriber) Transcribe(ctx context.Context, filename string, audio io.Reader) (string, error) {
	if !t.Enabled() {
		return "", fmt.Errorf("OPENAI_API_KEY is not set")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", err
	}
	_ = w.WriteField("model", "whisper-1")
	_ = w.WriteField("language", "ru")
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper status %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	return parsed.Text, nil
}
