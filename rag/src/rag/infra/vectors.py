"""Chunk vectors in Qdrant.

One collection per embedding model: vectors from different models cannot be compared, so a
model change means a new collection and re-indexing (ADR 0005). Each point carries the
workspace and visibility of its document so search can filter on them; Postgres stays the
source of truth and is checked again after every search.
"""

import uuid

from qdrant_client import QdrantClient, models

from rag.domain.answer import Passage
from rag.domain.chunks import Chunk

# Fixed namespace so a chunk's point ID is the same on every run: re-indexing overwrites
# points instead of adding duplicates.
_POINT_NAMESPACE = uuid.UUID("6f1c2a52-8d0e-4a3e-9a51-6f0f1b0d7c11")
_KEYWORD_FIELDS = ("workspace_id", "document_id", "visibility")


def point_id(document_id: str, chunk_index: int) -> str:
    return str(uuid.uuid5(_POINT_NAMESPACE, f"{document_id}:{chunk_index}"))


def collection_name(embedding_model: str) -> str:
    return "chunks_" + "".join(c if c.isalnum() or c in "-_" else "_" for c in embedding_model)


class VectorStore:
    def __init__(self, url: str, embedding_model: str):
        self._client = QdrantClient(url=url, timeout=30)
        self.collection = collection_name(embedding_model)

    def ensure_collection(self, dimension: int) -> None:
        """Creates the collection on first use. An existing one with another dimension means
        the configuration changed without a new collection name: refuse rather than mix."""
        if self._client.collection_exists(self.collection):
            existing = self._client.get_collection(self.collection).config.params.vectors.size
            if existing != dimension:
                raise RuntimeError(
                    f"collection {self.collection} holds {existing}-dimension vectors, "
                    f"but the embedding model returns {dimension}"
                )
            return
        self._client.create_collection(
            self.collection,
            vectors_config=models.VectorParams(size=dimension, distance=models.Distance.COSINE),
        )
        for field in _KEYWORD_FIELDS:
            self._client.create_payload_index(
                self.collection, field, field_schema=models.PayloadSchemaType.KEYWORD
            )
        self._client.create_payload_index(
            self.collection, "chunk_index", field_schema=models.PayloadSchemaType.INTEGER
        )

    def replace_document(
        self,
        document_id: str,
        workspace_id: str,
        visibility: str,
        chunks: list[Chunk],
        vectors: list[list[float]],
    ) -> None:
        """Writes the document's chunks, then removes chunks left over from a longer earlier
        version. Upserting first means search never sees the document missing."""
        points = [
            models.PointStruct(
                id=point_id(document_id, chunk.index),
                vector=vector,
                payload={
                    "workspace_id": workspace_id,
                    "document_id": document_id,
                    "visibility": visibility,
                    "chunk_index": chunk.index,
                    "text": chunk.text,
                    "page": chunk.page,
                    "heading": chunk.heading,
                },
            )
            for chunk, vector in zip(chunks, vectors, strict=True)
        ]
        self._client.upsert(self.collection, points=points, wait=True)
        self._client.delete(
            self.collection,
            points_selector=models.FilterSelector(
                filter=models.Filter(
                    must=[
                        _match("document_id", document_id),
                        models.FieldCondition(
                            key="chunk_index", range=models.Range(gte=len(chunks))
                        ),
                    ]
                )
            ),
            wait=True,
        )

    def set_visibility(self, document_id: str, visibility: str) -> None:
        if not self._client.collection_exists(self.collection):
            return
        self._client.set_payload(
            self.collection,
            payload={"visibility": visibility},
            points=models.Filter(must=[_match("document_id", document_id)]),
            wait=True,
        )

    def delete_document(self, document_id: str) -> None:
        if not self._client.collection_exists(self.collection):
            return
        self._client.delete(
            self.collection,
            points_selector=models.FilterSelector(
                filter=models.Filter(must=[_match("document_id", document_id)])
            ),
            wait=True,
        )

    def search(
        self, vector: list[float], workspace_id: str, visibilities: list[str], limit: int
    ) -> list[Passage]:
        """Nearest chunks inside one workspace, among the visibilities the caller may read."""
        if not self._client.collection_exists(self.collection):
            return []
        result = self._client.query_points(
            self.collection,
            query=vector,
            query_filter=models.Filter(
                must=[
                    _match("workspace_id", workspace_id),
                    models.FieldCondition(
                        key="visibility", match=models.MatchAny(any=visibilities)
                    ),
                ]
            ),
            limit=limit,
            with_payload=True,
        )
        return [
            Passage(
                document_id=point.payload["document_id"],
                chunk_index=point.payload["chunk_index"],
                text=point.payload["text"],
                page=point.payload.get("page", 0),
                heading=point.payload.get("heading", ""),
                score=point.score,
            )
            for point in result.points
        ]

    def count(self, document_id: str) -> int:
        if not self._client.collection_exists(self.collection):
            return 0
        return self._client.count(
            self.collection,
            count_filter=models.Filter(must=[_match("document_id", document_id)]),
            exact=True,
        ).count


def _match(key: str, value: str) -> models.FieldCondition:
    return models.FieldCondition(key=key, match=models.MatchValue(value=value))
