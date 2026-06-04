"""Token-approximate chunking shared by ingestion and evaluation.

Three strategies, each genuinely distinct:

* ``semantic``  - split on Markdown headings and blank-line paragraphs, then
  word-pack within each structural block. Best for structured docs.
* ``recursive`` - recursively split on a separator hierarchy
  (paragraph -> line -> sentence -> word) so breaks land on the most natural
  boundary that keeps a chunk under ``maxTokens``, then greedily merge adjacent
  pieces (with overlap). Best for prose without reliable headings.
* ``fixed``     - a uniform sliding window of ``maxTokens`` over the whole
  document, ignoring all structure. Predictable, structure-blind chunks.
"""
from __future__ import annotations

import re

# We approximate tokens as words * 1.3 to avoid a tokenizer dependency in the
# hot path. The operator's chunking spec is expressed in tokens.
_TOKENS_PER_WORD = 1.3

# Separator hierarchy for recursive splitting, coarsest first.
_SEPARATORS = ["\n\n", "\n", ". ", " ", ""]


def chunk_text(text: str, max_tokens: int, overlap: int, strategy: str) -> list[str]:
    if strategy == "recursive":
        chunks = _recursive_split(text, max_tokens, overlap)
    elif strategy == "semantic":
        chunks = []
        for block in _semantic_blocks(text):
            chunks += _pack_words(block, max_tokens, overlap)
    else:  # fixed -> sliding window over the whole doc
        chunks = _pack_words(text, max_tokens, overlap)
    return [c for c in chunks if c.strip()]


def _max_words(max_tokens: int) -> int:
    return max(1, int(max_tokens / _TOKENS_PER_WORD))


def _overlap_words(overlap: int) -> int:
    return max(0, int(overlap / _TOKENS_PER_WORD))


# --- semantic -------------------------------------------------------------

def _semantic_blocks(text: str) -> list[str]:
    # Split on markdown headings, then on blank-line paragraph boundaries.
    parts = re.split(r"(?=^#{1,6}\s)", text, flags=re.MULTILINE)
    blocks: list[str] = []
    for part in parts:
        blocks += [p for p in re.split(r"\n\s*\n", part) if p.strip()]
    return blocks


# --- fixed ----------------------------------------------------------------

def _pack_words(text: str, max_tokens: int, overlap: int) -> list[str]:
    words = text.split()
    if not words:
        return []
    max_words = _max_words(max_tokens)
    ov_words = _overlap_words(overlap)
    step = max(1, max_words - ov_words)
    out: list[str] = []
    for i in range(0, len(words), step):
        out.append(" ".join(words[i : i + max_words]))
        if i + max_words >= len(words):
            break
    return out


# --- recursive ------------------------------------------------------------

def _recursive_split(text: str, max_tokens: int, overlap: int) -> list[str]:
    max_words = _max_words(max_tokens)
    pieces = _split_on_separators(text, _SEPARATORS, max_words)
    return _merge_pieces(pieces, max_words, _overlap_words(overlap))


def _split_on_separators(text: str, separators: list[str], max_words: int) -> list[str]:
    """Break text into atomic pieces each <= max_words, preferring coarse boundaries."""
    if not text.strip():
        return []
    if _wc(text) <= max_words:
        return [text.strip()]
    for i, sep in enumerate(separators):
        if sep == "":
            # Finest level: hard-split into word windows (no overlap; added on merge).
            words = text.split()
            return [
                " ".join(words[j : j + max_words])
                for j in range(0, len(words), max_words)
            ]
        if sep in text:
            rest = separators[i + 1 :]
            out: list[str] = []
            for part in text.split(sep):
                if not part.strip():
                    continue
                if _wc(part) <= max_words:
                    out.append(part.strip())
                else:
                    out += _split_on_separators(part, rest, max_words)
            return out
    return [text.strip()]


def _merge_pieces(pieces: list[str], max_words: int, ov_words: int) -> list[str]:
    """Greedily merge atomic pieces up to max_words, carrying word overlap forward."""
    chunks: list[str] = []
    cur: list[str] = []
    for piece in pieces:
        words = piece.split()
        if cur and len(cur) + len(words) > max_words:
            chunks.append(" ".join(cur))
            cur = cur[len(cur) - ov_words :] if ov_words else []
        cur += words
    if cur:
        chunks.append(" ".join(cur))
    return chunks


def _wc(text: str) -> int:
    return len(text.split())
