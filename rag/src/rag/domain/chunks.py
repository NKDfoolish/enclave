"""Splitting a document's text into passages small enough to embed and to quote.

A chunk never spans two sections, so every chunk keeps an exact citation: the PDF page or the
heading it came from. Inside a section, paragraphs are packed into chunks of at most
MAX_CHARS characters, and each chunk repeats the end of the previous one (OVERLAP_CHARS) so
a sentence cut at a boundary is still found whole in one of them.
"""

import re
from dataclasses import dataclass

# About 300 tokens: small enough to point at one idea, large enough to keep its context.
MAX_CHARS = 1200
OVERLAP_CHARS = 200

_PARAGRAPH_BREAK = re.compile(r"\n\s*\n")
_SENTENCE_END = re.compile(r"(?<=[.!?…])\s+")
_WHITESPACE = re.compile(r"\s+")


@dataclass(frozen=True)
class Section:
    """A run of text with one citation location."""

    text: str
    # 1-based page for PDFs; 0 when the format has no pages.
    page: int = 0
    # Heading path such as "Refunds > Exceptions"; empty when there is none.
    heading: str = ""


@dataclass(frozen=True)
class Chunk:
    index: int
    text: str
    page: int
    heading: str


def chunk_sections(
    sections: list[Section], max_chars: int = MAX_CHARS, overlap_chars: int = OVERLAP_CHARS
) -> list[Chunk]:
    chunks: list[Chunk] = []
    for section in sections:
        for text in _pack(_units(section.text, max_chars), max_chars, overlap_chars):
            chunks.append(Chunk(len(chunks), text, section.page, section.heading))
    return chunks


def embedding_text(title: str, chunk: Chunk) -> str:
    """The text that is embedded: the passage with its document title and heading in front,
    so a passage like "It takes 14 days." is still found by "refund period"."""
    context = " > ".join(part for part in (title, chunk.heading) if part)
    return f"{context}\n\n{chunk.text}" if context else chunk.text


def _units(text: str, max_chars: int) -> list[str]:
    """Paragraphs with whitespace collapsed; any paragraph longer than max_chars is split into
    sentences, and any sentence still too long into words."""
    units: list[str] = []
    for paragraph in _PARAGRAPH_BREAK.split(text):
        paragraph = _WHITESPACE.sub(" ", paragraph).strip()
        if not paragraph:
            continue
        if len(paragraph) <= max_chars:
            units.append(paragraph)
            continue
        for sentence in _SENTENCE_END.split(paragraph):
            units.extend(_split_words(sentence, max_chars))
    return units


def _split_words(text: str, max_chars: int) -> list[str]:
    if len(text) <= max_chars:
        return [text]
    pieces, current = [], ""
    for word in text.split(" "):
        candidate = f"{current} {word}".strip()
        if len(candidate) > max_chars and current:
            pieces.append(current)
            candidate = word
        # A single "word" longer than max_chars (a URL, a table row) is cut hard.
        while len(candidate) > max_chars:
            pieces.append(candidate[:max_chars])
            candidate = candidate[max_chars:]
        current = candidate
    if current:
        pieces.append(current)
    return pieces


def _pack(units: list[str], max_chars: int, overlap_chars: int) -> list[str]:
    """Greedily fills chunks with whole units. A new chunk starts with the previous chunk's
    last units, up to overlap_chars, when they leave room for new text."""
    chunks: list[str] = []
    current: list[str] = []
    for unit in units:
        if current and _length(current + [unit]) > max_chars:
            chunks.append(" ".join(current))
            current = _overlap(current, overlap_chars)
            if _length(current + [unit]) > max_chars:
                current = []
        current.append(unit)
    if current:
        chunks.append(" ".join(current))
    return chunks


def _overlap(units: list[str], overlap_chars: int) -> list[str]:
    kept: list[str] = []
    for unit in reversed(units):
        if _length([unit] + kept) > overlap_chars:
            break
        kept.insert(0, unit)
    return kept


def _length(units: list[str]) -> int:
    return sum(len(unit) for unit in units) + max(len(units) - 1, 0)
