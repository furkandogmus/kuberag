"""Token-approximate chunking shared by ingestion and evaluation."""
from __future__ import annotations

import re

# We approximate tokens as words * 1.3 to avoid a tokenizer dependency in the
# hot path. The operator's chunking spec is expressed in tokens.
_TOKENS_PER_WORD = 1.3


def chunk_text(text: str, max_tokens: int, overlap: int, strategy: str) -> list[str]:
    if strategy == "semantic":
        blocks = _semantic_blocks(text)
    else:  # recursive | fixed -> sliding window over the whole doc
        blocks = [text]

    chunks: list[str] = []
    for block in blocks:
        chunks += _pack_words(block, max_tokens, overlap)
    return [c for c in chunks if c.strip()]


def _semantic_blocks(text: str) -> list[str]:
    # Split on markdown headings, then on blank-line paragraph boundaries.
    parts = re.split(r"(?=^#{1,6}\s)", text, flags=re.MULTILINE)
    blocks: list[str] = []
    for part in parts:
        blocks += [p for p in re.split(r"\n\s*\n", part) if p.strip()]
    return blocks


def _pack_words(text: str, max_tokens: int, overlap: int) -> list[str]:
    words = text.split()
    if not words:
        return []
    max_words = max(1, int(max_tokens / _TOKENS_PER_WORD))
    ov_words = max(0, int(overlap / _TOKENS_PER_WORD))
    step = max(1, max_words - ov_words)
    out: list[str] = []
    for i in range(0, len(words), step):
        out.append(" ".join(words[i : i + max_words]))
        if i + max_words >= len(words):
            break
    return out
