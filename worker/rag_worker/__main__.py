"""Entrypoint: `python -m rag_worker <ingest|eval|cleanup|backup|restore>`."""
from __future__ import annotations

import sys

from . import backup, cleanup, evaluate, ingest, restore
from .common import init_tracing


def main() -> None:
    init_tracing()
    if len(sys.argv) < 2:
        sys.exit("usage: rag_worker <ingest|eval|cleanup|backup|restore>")
    cmd = sys.argv[1]
    if cmd == "ingest":
        ingest.run()
    elif cmd == "eval":
        evaluate.run()
    elif cmd == "cleanup":
        cleanup.run()
    elif cmd == "backup":
        backup.run()
    elif cmd == "restore":
        restore.run()
    else:
        sys.exit(f"unknown command: {cmd}")


if __name__ == "__main__":
    main()
