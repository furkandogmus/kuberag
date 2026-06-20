"""FastAPI retrieval (and optional generation) server deployed by the Retriever.

Config comes entirely from env (set by the operator):
  VECTORSTORE_TYPE, VECTORSTORE_ENDPOINT, VECTORSTORE_COLLECTION, VECTORSTORE_CREDENTIAL
  EMBEDDING_MODEL, EMBEDDING_PROVIDER, EMBEDDING_BASE_URL, EMBEDDING_DIMENSION, EMBEDDING_API_KEY
  TOPK, SCORE_THRESHOLD, RERANK_ENABLED, RERANK_MODEL, RERANK_CANDIDATES
  HYBRID_DEFAULT, HYBRID_DENSE_PERCENT
  GEN_ENABLED, GEN_PROVIDER, GEN_MODEL, GEN_BASE_URL, GEN_API_KEY, GEN_MAX_TOKENS, GEN_SYSTEM_PROMPT
"""
from __future__ import annotations

import os
import time

from contextlib import asynccontextmanager
from typing import Literal
from fastapi import FastAPI, UploadFile, File, Form
from fastapi.responses import HTMLResponse
from pydantic import BaseModel, Field

from rag_worker.chunking import chunk_text

from rag_worker.embeddings import from_spec
from rag_worker.stores import make_store

@asynccontextmanager
async def lifespan(app: FastAPI):
    yield
    global _store
    if _store is not None:
        try:
            _store.close()
        except Exception:
            pass

app = FastAPI(title="kuberag-retriever", lifespan=lifespan)

_DEFAULT_TOPK = int(os.environ.get("TOPK", "8"))
_SCORE_THRESHOLD = int(os.environ.get("SCORE_THRESHOLD", "0")) / 100.0
_RERANK = os.environ.get("RERANK_ENABLED", "false").lower() == "true"
# Candidates retrieved before reranking; 0 => auto (max(4×topK, 20)).
_RERANK_CANDIDATES = int(os.environ.get("RERANK_CANDIDATES", "0") or 0)
# Default hybrid (vector + lexical) retrieval when a request doesn't specify.
_HYBRID_DEFAULT = os.environ.get("HYBRID_DEFAULT", "false").lower() == "true"
# Dense (vector) weight in hybrid RRF fusion; lexical gets the remainder.
_HYBRID_DENSE_W = int(os.environ.get("HYBRID_DENSE_PERCENT", "50") or 50) / 100.0
_GEN_ENABLED = os.environ.get("GEN_ENABLED", "false").lower() == "true"

# OpenAI-compatible chat base URLs per provider.
_GEN_BASE_URLS = {
    "openai": "https://api.openai.com/v1",
    "openrouter": "https://openrouter.ai/api/v1",
    "groq": "https://api.groq.com/openai/v1",
    "gemini": "https://generativelanguage.googleapis.com/v1beta/openai/",
}
_DEFAULT_SYSTEM_PROMPT = (
    "You are a helpful assistant. Answer the question using ONLY the provided "
    "context. If the context is insufficient, say so. Cite sources by their path."
)


def _spec_from_env() -> dict:
    return {
        "vectorStore": {
            "type": os.environ["VECTORSTORE_TYPE"],
            "endpoint": os.environ["VECTORSTORE_ENDPOINT"],
            "collection": os.environ.get("VECTORSTORE_COLLECTION", ""),
            "distance": os.environ.get("DISTANCE", "cosine"),
        }
    }


# Lazy singletons so the import (and probes) succeed before the store is ready.
_embedder = None
_store = None
_reranker = None
_gen_client = None


def _embedding_spec() -> dict:
    return {
        "model": os.environ["EMBEDDING_MODEL"],
        "provider": os.environ.get("EMBEDDING_PROVIDER", "local") or "local",
        "baseURL": os.environ.get("EMBEDDING_BASE_URL", ""),
        "dimension": int(os.environ.get("EMBEDDING_DIMENSION", "0") or 0),
        "queryPrefix": os.environ.get("EMBEDDING_QUERY_PREFIX", ""),
        "documentPrefix": os.environ.get("EMBEDDING_DOC_PREFIX", ""),
    }


def _ensure() -> None:
    global _embedder, _store, _reranker, _gen_client
    if _embedder is None:
        _embedder = from_spec(_embedding_spec())
    if _store is None:
        os.environ.setdefault("KB_NAME", os.environ.get("VECTORSTORE_COLLECTION", "kb"))
        _store = make_store(_spec_from_env())
    if _RERANK and _reranker is None:
        from fastembed.rerank.cross_encoder import TextCrossEncoder

        model = os.environ.get("RERANK_MODEL") or "bge-reranker-base"
        _reranker = TextCrossEncoder(model_name=model)
    if _GEN_ENABLED and _gen_client is None:
        from openai import OpenAI

        provider = os.environ.get("GEN_PROVIDER", "openai")
        base_url = os.environ.get("GEN_BASE_URL") or _GEN_BASE_URLS.get(provider)
        if not base_url:
            raise ValueError(f"generation provider {provider!r} requires GEN_BASE_URL")
        _gen_client = OpenAI(base_url=base_url, api_key=os.environ.get("GEN_API_KEY") or "no-key")


def _generate(
    question: str,
    chunks: list[Chunk],
    history: list[Message] | None = None,
    temperature: float | None = None,
    system_prompt: str | None = None,
    max_tokens: int | None = None,
) -> str:
    context = "\n\n".join(f"[{c.docPath}]\n{c.text}" for c in chunks)
    system = system_prompt or os.environ.get("GEN_SYSTEM_PROMPT") or _DEFAULT_SYSTEM_PROMPT
    messages = [{"role": "system", "content": system}]
    if history:
        for msg in history:
            messages.append({"role": msg.role, "content": msg.content})
    messages.append({"role": "user", "content": f"Context:\n{context}\n\nQuestion: {question}"})
    
    kwargs = {
        "model": os.environ["GEN_MODEL"],
        "max_tokens": max_tokens or int(os.environ.get("GEN_MAX_TOKENS", "512")),
        "messages": messages,
    }
    if temperature is not None:
        kwargs["temperature"] = temperature

    resp = _gen_client.chat.completions.create(**kwargs)
    return resp.choices[0].message.content or ""


def rrf(
    vector_hits: list[dict],
    text_hits: list[dict],
    k: int = 60,
    dense_weight: float = 1.0,
    text_weight: float = 1.0,
) -> list[dict]:
    scores = {}
    payloads = {}

    def make_key(h):
        p = h["payload"]
        return (p.get("source", ""), p.get("doc_path", ""), p.get("text", ""))

    for rank, h in enumerate(vector_hits):
        key = make_key(h)
        scores[key] = scores.get(key, 0.0) + dense_weight / (k + rank + 1)
        payloads[key] = h["payload"]

    for rank, h in enumerate(text_hits):
        key = make_key(h)
        scores[key] = scores.get(key, 0.0) + text_weight / (k + rank + 1)
        payloads[key] = h["payload"]
        
    fused = []
    for key, score in scores.items():
        fused.append({
            "score": score,
            "payload": payloads[key]
        })
    fused.sort(key=lambda x: x["score"], reverse=True)
    return fused


class Message(BaseModel):
    role: Literal["user", "assistant"]
    content: str = Field(min_length=1)


class QueryRequest(BaseModel):
    query: str = Field(min_length=1)
    topK: int | None = Field(default=None, ge=1, le=100)
    source: str | None = Field(default=None, min_length=1)
    history: list[Message] | None = None
    docPath: str | None = Field(default=None, min_length=1)
    docPathPrefix: str | None = Field(default=None, min_length=1)
    hybrid: bool | None = None
    # Per-request retrieval-tuning overrides (fall back to the Retriever's spec
    # defaults when omitted). They let the playground experiment live without a
    # redeploy.
    hybridDensePercent: int | None = Field(default=None, ge=0, le=100)
    scoreThresholdPercent: int | None = Field(default=None, ge=0, le=100)
    rerank: bool | None = None
    temperature: float | None = Field(default=None, ge=0, le=2)
    systemPrompt: str | None = Field(default=None, min_length=1)
    maxTokens: int | None = Field(default=None, ge=1, le=8192)


class Chunk(BaseModel):
    text: str
    source: str
    docPath: str
    score: float


class QueryMeta(BaseModel):
    """Diagnostics describing how a query was retrieved — surfaced so callers (the
    playground) can see the effect of the tuning knobs they set."""
    topK: int
    hybrid: bool
    hybridDensePercent: int | None = None
    scoreThresholdPercent: int
    reranked: bool
    candidates: int
    returned: int
    tookMillis: int


class QueryResponse(BaseModel):
    query: str
    results: list[Chunk]
    answer: str | None = None
    generationError: str | None = None
    meta: QueryMeta | None = None


@app.get("/", response_class=HTMLResponse)
def playground() -> HTMLResponse:
    html_path = os.path.join(os.path.dirname(__file__), "playground.html")
    with open(html_path, "r", encoding="utf-8") as f:
        return HTMLResponse(content=f.read())


@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok"}


@app.post("/query", response_model=QueryResponse)
def query(req: QueryRequest) -> QueryResponse:
    _ensure()
    t0 = time.perf_counter()
    topk = req.topK or _DEFAULT_TOPK
    use_hybrid = req.hybrid if req.hybrid is not None else _HYBRID_DEFAULT
    # Per-request overrides fall back to the server (spec) defaults.
    dense_w = req.hybridDensePercent / 100.0 if req.hybridDensePercent is not None else _HYBRID_DENSE_W
    threshold = req.scoreThresholdPercent / 100.0 if req.scoreThresholdPercent is not None else _SCORE_THRESHOLD
    # Reranking can only be turned *off* per request (the model is loaded at
    # startup); a request can't enable it on a Retriever that didn't opt in.
    use_rerank = _RERANK and (req.rerank if req.rerank is not None else True)

    if use_rerank:
        # Give the reranker a deeper candidate pool, then return the top `topk`.
        fetch_k = _RERANK_CANDIDATES if _RERANK_CANDIDATES > 0 else max(topk * 4, 20)
    elif use_hybrid:
        fetch_k = max(topk * 3, 20)
    else:
        fetch_k = topk
    fetch_k = max(fetch_k, topk)

    if use_hybrid:
        qv = _embedder.embed_query(req.query)
        vector_hits = _store.search(
            qv, fetch_k, source=req.source, doc_path=req.docPath, doc_path_prefix=req.docPathPrefix
        )
        text_hits = _store.search_text(
            req.query, fetch_k, source=req.source, doc_path=req.docPath, doc_path_prefix=req.docPathPrefix
        )
        hits = rrf(
            vector_hits, text_hits,
            dense_weight=dense_w, text_weight=1.0 - dense_w,
        )
    else:
        qv = _embedder.embed_query(req.query)
        hits = _store.search(
            qv, fetch_k, source=req.source, doc_path=req.docPath, doc_path_prefix=req.docPathPrefix
        )

    reranked = bool(use_rerank and hits)
    if reranked:
        scores = list(_reranker.rerank(req.query, [h["payload"].get("text", "") for h in hits]))
        for h, s in zip(hits, scores):
            h["score"] = float(s)
        hits.sort(key=lambda h: h["score"], reverse=True)

    results = []
    for h in hits[:topk]:
        # RRF fusion scores are rank-derived, not similarities, so the similarity
        # threshold only applies to raw vector/rerank scores.
        is_rrf_score = use_hybrid and not use_rerank
        if not is_rrf_score and h["score"] < threshold:
            continue
        p = h["payload"]
        results.append(Chunk(
            text=p.get("text", ""),
            source=p.get("source", ""),
            docPath=p.get("doc_path", ""),
            score=round(float(h["score"]), 4),
        ))

    meta = QueryMeta(
        topK=topk,
        hybrid=use_hybrid,
        hybridDensePercent=round(dense_w * 100) if use_hybrid else None,
        scoreThresholdPercent=round(threshold * 100),
        reranked=reranked,
        candidates=fetch_k,
        returned=len(results),
        tookMillis=round((time.perf_counter() - t0) * 1000),
    )

    answer = None
    genErr = None
    if _GEN_ENABLED and results:
        try:
            answer = _generate(
                req.query,
                results,
                req.history,
                temperature=req.temperature,
                system_prompt=req.systemPrompt,
                max_tokens=req.maxTokens,
            )
        except Exception as e:
            genErr = f"{type(e).__name__}: {e}"
    return QueryResponse(query=req.query, results=results, answer=answer, generationError=genErr, meta=meta)


# Ingest endpoints (Dev/Playground mode)
DISABLE_PLAYGROUND_INGEST = os.environ.get("DISABLE_PLAYGROUND_INGEST", "").lower() == "true"

class UrlIngestRequest(BaseModel):
    url: str = Field(min_length=1)
    source: str = Field(default="playground", min_length=1)
    strategy: str = Field(default="semantic", min_length=1)
    maxTokens: int = Field(default=800, ge=1)
    overlap: int = Field(default=80, ge=0)


@app.post("/ingest/file")
def ingest_file(
    file: UploadFile = File(...),
    source: str = Form("playground"),
    strategy: str = Form("semantic"),
    maxTokens: int = Form(800),
    overlap: int = Form(80),
) -> dict:
    if DISABLE_PLAYGROUND_INGEST:
        return {"status": "error", "message": "Playground ingest is disabled in production."}
    _ensure()
    content = file.file.read()
    filename = file.filename
    if filename.lower().endswith(".pdf"):
        from rag_worker.sources import _read_pdf_bytes
        text = _read_pdf_bytes(content)
    else:
        text = content.decode("utf-8", errors="ignore")

    chunks = chunk_text(text, maxTokens, overlap, strategy)
    if not chunks:
        return {"status": "ok", "message": "No text extracted from file", "chunks": 0}

    points = []
    import hashlib
    import uuid
    for i, chunk in enumerate(chunks):
        vector = _embedder.embed_query(chunk)
        chunk_hash = hashlib.sha256(chunk.encode()).hexdigest()[:16]
        points.append({
            "id": str(uuid.uuid4()),
            "vector": vector,
            "payload": {
                "source": source,
                "doc_path": filename,
                "text": chunk,
                "chunk_hash": chunk_hash
            }
        })
    _store.upsert(points)
    return {"status": "ok", "message": f"Successfully ingested {len(points)} chunks from file {filename}", "chunks": len(points), "filename": filename}


@app.post("/ingest/url")
def ingest_url(req: UrlIngestRequest) -> dict:
    if DISABLE_PLAYGROUND_INGEST:
        return {"status": "error", "message": "Playground ingest is disabled in production."}
    _ensure()
    import requests
    from rag_worker.sources import _strip_html
    import hashlib
    import uuid

    try:
        resp = requests.get(req.url, timeout=15, headers={"User-Agent": "kuberag-playground/1.0"})
        resp.raise_for_status()
    except Exception as e:
        return {"status": "error", "message": f"Failed to fetch URL: {e}"}

    text = _strip_html(resp.text)
    chunks = chunk_text(text, req.maxTokens, req.overlap, req.strategy)
    if not chunks:
        return {"status": "ok", "message": "No text extracted from URL", "chunks": 0}

    points = []
    for i, chunk in enumerate(chunks):
        vector = _embedder.embed_query(chunk)
        chunk_hash = hashlib.sha256(chunk.encode()).hexdigest()[:16]
        points.append({
            "id": str(uuid.uuid4()),
            "vector": vector,
            "payload": {
                "source": req.source,
                "doc_path": req.url,
                "text": chunk,
                "chunk_hash": chunk_hash
            }
        })
    _store.upsert(points)
    return {"status": "ok", "message": f"Successfully ingested {len(points)} chunks from URL {req.url}", "chunks": len(points), "url": req.url}

