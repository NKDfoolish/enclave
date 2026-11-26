"""Extracting text from the formats the Core API accepts, as sections that keep their
citation location: one section per PDF page, one per heading in DOCX and Markdown.

The Core API has already checked each file's bytes against its type (Part 2), but documents
are still untrusted input: any library error becomes ParseError, so the document is marked
failed instead of being retried forever.
"""

import io
import re

import docx
from docx.table import Table
from pypdf import PdfReader

from rag.domain.chunks import Section
from rag.domain.documents import ParseError

PDF = "application/pdf"
MARKDOWN = "text/markdown"
DOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

_MARKDOWN_HEADING = re.compile(r"^(#{1,6})\s+(.+?)\s*#*\s*$")


def parse_document(content_type: str, data: bytes) -> list[Section]:
    try:
        if content_type == PDF:
            return _parse_pdf(data)
        if content_type == MARKDOWN:
            return _parse_markdown(data.decode("utf-8"))
        if content_type == DOCX:
            return _parse_docx(data)
    except ParseError:
        raise
    except Exception as exc:  # library errors on malformed files
        raise ParseError(f"cannot read {content_type}: {exc}") from exc
    raise ParseError(f"unsupported content type {content_type}")


def _parse_pdf(data: bytes) -> list[Section]:
    reader = PdfReader(io.BytesIO(data))
    sections = []
    for number, page in enumerate(reader.pages, start=1):
        text = page.extract_text() or ""
        if text.strip():
            sections.append(Section(text=text, page=number))
    return sections


def _parse_markdown(text: str) -> list[Section]:
    sections: list[Section] = []
    headings: list[str] = []  # headings[i] is the current level-(i+1) heading
    lines: list[str] = []
    in_code_block = False

    def flush() -> None:
        body = "\n".join(lines).strip()
        if body:
            sections.append(Section(text=body, heading=" > ".join(h for h in headings if h)))
        lines.clear()

    for line in text.splitlines():
        if line.strip().startswith("```"):
            in_code_block = not in_code_block
        match = None if in_code_block else _MARKDOWN_HEADING.match(line)
        if match:
            flush()
            level = len(match.group(1))
            headings[:] = headings[: level - 1] + [""] * max(0, level - 1 - len(headings))
            headings.append(match.group(2))
        else:
            lines.append(line)
    flush()
    return sections


def _parse_docx(data: bytes) -> list[Section]:
    """Paragraphs in order, split at Heading styles. Tables are read row by row, since policy
    documents often keep their key facts in tables."""
    document = docx.Document(io.BytesIO(data))
    sections: list[Section] = []
    headings: list[str] = []
    lines: list[str] = []

    def flush() -> None:
        body = "\n\n".join(lines).strip()
        if body:
            sections.append(Section(text=body, heading=" > ".join(headings)))
        lines.clear()

    for block in document.iter_inner_content():
        if isinstance(block, Table):
            for row in block.rows:
                cells = [cell.text.strip() for cell in row.cells]
                lines.append(" | ".join(cell for cell in cells if cell))
            continue
        level = _heading_level(block.style.name if block.style is not None else "")
        if level:
            flush()
            headings[:] = headings[: level - 1]
            headings.append(block.text.strip())
        elif block.text.strip():
            lines.append(block.text)
    flush()
    return sections


def _heading_level(style_name: str) -> int:
    if style_name == "Title":
        return 1
    match = re.fullmatch(r"Heading (\d)", style_name)
    return int(match.group(1)) if match else 0
