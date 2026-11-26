"""Settings for the RAG service and its ingestion worker, read from environment variables.

Every problem is collected and reported at once, so a misconfigured process fails at startup
instead of on the first document or question.
"""

import os
from dataclasses import dataclass


class ConfigError(Exception):
    pass


@dataclass(frozen=True)
class Config:
    grpc_listen: str
    database_url: str
    redis_url: str
    qdrant_url: str
    # The LLM Gateway's OpenAI-format base URL, e.g. http://localhost:8090/v1.
    gateway_url: str
    gateway_token: str
    # Gateway aliases. Changing the embedding model means re-indexing every document.
    embedding_model: str
    chat_model: str
    # How many passages the model sees, and the lowest similarity worth showing it.
    top_k: int
    min_score: float
    max_answer_tokens: int
    objectstore_endpoint: str
    objectstore_bucket: str
    objectstore_access_key: str
    objectstore_secret_key: str
    objectstore_region: str


def load() -> Config:
    errors: list[str] = []

    def required(key: str) -> str:
        value = os.environ.get(key, "")
        if not value:
            errors.append(f"{key} is required")
        return value

    def number(key: str, default: str, parse):
        raw = os.environ.get(key, default)
        try:
            return parse(raw)
        except ValueError:
            errors.append(f"{key}: {raw!r} is not a number")
            return parse(default)

    config = Config(
        grpc_listen=os.environ.get("RAG_GRPC_LISTEN", "127.0.0.1:50051"),
        database_url=required("DATABASE_URL"),
        redis_url=required("REDIS_URL"),
        qdrant_url=os.environ.get("QDRANT_URL", "http://localhost:6333"),
        gateway_url=os.environ.get("GATEWAY_URL", "http://localhost:8090/v1"),
        gateway_token=required("RAG_GATEWAY_TOKEN"),
        embedding_model=os.environ.get("RAG_EMBEDDING_MODEL", "embed-local"),
        chat_model=os.environ.get("RAG_CHAT_MODEL", "chat-fast"),
        top_k=number("RAG_TOP_K", "6", int),
        min_score=number("RAG_MIN_SCORE", "0.6", float),
        max_answer_tokens=number("RAG_MAX_ANSWER_TOKENS", "4000", int),
        objectstore_endpoint=required("OBJECTSTORE_ENDPOINT"),
        objectstore_bucket=os.environ.get("OBJECTSTORE_BUCKET", "enclave-documents"),
        objectstore_access_key=required("OBJECTSTORE_ACCESS_KEY"),
        objectstore_secret_key=required("OBJECTSTORE_SECRET_KEY"),
        objectstore_region=os.environ.get("OBJECTSTORE_REGION", "us-east-1"),
    )
    if config.top_k < 1:
        errors.append("RAG_TOP_K must be at least 1")
    if not 0 <= config.min_score < 1:
        errors.append("RAG_MIN_SCORE must be between 0 and 1")
    if errors:
        raise ConfigError("invalid config: " + "; ".join(errors))
    return config
