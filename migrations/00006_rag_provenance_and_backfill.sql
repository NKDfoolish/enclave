-- +goose Up
-- NULL identifies legacy answers whose complete model context cannot be reconstructed.
ALTER TABLE conversation_messages ADD COLUMN source_document_ids uuid[];
ALTER TABLE document_index ADD COLUMN indexed_title text;

-- Also covers ready documents created before the ingestion worker existed.
INSERT INTO outbox_jobs (kind, payload)
SELECT 'document.sync_search_index', jsonb_build_object('document_id', id)
FROM documents WHERE status = 'ready';

-- +goose Down
ALTER TABLE document_index DROP COLUMN indexed_title;
ALTER TABLE conversation_messages DROP COLUMN source_document_ids;
