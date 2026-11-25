package openai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"enclave/gateway/internal/llm/domain"
)

// maxEventLine bounds one SSE line. Chunks are small; a huge line means a broken stream.
const maxEventLine = 1 << 20

// chatChunk is the part of an OpenAI-style streaming chunk the gateway reads. Providers add
// their own fields (Ollama: system_fingerprint, NVIDIA: logprobs, reasoning models:
// reasoning_content); they are ignored.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Some providers report a failure inside the stream instead of with a status code.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chatStream reads Server-Sent Events:
//
//	data: {"choices":[{"delta":{"content":"Xin"}}]}
//	<blank line>
//	data: [DONE]
//
// Lines starting with ":" are comments (keep-alives); "event:" and "id:" lines are not used
// by this format.
type chatStream struct {
	provider string
	body     io.ReadCloser
	scanner  *bufio.Scanner
	finished bool // a finish_reason or [DONE] was seen
}

func newChatStream(provider string, body io.ReadCloser) *chatStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	return &chatStream{provider: provider, body: body, scanner: scanner}
}

// Next returns one chunk per provider event, including events with no visible text (a
// reasoning model's thinking, the usage-only final chunk). Each one proves the provider is
// alive, which is what the first-token timeout waits for.
func (s *chatStream) Next() (domain.Chunk, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		data, isData := strings.CutPrefix(line, "data:")
		if !isData {
			continue // blank separator, comment or a field this format does not use
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			s.finished = true
			return domain.Chunk{}, io.EOF
		}
		return s.parse(data)
	}
	if err := s.scanner.Err(); err != nil {
		return domain.Chunk{}, domain.NewTransportError(s.provider, fmt.Errorf("read stream: %w", err))
	}
	if !s.finished {
		// The connection closed before the provider said the answer was complete.
		return domain.Chunk{}, domain.NewTransportError(s.provider, io.ErrUnexpectedEOF)
	}
	return domain.Chunk{}, io.EOF
}

func (s *chatStream) parse(data string) (domain.Chunk, error) {
	var event chatChunk
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return domain.Chunk{}, domain.NewTransportError(s.provider, fmt.Errorf("decode stream event: %w", err))
	}
	if event.Error != nil {
		return domain.Chunk{}, domain.NewTransportError(s.provider, errors.New(event.Error.Message))
	}

	var chunk domain.Chunk
	if len(event.Choices) > 0 {
		choice := event.Choices[0]
		chunk.Content = choice.Delta.Content
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			chunk.FinishReason = domain.FinishReason(*choice.FinishReason)
			s.finished = true
		}
	}
	if event.Usage != nil {
		chunk.Usage = &domain.Usage{InputTokens: event.Usage.PromptTokens, OutputTokens: event.Usage.CompletionTokens}
	}
	return chunk, nil
}

func (s *chatStream) Close() error {
	return s.body.Close()
}
