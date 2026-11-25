package app

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"enclave/gateway/internal/llm/domain"
)

// tokenCounter keeps the usage a provider reports and, in case it never does (a stream cut
// short), the amount of text seen, so the gateway can still estimate what was spent.
type tokenCounter struct {
	reported    *domain.Usage
	inputRunes  int
	outputRunes int
}

func newTokenCounter(req domain.ChatRequest) *tokenCounter {
	counter := &tokenCounter{}
	for _, message := range req.Messages {
		counter.inputRunes += utf8.RuneCountInString(message.Content)
	}
	return counter
}

func (c *tokenCounter) add(chunk domain.Chunk) {
	c.outputRunes += utf8.RuneCountInString(chunk.Content)
	if chunk.Usage != nil {
		usage := *chunk.Usage
		c.reported = &usage
	}
}

// usage returns the provider's figures when it reported them. Otherwise, if the provider
// produced output, it estimates them; if it never produced any (connection refused, error
// status, first-token timeout), it assumes nothing was billed and returns zero.
func (c *tokenCounter) usage(began bool) (usage domain.Usage, estimated bool) {
	if c.reported != nil {
		return *c.reported, false
	}
	if !began {
		return domain.Usage{}, false
	}
	return domain.Usage{
		InputTokens:  domain.EstimateTokens(c.inputRunes),
		OutputTokens: domain.EstimateTokens(c.outputRunes),
	}, true
}

// explainCancel turns "context canceled" into the reason the attempt was cancelled when the
// first-token timer did it, so the error is retryable and readable.
func explainCancel(attemptCtx context.Context, err error) error {
	if errors.Is(context.Cause(attemptCtx), domain.ErrFirstTokenTimeout) {
		return fmt.Errorf("%w: %w", domain.ErrFirstTokenTimeout, err)
	}
	return err
}

// attemptStatus classifies how an attempt ended. requestCtx is the whole request's context:
// if it was cancelled, the caller went away (as opposed to the request timing out).
func attemptStatus(requestCtx context.Context, began bool, err error) domain.AttemptStatus {
	switch {
	case err == nil:
		return domain.AttemptOK
	case errors.Is(requestCtx.Err(), context.Canceled):
		return domain.AttemptCancelled
	case began:
		return domain.AttemptInterrupted
	default:
		return domain.AttemptFailed
	}
}

// errorCode is a short, stable label for usage records and logs. It never contains provider
// messages, which may echo parts of the prompt.
func errorCode(err error) string {
	var providerErr *domain.ProviderError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrFirstTokenTimeout):
		return "first_token_timeout"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &providerErr) && providerErr.StatusCode == 0:
		return "provider_unreachable"
	case errors.As(err, &providerErr):
		return fmt.Sprintf("provider_status_%d", providerErr.StatusCode)
	default:
		return "error"
	}
}

func totalRunes(texts []string) int {
	total := 0
	for _, text := range texts {
		total += utf8.RuneCountInString(text)
	}
	return total
}
