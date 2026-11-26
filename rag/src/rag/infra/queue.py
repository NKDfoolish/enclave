"""The ingestion queue: a Redis Stream with a consumer group.

The Core API adds one message per document change ({"document_id": ...}) from its outbox, so
a message is never lost even if Redis is down at commit time. Workers in the group "indexers"
share the stream: Redis hands each message to one worker and remembers it as pending until
that worker acknowledges it.

- A worker that crashes leaves its messages pending. After CLAIM_IDLE_MS another worker
  claims them (XAUTOCLAIM), like an expired lease in the Core API's outbox.
- A message that keeps failing is moved to a dead-letter stream after MAX_DELIVERIES.
"""

import os
import socket
from dataclasses import dataclass

import redis

STREAM = "rag:ingest"  # the dead-letter stream is STREAM + ":dead"
GROUP = "indexers"
# Long enough that a slow but alive worker (a large PDF on a local model) keeps its message.
CLAIM_IDLE_MS = 120_000
MAX_DELIVERIES = 5
# Retain a bounded diagnostic history in the dead-letter stream.
MAX_DEAD_LETTERS = 100_000


@dataclass(frozen=True)
class Message:
    id: str
    document_id: str
    deliveries: int


class IngestQueue:
    def __init__(
        self,
        client: redis.Redis,
        consumer: str | None = None,
        stream: str = STREAM,
        claim_idle_ms: int = CLAIM_IDLE_MS,
    ):
        self._redis = client
        self.consumer = consumer or f"{socket.gethostname()}-{os.getpid()}"
        # Tests use their own stream and a short idle time; the worker keeps the defaults.
        self._stream = stream
        self._dead_letter_stream = f"{stream}:dead"
        self._claim_idle_ms = claim_idle_ms

    def ensure_group(self) -> None:
        """Creates the group at the start of the stream, so messages added before any worker
        ever ran are processed too."""
        try:
            self._redis.xgroup_create(self._stream, GROUP, id="0", mkstream=True)
        except redis.ResponseError as exc:
            if "BUSYGROUP" not in str(exc):
                raise

    def publish(self, document_id: str) -> str:
        """Adds a message. The Core API publishes from Go; this is for tools and tests."""
        return self._redis.xadd(self._stream, {"document_id": document_id})

    def receive(self, block_ms: int = 5000) -> list[Message]:
        """Messages for this worker: first any left pending too long by a dead or stuck worker,
        otherwise new ones, waiting up to block_ms."""
        _, claimed, _ = self._redis.xautoclaim(
            self._stream,
            GROUP,
            self.consumer,
            min_idle_time=self._claim_idle_ms,
            start_id="0-0",
            count=1,
        )
        if claimed:
            return [self._with_deliveries(message_id, fields) for message_id, fields in claimed]

        response = self._redis.xreadgroup(
            GROUP, self.consumer, {self._stream: ">"}, count=1, block=block_ms
        )
        if not response:
            return []
        _, entries = response[0]
        return [
            Message(_text(message_id), _text(fields[b"document_id"]), 1)
            for message_id, fields in entries
        ]

    def ack(self, message: Message) -> None:
        # The ingestion stream has one consumer group. Retire only the processed entry.
        with self._redis.pipeline(transaction=True) as pipe:
            pipe.xack(self._stream, GROUP, message.id)
            pipe.xdel(self._stream, message.id)
            pipe.execute()

    def dead_letter(self, message: Message, error: str) -> None:
        """Keeps the failed message for a human and stops redelivering it."""
        with self._redis.pipeline(transaction=True) as pipe:
            pipe.xadd(
                self._dead_letter_stream,
                {
                    "document_id": message.document_id,
                    "message_id": message.id,
                    "error": error[:500],
                },
                maxlen=MAX_DEAD_LETTERS,
                approximate=True,
            )
            pipe.xack(self._stream, GROUP, message.id)
            pipe.xdel(self._stream, message.id)
            pipe.execute()

    def _with_deliveries(self, message_id: bytes, fields: dict) -> Message:
        pending = self._redis.xpending_range(
            self._stream, GROUP, min=message_id, max=message_id, count=1
        )
        deliveries = pending[0]["times_delivered"] if pending else 1
        return Message(_text(message_id), _text(fields[b"document_id"]), deliveries)


def _text(value: bytes | str) -> str:
    return value.decode() if isinstance(value, bytes) else value
