from rag.domain.chunks import Chunk, Section, chunk_sections, embedding_text


def test_short_sections_become_one_chunk_each_with_their_location():
    chunks = chunk_sections(
        [Section("Refunds take 14 days.", page=2), Section("Ship in 3 days.", heading="Shipping")]
    )

    assert chunks == [
        Chunk(0, "Refunds take 14 days.", 2, ""),
        Chunk(1, "Ship in 3 days.", 0, "Shipping"),
    ]


def test_chunks_never_cross_sections():
    chunks = chunk_sections([Section("a " * 50, page=1), Section("b " * 50, page=2)])

    assert [chunk.page for chunk in chunks] == [1, 2]
    assert "b" not in chunks[0].text


def test_long_text_is_split_within_the_limit_with_overlap():
    paragraphs = [f"Paragraph {i} " + "word " * 40 for i in range(12)]
    chunks = chunk_sections([Section("\n\n".join(paragraphs))], max_chars=500, overlap_chars=250)

    assert len(chunks) > 1
    assert all(len(chunk.text) <= 500 for chunk in chunks)
    # Each paragraph is about 210 characters, so the 250-character overlap repeats exactly one:
    # every chunk after the first starts with the paragraph the previous chunk ended with.
    for previous, current in zip(chunks, chunks[1:], strict=False):
        first_paragraph = "Paragraph " + current.text.split(" ")[1]
        assert first_paragraph in previous.text
    assert [chunk.index for chunk in chunks] == list(range(len(chunks)))


def test_a_paragraph_longer_than_the_limit_is_split_by_sentences_then_words():
    sentences = " ".join(f"Sentence number {i} is here." for i in range(60))
    one_huge_word = "x" * 1500
    chunks = chunk_sections([Section(sentences), Section(one_huge_word)], max_chars=300)

    assert all(len(chunk.text) <= 300 for chunk in chunks)
    assert "".join(chunk.text for chunk in chunks if chunk.text.startswith("x")) == one_huge_word


def test_whitespace_is_collapsed_and_empty_sections_skipped():
    chunks = chunk_sections([Section("  line one\n line   two  "), Section("   \n\n  ")])

    assert [chunk.text for chunk in chunks] == ["line one line two"]


def test_embedding_text_adds_title_and_heading_for_context():
    chunk = Chunk(0, "It takes 14 days.", 0, "Refunds > Timing")

    assert embedding_text("Support policy", chunk) == (
        "Support policy > Refunds > Timing\n\nIt takes 14 days."
    )
    assert embedding_text("", Chunk(0, "x", 0, "")) == "x"
