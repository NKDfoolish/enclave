# Part 4: RAG and Q&A — Summary

Branch: `part-4-rag`. Flow diagrams: [docs/flows.md](../flows.md) sections 29–34.
Decisions and trade-offs: [ADR 0005](../adr/0005-rag-and-qa.md).

The plan's "Done when" criteria:

- [x] Staff and clients get grounded answers from permitted documents, or an explicit "not enough
  information" response.
- [x] A 30–50 question evaluation covers answer quality, citations, unanswerable questions, and
  permission boundaries: 42 questions in `rag/eval/`, run with `make rag-eval`.
- [x] Retrieval quality and latency are measured on a fixed dataset; retries do not duplicate
  chunks or resurrect deleted documents (tests: deterministic point IDs, stale-chunk removal,
  the check after writing).

Tests: Python 52 (chunking, parsing of real PDF/DOCX/Markdown, indexing and answering with fakes,
gRPC streaming and cancellation, Redis Streams / Qdrant / Postgres integration). Go: the chat
context end to end (`api/cmd/api/chat_test.go`), index sync on every document change, chat domain
rules. Mutation checks: 14/14 caught.

---

## 1. What was built

```text
rag/src/rag/
├── domain/      chunks.py (sections → chunks), answer.py (prompt, citations, refusal text),
│                documents.py (records and errors shared by the layers)
├── app/         indexing.py (Indexer.sync: reconcile the index with Postgres)
│                answering.py (Answerer: retrieve, re-check access, stream the answer)
├── infra/       parsing.py (pypdf, python-docx, Markdown), gateway.py (openai client → gateway),
│                vectors.py (Qdrant), postgres.py, storage.py (S3), queue.py (Redis Streams)
├── transport/   grpc_server.py (Ping, Answer stream)
├── main.py      gRPC server        worker.py   ingestion worker        reindex.py
rag/eval/        corpus.py (4 sample documents), questions.json (42), run.py (+ Ragas)
api/internal/chat/            NEW context: conversations, Ask (SSE), hidden answers
api/internal/document/        sync_search_index outbox job → Redis Stream; index_status; Readable
api/internal/ragclient        Answer: gRPC server streaming with a 3-minute deadline
proto/enclave/rag/v1          Answer RPC: sources, deltas, cited numbers
gateway/                      embedding input prefixes, JSON response format, top_p, chat-judge
migrations/00005              document_index, conversations, conversation_messages
web/static                    "Ask the documents" panel; "searchable" status per document
```

## 2. Design decisions

| Decision | Why |
|---|---|
| Outbox → Redis Stream → worker | The outbox makes the hand-over survive a Redis outage; the stream lets several workers share the load with acknowledgements, like a lease. |
| The worker reconciles instead of replaying commands | One message type ("this document changed") covers ready, share, unshare and delete; duplicates and reordering cannot corrupt the index. |
| Check Postgres again after writing vectors | A document deleted while it was being embedded is removed instead of resurrected. |
| Point ID = uuid5(document, chunk index) | Retries overwrite; nothing is duplicated. Leftover chunks of a longer version are deleted after the upsert. |
| Chunks never cross sections; title and heading are embedded with the text | Every chunk keeps an exact page or heading for its citation, and short passages keep their context. |
| `pypdf` + `python-docx` instead of docling | docling needs PyTorch and model downloads; too heavy for now. |
| Local embeddings by default; one collection per model | Content stays inside Enclave; vectors of different models are never mixed. |
| Prefixes ("search_query: ") added by the gateway | Model-specific knowledge belongs with the model's route in the catalog, not in every caller. |
| Score threshold, then no model call | Clearly unrelated questions get "not enough information" instantly and deterministically. |
| Postgres re-check after vector search | The vector store's copy of visibility can lag an unshare; the model only ever sees passages Postgres allows right now. |
| Conversations in the Core API, private to their owner | Access rules live where the users and roles are; RAG stays stateless. |
| Hide earlier answers whose sources are no longer readable | An answer can quote a document; losing access to the document must hide the quote too. |
| gRPC server streaming, then SSE to the browser | The first words show after about 0.2 s; cancelling at the browser stops the model. |
| Evaluation through the real API as real roles | Permission boundaries are tested the way users meet them, not only in unit tests. |

## 3. Concepts worth knowing

- **RAG**: retrieve passages, put them in the prompt, ask the model to answer only from them and
  cite them. Quality depends on retrieval first: a model cannot cite what it was never given.
- **Embeddings and cosine similarity**: texts become vectors; similar meaning, similar direction.
  Scores are only comparable within one model.
- **Asymmetric embedding**: queries and passages are embedded differently (nomic's prefixes,
  NVIDIA's `input_type`).
- **Chunking with overlap**: small enough to be specific, overlapping so a fact cut at a boundary
  is whole in one chunk.
- **Redis Streams consumer groups**: `XREADGROUP` hands each message to one consumer; `XACK`
  confirms; the pending entries list remembers the rest; `XAUTOCLAIM` takes over idle messages;
  a dead-letter stream stops endless retries.
- **Reconciliation (level-triggered)**: act on the current state, not on the event. The same idea
  as Kubernetes controllers and Terraform.
- **Prompt injection**: documents are untrusted input. Sources are quoted as data with an explicit
  rule to ignore instructions in them; more importantly, access is enforced before the model, so
  even a fooled model has nothing forbidden to reveal.
- **Evaluation**: retrieval hit rate, citation accuracy, answer correctness, refusal rate,
  permission safety, latency; Ragas adds LLM-judged faithfulness (claims supported by the
  sources) and answer relevancy.

## 4. Measurements

`make rag-eval`, 42 questions, local models (`chat-fast` = Ollama `llama3.2:3b`,
`embed-local` = Ollama `nomic-embed-text`), Apple M4:

| | min score 0.5 | **min score 0.6** (default) | NVIDIA embeddings, no threshold |
|---|---|---|---|
| Expected document retrieved | 25/26 (96%) | 25/26 (96%) | **26/26 (100%)** |
| Expected document cited | 24/26 (92%) | 25/26 (96%) | 25/26 (96%) |
| Expected fact in the answer | 24/26 (92%) | 23/26 (88%)¹ | 25/26 (96%) |
| Unanswerable declined | 5/8 (62%) | **8/8 (100%)** | 7/8 (88%) |
| Permission probes safe | 8/8 (100%) | 8/8 (100%) | 8/8 (100%) |
| First token p50 / p95 | 0.55 / 0.80 s | 0.17 / 0.54 s | 1.12 / 1.43 s |
| Full answer p50 / p95 | 0.98 / 1.74 s | 0.66 / 1.06 s | 1.59 / 1.96 s |

¹ One "miss" was a correct answer worded differently ("cannot be refunded"); the expected
phrasings were widened afterwards. The two real misses: an English question about a Vietnamese
document (nomic is English-centred; NVIDIA found it) and a table the 3B model misread.

Top similarity per question with `embed-local`: answerable 0.61–0.87, unanswerable 0.50–0.73,
permission probes (no internal passage reachable) 0.47–0.70. With `embed-nvidia`: answerable
0.31–0.71, unanswerable 0.11–0.40, probes 0.03–0.26. Thresholds do not transfer between models.

Ragas (`ARGS=--ragas`, judge `chat-judge` = NVIDIA `nemotron-3.5-lightning`, contexts = the full
text of the cited documents): **faithfulness 0.84**, **answer relevancy 0.76** over 21 answered
questions. Three more were skipped because the reasoning judge ran out of its 4,000 output tokens
before producing JSON. The run took about 20 minutes, almost all of it judge calls (7–46 s each).

## 5. Things found while building

| Finding | Resolution |
|---|---|
| `ragas` 0.4.3 imports a module `langchain-community` removed in 0.4.2 | Pinned `langchain-community==0.4.1` in the eval group. |
| `ragas` needs `instructor`, which supports `openai` 2.x only | The RAG service accepts `openai>=2.0`; the lock picks a version both accept. |
| Ragas asks for `response_format: json_object`; the gateway rejected unknown fields | The gateway now supports JSON output (OpenAI-format providers natively, Anthropic through the system prompt) and `top_p`. |
| `nomic-embed-text` retrieves worse without its task prefixes | The catalog route declares `input_prefixes`; the gateway adds them. |
| An `AnswerEvent` told apart by "is the sources list nil" would misread an empty list | Explicit `Kind` field. |
| `app/indexing.py` first imported infra types | Records and errors moved to `domain`; the parser is passed in. |
| A reasoning model as judge spends up to 4,000 tokens per call | Ragas runs are slow (minutes per ten questions); `chat-judge` is its own alias so it can be swapped. |

## 6. Deliberately left for later

- Hybrid search (keyword + vector) and reranking (`nemotron-3.5-rerank` is available on NVIDIA).
- OCR and layout-aware parsing (docling) for scanned PDFs and complex tables.
- Separate gateway budgets for indexing and answering; summarising long conversations.
- Streaming the answer's citations as they appear instead of at the end.
