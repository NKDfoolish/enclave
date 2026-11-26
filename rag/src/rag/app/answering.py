"""Answering a question from the documents the caller may read.

1. Embed the question (with the previous question, for follow-ups).
2. Search the workspace's chunks, filtered by the visibilities the Core API passed.
3. Check every hit against Postgres; drop documents that were deleted or unshared since.
4. Without relevant passages, say so without calling a model.
5. Otherwise stream the model's answer and report which sources it cited.

Access is never decided by the model: it only ever sees passages that passed steps 2 and 3.
"""

import logging
from collections.abc import Iterator
from dataclasses import dataclass
from typing import Protocol

from rag.domain.answer import (
    VISIBILITIES,
    Passage,
    Source,
    Turn,
    build_messages,
    cited_numbers,
    not_enough_information,
    number_sources,
    retrieval_query,
)

log = logging.getLogger("rag.answering")

# Search wider than top_k: some hits are dropped by the score threshold or the access check.
SEARCH_FACTOR = 3
MAX_QUESTION_CHARS = 4000
MAX_HISTORY_TURNS = 10


class Embedder(Protocol):
    def embed(
        self, texts: list[str], input_type: str, workspace_id: str, request_id: str = ""
    ) -> list[list[float]]: ...


class ChatModel(Protocol):
    def stream_chat(
        self, messages: list[dict], workspace_id: str, request_id: str = ""
    ) -> Iterator[str]: ...


class PassageSearch(Protocol):
    def search(
        self, vector: list[float], workspace_id: str, visibilities: list[str], limit: int
    ) -> list[Passage]: ...


class AccessCheck(Protocol):
    def visible_titles(
        self, workspace_id: str, document_ids: list[str], visibilities: list[str]
    ) -> dict[str, str]: ...


@dataclass(frozen=True)
class Question:
    workspace_id: str
    visibilities: list[str]
    text: str
    history: list[Turn]
    request_id: str = ""


@dataclass(frozen=True)
class SourcesFound:
    sources: list[Source]


@dataclass(frozen=True)
class AnswerText:
    text: str


@dataclass(frozen=True)
class AnswerDone:
    cited: list[int]


AnswerEvent = SourcesFound | AnswerText | AnswerDone


class InvalidQuestion(ValueError):
    pass


class Answerer:
    def __init__(
        self,
        embedder: Embedder,
        chat: ChatModel,
        search: PassageSearch,
        access: AccessCheck,
        top_k: int,
        min_score: float,
    ):
        self._embedder = embedder
        self._chat = chat
        self._search = search
        self._access = access
        self._top_k = top_k
        self._min_score = min_score

    def answer(self, question: Question) -> Iterator[AnswerEvent]:
        """Yields SourcesFound, then AnswerText pieces, then AnswerDone. Stopping the iteration
        early (the caller hung up) closes the model stream."""
        _validate(question)
        sources = self.retrieve(question)
        yield SourcesFound(sources)

        if not sources:
            yield AnswerText(not_enough_information(question.text))
            yield AnswerDone([])
            return

        history = question.history[-MAX_HISTORY_TURNS:]
        messages = build_messages(question.text, sources, history)
        answer = []
        for piece in self._chat.stream_chat(messages, question.workspace_id, question.request_id):
            answer.append(piece)
            yield AnswerText(piece)
        yield AnswerDone(cited_numbers("".join(answer), len(sources)))

    def retrieve(self, question: Question) -> list[Source]:
        """The top passages the caller may read, most relevant first."""
        query = retrieval_query(question.text, question.history)
        [vector] = self._embedder.embed(
            [query],
            input_type="query",
            workspace_id=question.workspace_id,
            request_id=question.request_id,
        )
        hits = self._search.search(
            vector, question.workspace_id, question.visibilities, self._top_k * SEARCH_FACTOR
        )
        relevant = [hit for hit in hits if hit.score >= self._min_score]
        titles = self._access.visible_titles(
            question.workspace_id,
            sorted({hit.document_id for hit in relevant}),
            question.visibilities,
        )
        stale = [hit for hit in relevant if hit.document_id not in titles]
        if stale:
            # The vector store still had them; the index catches up when its message is handled.
            log.info("dropped %d passages from documents no longer visible", len(stale))
        allowed = [hit for hit in relevant if hit.document_id in titles]
        return number_sources(allowed[: self._top_k], titles)


def _validate(question: Question) -> None:
    if not question.text.strip() or len(question.text) > MAX_QUESTION_CHARS:
        raise InvalidQuestion(f"question must be 1-{MAX_QUESTION_CHARS} characters")
    if not question.visibilities or not set(question.visibilities) <= VISIBILITIES:
        raise InvalidQuestion("visibilities must be a non-empty subset of internal, client_common")
    if any(turn.role not in ("user", "assistant") for turn in question.history):
        raise InvalidQuestion("history roles must be user or assistant")
