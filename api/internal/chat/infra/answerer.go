package infra

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"enclave/api/internal/chat/app"
	"enclave/api/internal/chat/domain"
	"enclave/api/internal/ragclient"
	ragv1 "enclave/gen/go/enclave/rag/v1"
)

// Answerer asks the RAG service over gRPC and translates its stream into app events.
type Answerer struct {
	rag *ragclient.Client
}

func NewAnswerer(rag *ragclient.Client) *Answerer {
	return &Answerer{rag: rag}
}

func (a *Answerer) Answer(ctx context.Context, req app.AnswerRequest, onEvent func(app.AnswerEvent) error) error {
	request := &ragv1.AnswerRequest{
		WorkspaceId: req.WorkspaceID.String(),
		Question:    req.Question,
		RequestId:   req.RequestID,
	}
	for _, visibility := range req.Visibilities {
		request.Visibilities = append(request.Visibilities, string(visibility))
	}
	for _, turn := range req.History {
		request.History = append(request.History, &ragv1.Turn{Role: string(turn.Role), Content: turn.Content})
	}

	err := a.rag.Answer(ctx, request, func(resp *ragv1.AnswerResponse) error {
		event, err := toEvent(resp)
		if err != nil {
			return err
		}
		return onEvent(event)
	})
	return classify(err)
}

func toEvent(resp *ragv1.AnswerResponse) (app.AnswerEvent, error) {
	switch event := resp.GetEvent().(type) {
	case *ragv1.AnswerResponse_Sources:
		sources := make([]domain.Citation, 0, len(event.Sources.GetSources()))
		for _, source := range event.Sources.GetSources() {
			documentID, err := uuid.Parse(source.GetDocumentId())
			if err != nil {
				return app.AnswerEvent{}, fmt.Errorf("rag returned source with document id %q: %w", source.GetDocumentId(), err)
			}
			sources = append(sources, domain.Citation{
				Number:     int(source.GetNumber()),
				DocumentID: documentID,
				Title:      source.GetTitle(),
				Page:       int(source.GetPage()),
				Section:    source.GetSection(),
				Snippet:    source.GetSnippet(),
			})
		}
		return app.AnswerEvent{Kind: app.EventSources, Sources: sources}, nil
	case *ragv1.AnswerResponse_Delta:
		return app.AnswerEvent{Kind: app.EventDelta, Delta: event.Delta}, nil
	case *ragv1.AnswerResponse_Done:
		cited := make([]int, 0, len(event.Done.GetCited()))
		for _, number := range event.Done.GetCited() {
			cited = append(cited, int(number))
		}
		return app.AnswerEvent{Kind: app.EventDone, Cited: cited}, nil
	default:
		return app.AnswerEvent{}, fmt.Errorf("rag returned an empty answer event")
	}
}

// classify maps gRPC status codes to the domain errors the HTTP layer understands. A context
// error (the caller left) is passed through unchanged.
func classify(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	switch status.Code(err) {
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %w", domain.ErrInvalidQuestion, err)
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %w", domain.ErrNoModelAllowed, err)
	case codes.Canceled:
		return fmt.Errorf("%w: %w", context.Canceled, err)
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Unknown:
		return fmt.Errorf("%w: %w", domain.ErrAnswerUnavailable, err)
	default:
		return err
	}
}
