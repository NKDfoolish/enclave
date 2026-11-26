"""Calls to the LLM Gateway (Part 3) with the standard openai client.

The gateway decides which provider answers, enforces the workspace's AI settings and rate
limit, and records the cost; this client only names the alias and the workspace.
"""

from collections.abc import Iterator

import openai

from rag.domain.documents import GatewayError

WORKSPACE_HEADER = "X-Enclave-Workspace-ID"
REQUEST_ID_HEADER = "X-Request-ID"

# The gateway accepts at most 256 inputs per call; smaller batches keep each request quick.
EMBED_BATCH = 32

# Statuses that blame the request or the configuration: retrying gives the same answer.
_PERMANENT_STATUSES = {400, 401, 403, 404, 413, 422}


class Gateway:
    def __init__(
        self,
        base_url: str,
        token: str,
        embedding_model: str,
        chat_model: str,
        max_answer_tokens: int,
        timeout_seconds: float = 300,
    ):
        # Retries are off: the gateway already falls back between providers, and the worker
        # retries whole documents.
        self._client = openai.OpenAI(
            base_url=base_url, api_key=token, max_retries=0, timeout=timeout_seconds
        )
        self.embedding_model = embedding_model
        self._chat_model = chat_model
        self._max_answer_tokens = max_answer_tokens

    def embed(
        self, texts: list[str], input_type: str, workspace_id: str, request_id: str = ""
    ) -> list[list[float]]:
        """One vector per text, in order. input_type is "query" or "passage"."""
        vectors: list[list[float]] = []
        for start in range(0, len(texts), EMBED_BATCH):
            batch = texts[start : start + EMBED_BATCH]
            try:
                response = self._client.embeddings.create(
                    model=self.embedding_model,
                    input=batch,
                    extra_body={"input_type": input_type},
                    extra_headers=_headers(workspace_id, request_id),
                )
            except openai.APIStatusError as exc:
                raise _status_error("embed", exc) from exc
            except openai.APIConnectionError as exc:
                raise GatewayError(f"embed: gateway unreachable: {exc}", retryable=True) from exc
            vectors += [item.embedding for item in sorted(response.data, key=lambda d: d.index)]
        return vectors

    def stream_chat(
        self, messages: list[dict], workspace_id: str, request_id: str = ""
    ) -> Iterator[str]:
        """Yields the answer text as the gateway streams it. Closing the iterator early closes
        the HTTP connection, and the gateway then stops the provider."""
        try:
            stream = self._client.chat.completions.create(
                model=self._chat_model,
                messages=messages,
                stream=True,
                max_tokens=self._max_answer_tokens,
                extra_headers=_headers(workspace_id, request_id),
            )
        except openai.APIStatusError as exc:
            raise _status_error("chat", exc) from exc
        except openai.APIConnectionError as exc:
            raise GatewayError(f"chat: gateway unreachable: {exc}", retryable=True) from exc
        with stream:
            for chunk in stream:
                if chunk.choices and chunk.choices[0].delta.content:
                    yield chunk.choices[0].delta.content


def _headers(workspace_id: str, request_id: str) -> dict[str, str]:
    headers = {WORKSPACE_HEADER: workspace_id}
    if request_id:
        headers[REQUEST_ID_HEADER] = request_id
    return headers


def _status_error(operation: str, exc: openai.APIStatusError) -> GatewayError:
    # The gateway's error body is {"error": {"code": ...}}; the client may hand over either the
    # whole body or just the inner object.
    body = exc.body if isinstance(exc.body, dict) else {}
    inner = body.get("error")
    code = str(inner.get("code", "")) if isinstance(inner, dict) else str(body.get("code", ""))
    return GatewayError(
        f"{operation}: gateway answered {exc.status_code} {code}".strip(),
        retryable=exc.status_code not in _PERMANENT_STATUSES,
    )
