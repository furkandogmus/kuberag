# Ingestion / evaluation / cleanup worker.
FROM python:3.14-slim

# git clones GitHub sources; gcc/g++ build native wheels (PyStemmer, etc.).
RUN apt-get update \
    && apt-get install -y --no-install-recommends git build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt \
    && apt-get purge -y build-essential && apt-get autoremove -y

COPY --chown=65532:65532 rag_worker/ ./rag_worker/

# Run as non-root, consistent with the pod security context.
USER 65532:65532

ENTRYPOINT ["python", "-m", "rag_worker"]
