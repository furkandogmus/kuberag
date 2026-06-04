"""Teardown: drop the vector store collection when a KnowledgeBase is deleted."""
from __future__ import annotations

from .common import load_spec, log
from .stores import make_store


def run() -> None:
    spec = load_spec()
    store = make_store(spec)
    store.drop()
    log("dropped vector store collection")
