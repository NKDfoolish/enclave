package domain

import (
	"fmt"
	"unicode/utf8"
)

const (
	maxEmbeddingInputs      = 256
	maxEmbeddingInputLength = 32_000 // characters, about 8k tokens
)

// InputType tells asymmetric embedding models whether a text is a search query or a stored
// passage; they embed the two differently. Providers that do not need it ignore it.
type InputType string

const (
	InputQuery   InputType = "query"
	InputPassage InputType = "passage"
)

type EmbeddingRequest struct {
	Model     string
	Inputs    []string
	InputType InputType // empty when the caller did not say
}

func (r EmbeddingRequest) Validate() error {
	if r.Model == "" {
		return invalid("model is required")
	}
	if len(r.Inputs) == 0 || len(r.Inputs) > maxEmbeddingInputs {
		return invalid(fmt.Sprintf("input must contain 1-%d texts", maxEmbeddingInputs))
	}
	for i, input := range r.Inputs {
		if input == "" || utf8.RuneCountInString(input) > maxEmbeddingInputLength {
			return invalid(fmt.Sprintf("input[%d] must be 1-%d characters", i, maxEmbeddingInputLength))
		}
		if !utf8.ValidString(input) {
			return invalid(fmt.Sprintf("input[%d] must be valid UTF-8", i))
		}
	}
	switch r.InputType {
	case "", InputQuery, InputPassage:
	default:
		return invalid("input_type must be query or passage")
	}
	return nil
}

// Embeddings holds one vector per input, in input order.
type Embeddings struct {
	Vectors     [][]float32
	InputTokens int
}
