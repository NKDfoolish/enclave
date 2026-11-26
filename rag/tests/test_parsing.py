import io

import docx
import pytest

from rag.domain.chunks import Section
from rag.domain.documents import ParseError
from rag.infra.parsing import DOCX, MARKDOWN, PDF, parse_document
from tests.pdf import make_pdf


def test_pdf_gives_one_section_per_page_with_page_numbers():
    data = make_pdf(["Refunds are paid within 14 days.", "", "Shipping takes 3 days."])

    sections = parse_document(PDF, data)

    # The empty page 2 is skipped; page numbers stay true to the file.
    assert [(s.page, s.text.strip()) for s in sections] == [
        (1, "Refunds are paid within 14 days."),
        (3, "Shipping takes 3 days."),
    ]


def test_markdown_sections_follow_nested_headings():
    text = "# Support\nIntro.\n## Refunds\nWithin 14 days.\n### Exceptions\nNone on sale items.\n"
    text += "```\n# not a heading\n```\n## Shipping\nThree days.\n# Contact\nEmail us."

    sections = parse_document(MARKDOWN, text.encode())

    assert [s.heading for s in sections] == [
        "Support",
        "Support > Refunds",
        "Support > Refunds > Exceptions",
        "Support > Shipping",
        "Contact",
    ]
    assert "# not a heading" in sections[2].text


def test_docx_splits_at_headings_and_reads_tables():
    document = docx.Document()
    document.add_heading("Leave policy", level=1)
    document.add_paragraph("Staff get 12 days of paid leave.")
    document.add_heading("Sick leave", level=2)
    table = document.add_table(rows=2, cols=2)
    table.cell(0, 0).text, table.cell(0, 1).text = "Days", "Paid"
    table.cell(1, 0).text, table.cell(1, 1).text = "5", "Yes"
    buffer = io.BytesIO()
    document.save(buffer)

    sections = parse_document(DOCX, buffer.getvalue())

    assert sections == [
        Section("Staff get 12 days of paid leave.", heading="Leave policy"),
        Section("Days | Paid\n\n5 | Yes", heading="Leave policy > Sick leave"),
    ]


@pytest.mark.parametrize(
    ("content_type", "data"),
    [
        (PDF, b"%PDF-1.7 broken"),
        (DOCX, b"PK not really a zip"),
        (MARKDOWN, b"\xff\xfe"),
        ("x/y", b""),
    ],
)
def test_unreadable_files_raise_parse_error(content_type, data):
    with pytest.raises(ParseError):
        parse_document(content_type, data)
