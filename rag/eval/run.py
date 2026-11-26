"""Evaluates Q&A end to end, through the Core API, as real users would use it.

It creates a workspace with an admin, a staff member and a client, uploads the sample
corpus (eval/corpus.py), waits until the RAG worker has indexed it, then asks every question
in eval/questions.json as the role the question names. It measures:

- retrieval: is the expected document among the sources found?
- citation: does the answer cite the expected document?
- correctness: does the answer contain one of the expected facts?
- unanswerable: does the answer decline, citing nothing?
- permission: does a client's answer avoid every internal document and fact? (must be 100%)
- latency: time to the first word and to the full answer.

With --ragas it also scores answer relevancy and faithfulness with Ragas, using the gateway's
chat-judge model. Needs the Core API, the gateway, the RAG server and worker running, and
Ollama serving. Run it with `make rag-eval` (add ARGS=--ragas).
"""

import argparse
import asyncio
import json
import os
import statistics
import sys
import time
import uuid
from dataclasses import asdict, dataclass, field
from pathlib import Path

import httpx

from eval.corpus import CORPUS, INTERNAL_TITLES

API_URL = os.environ.get("EVAL_API_URL", "http://localhost:8080")
GATEWAY_URL = os.environ.get("GATEWAY_URL", "http://localhost:8090/v1")
GATEWAY_TOKEN = os.environ.get("EVAL_GATEWAY_TOKEN", "enclave-dev-gateway-token")
PASSWORD = "eval-password-0123456789"
RESULTS = Path(__file__).parent / "results"
QUESTIONS = Path(__file__).parent / "questions.json"
REFUSALS = ("not contain enough", "không có đủ", "do not contain", "does not contain")


class ApiError(Exception):
    pass


class User:
    def __init__(self, name: str):
        self.email = f"eval-{name}-{uuid.uuid4().hex[:8]}@example.com"
        self.http = httpx.Client(base_url=API_URL, timeout=300)
        self.call(
            "POST",
            "/v1/auth/register",
            {"email": self.email, "display_name": name, "password": PASSWORD},
        )
        self.call("POST", "/v1/auth/login", {"email": self.email, "password": PASSWORD})

    def call(self, method: str, path: str, body: dict | None = None) -> dict:
        response = self.http.request(method, path, json=body)
        if response.status_code >= 400:
            raise ApiError(f"{method} {path}: {response.status_code} {response.text}")
        return response.json() if response.content else {}


@dataclass
class Answer:
    sources: list[dict] = field(default_factory=list)
    text: str = ""
    citations: list[dict] = field(default_factory=list)
    error: str = ""
    first_token_s: float | None = None
    total_s: float = 0.0


def ask(user: User, workspace_id: str, question: str) -> Answer:
    conversation = user.call("POST", f"/v1/workspaces/{workspace_id}/conversations")
    path = f"/v1/workspaces/{workspace_id}/conversations/{conversation['id']}/messages"
    answer = Answer()
    started = time.monotonic()
    event = ""
    with user.http.stream("POST", path, json={"content": question}) as response:
        if response.status_code >= 400:
            response.read()
            answer.error = f"{response.status_code} {response.text}"
            return answer
        for line in response.iter_lines():
            if line.startswith("event: "):
                event = line[len("event: ") :]
            elif line.startswith("data: "):
                data = json.loads(line[len("data: ") :])
                if event == "sources":
                    answer.sources = data["sources"]
                elif event == "delta":
                    if answer.first_token_s is None:
                        answer.first_token_s = time.monotonic() - started
                    answer.text += data["text"]
                elif event == "done":
                    answer.citations = data["message"]["citations"]
                elif event == "error":
                    answer.error = data["error"]["message"]
    answer.total_s = time.monotonic() - started
    return answer


def set_up(allow_nvidia: bool) -> tuple[str, dict[str, User]]:
    admin, staff, client = User("admin"), User("staff"), User("client")
    workspace_id = admin.call("POST", "/v1/workspaces", {"name": "Lumen Coffee (eval)"})["id"]
    for user, role in ((staff, "staff"), (client, "client")):
        invitation = admin.call(
            "POST",
            f"/v1/workspaces/{workspace_id}/invitations",
            {"email": user.email, "role": role},
        )
        user.call("POST", "/v1/invitations/accept", {"token": invitation["token"]})
    if allow_nvidia:
        # For the judge (chat-judge) or NVIDIA embeddings; chat-fast still tries Ollama first.
        admin.call(
            "PUT", f"/v1/workspaces/{workspace_id}/ai-settings", {"external_providers": ["nvidia"]}
        )

    for doc in CORPUS:
        content = doc.content()
        created = admin.call(
            "POST",
            f"/v1/workspaces/{workspace_id}/documents",
            {"title": doc.title, "filename": doc.filename, "size_bytes": len(content)},
        )
        upload = created["upload"]
        stored = httpx.request(
            upload["method"], upload["url"], headers=upload["headers"], content=content, timeout=60
        )
        stored.raise_for_status()
        document_id = created["document"]["id"]
        admin.call("POST", f"/v1/workspaces/{workspace_id}/documents/{document_id}/complete")
        if doc.visibility == "client_common":
            wait_until(
                lambda d=document_id: status_of(admin, workspace_id, d)["status"] == "ready", 60
            )
            admin.call(
                "PATCH",
                f"/v1/workspaces/{workspace_id}/documents/{document_id}",
                {"visibility": "client_common"},
            )

    print("waiting for the RAG worker to index the corpus...", flush=True)
    wait_until(lambda: all_indexed(admin, workspace_id), 600)
    return workspace_id, {"admin": admin, "staff": staff, "client": client}


def status_of(user: User, workspace_id: str, document_id: str) -> dict:
    return user.call("GET", f"/v1/workspaces/{workspace_id}/documents/{document_id}")


def all_indexed(user: User, workspace_id: str) -> bool:
    documents = user.call("GET", f"/v1/workspaces/{workspace_id}/documents")["documents"]
    failed = [d["title"] for d in documents if d.get("index_status") == "failed"]
    if failed:
        raise ApiError(f"indexing failed for {failed}")
    return len(documents) == len(CORPUS) and all(
        d.get("index_status") == "indexed" for d in documents
    )


def wait_until(condition, timeout_s: float) -> None:
    deadline = time.monotonic() + timeout_s
    while not condition():
        if time.monotonic() > deadline:
            raise TimeoutError("timed out waiting")
        time.sleep(1)


def score(question: dict, answer: Answer) -> dict:
    source_titles = [s["title"] for s in answer.sources]
    cited_titles = [c["title"] for c in answer.citations]
    text = answer.text.casefold()
    result = {"id": question["id"], "kind": question["kind"], "error": answer.error}
    if question["kind"] == "answerable":
        result["retrieved"] = question["expected_doc"] in source_titles
        result["cited"] = question["expected_doc"] in cited_titles
        result["correct"] = any(k.casefold() in text for k in question["expect_any"])
    elif question["kind"] == "unanswerable":
        result["declined"] = not answer.citations or any(r in text for r in REFUSALS)
    else:
        leaked_source = any(title in INTERNAL_TITLES for title in source_titles)
        leaked_fact = any(f.casefold() in text for f in question["forbidden"])
        result["safe"] = not leaked_source and not leaked_fact
    return result


def rate(results: list[dict], kind: str, key: str) -> str:
    rows = [r for r in results if r["kind"] == kind]
    passed = sum(1 for r in rows if r.get(key))
    return f"{passed}/{len(rows)} ({100 * passed / len(rows):.0f}%)" if rows else "-"


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return values[0]
    return statistics.quantiles(values, n=100, method="inclusive")[int(p) - 1]


async def ragas_scores(questions: list[dict], answers: list[Answer], workspace_id: str) -> dict:
    """Answer relevancy and faithfulness for answered questions. Contexts are the full text of
    the documents the answer drew on (the sample documents are short)."""
    from openai import AsyncOpenAI
    from ragas.embeddings import embedding_factory
    from ragas.llms import llm_factory
    from ragas.metrics.collections import AnswerRelevancy, Faithfulness

    client = AsyncOpenAI(
        base_url=GATEWAY_URL,
        api_key=GATEWAY_TOKEN,
        default_headers={"X-Enclave-Workspace-ID": workspace_id},
        max_retries=6,  # the gateway's rate limit answers 429 with Retry-After
    )
    llm = llm_factory("chat-judge", client=client, max_tokens=4000)
    embeddings = embedding_factory("openai", model="embed-local", client=client)
    faithfulness = Faithfulness(llm=llm)
    relevancy = AnswerRelevancy(llm=llm, embeddings=embeddings, strictness=1)
    texts = {doc.title: doc.text for doc in CORPUS}

    faithful, relevant = [], []
    for question, answer in zip(questions, answers, strict=True):
        if question["kind"] != "answerable" or answer.error or not answer.citations:
            continue
        contexts = [texts[title] for title in {c["title"] for c in answer.citations}]
        try:
            faithful.append(
                (await faithfulness.ascore(question["question"], answer.text, contexts)).value
            )
            relevant.append((await relevancy.ascore(question["question"], answer.text)).value)
        except Exception as exc:  # a judge failure skips one question, not the run
            print(f"  ragas skipped {question['id']}: {exc}", file=sys.stderr)
    return {
        "faithfulness": round(statistics.mean(faithful), 3) if faithful else None,
        "answer_relevancy": round(statistics.mean(relevant), 3) if relevant else None,
        "scored": len(faithful),
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument(
        "--ragas", action="store_true", help="also score with Ragas (uses chat-judge)"
    )
    parser.add_argument(
        "--allow-nvidia",
        action="store_true",
        help="allow the NVIDIA provider in the eval workspace (needed for embed-nvidia)",
    )
    args = parser.parse_args()

    questions = json.loads(QUESTIONS.read_text())
    workspace_id, users = set_up(allow_nvidia=args.ragas or args.allow_nvidia)

    answers, results = [], []
    for question in questions:
        answer = ask(users[question["role"]], workspace_id, question["question"])
        answers.append(answer)
        result = score(question, answer)
        results.append(
            result | {"answer": answer.text, "sources": [s["title"] for s in answer.sources]}
        )
        print(
            f"{question['id']} {question['role']:6} {answer.total_s:5.1f}s {answer.text[:90]!r}",
            flush=True,
        )

    first_tokens = [a.first_token_s for a in answers if a.first_token_s is not None]
    totals = [a.total_s for a in answers if not a.error]
    summary = {
        "questions": len(questions),
        "errors": sum(1 for a in answers if a.error),
        "retrieval_hit": rate(results, "answerable", "retrieved"),
        "cited_expected_doc": rate(results, "answerable", "cited"),
        "correct_fact": rate(results, "answerable", "correct"),
        "declined_unanswerable": rate(results, "unanswerable", "declined"),
        "permission_safe": rate(results, "permission", "safe"),
        "first_token_p50_s": round(percentile(first_tokens, 50), 2),
        "first_token_p95_s": round(percentile(first_tokens, 95), 2),
        "answer_p50_s": round(percentile(totals, 50), 2),
        "answer_p95_s": round(percentile(totals, 95), 2),
        "chat_model": os.environ.get("RAG_CHAT_MODEL", "chat-fast"),
        "embedding_model": os.environ.get("RAG_EMBEDDING_MODEL", "embed-local"),
    }
    if args.ragas:
        summary["ragas"] = asyncio.run(ragas_scores(questions, answers, workspace_id))

    RESULTS.mkdir(exist_ok=True)
    report = {"summary": summary, "results": results, "answers": [asdict(a) for a in answers]}
    (RESULTS / "latest.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    if not summary["permission_safe"].startswith(
        f"{len([q for q in questions if q['kind'] == 'permission'])}/"
    ):
        sys.exit("permission boundary broken: a client saw internal content")


if __name__ == "__main__":
    main()
