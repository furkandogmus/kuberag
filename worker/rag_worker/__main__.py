"""Entrypoint: `python -m rag_worker <ingest|eval|cleanup>`."""
from __future__ import annotations

import sys

from . import cleanup, evaluate, ingest


def main() -> None:
    if len(sys.argv) < 2:
        sys.exit("usage: rag_worker <ingest|eval|cleanup>")
    cmd = sys.argv[1]
    if cmd == "ingest":
        ingest.run()
    elif cmd == "eval":
        evaluate.run()
    elif cmd == "cleanup":
        cleanup.run()
    else:
        sys.exit(f"unknown command: {cmd}")


if __name__ == "__main__":
    main()
