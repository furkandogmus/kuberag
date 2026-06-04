"""Source backends: github, s3, web. Each yields documents plus a revision
marker the operator stores so unchanged sources can be skipped on the next run."""
from __future__ import annotations

import hashlib
import os
import re
import subprocess
import urllib.parse
from dataclasses import dataclass, field
from pathlib import Path

TEXT_EXTS = {".md", ".mdx", ".txt", ".rst", ".html", ".htm"}


@dataclass
class SourceDocs:
    revision: str
    # (doc_path, text) pairs. doc_path is stable and used for chunk ids.
    docs: list[tuple[str, str]] = field(default_factory=list)


def _hash(*parts: str) -> str:
    h = hashlib.sha256()
    for p in parts:
        h.update(p.encode())
        h.update(b"\0")
    return h.hexdigest()[:16]


def _glob_to_regex(glob: str) -> str:
    """Translate a gitignore-style glob to a regex with correct ** semantics.

    fnmatch is wrong for paths: its '*' matches '/' and it has no notion of '**'.
    Here: '**/' matches zero or more directories, '**' matches anything, '*'
    matches within a single segment, '?' a single non-separator char.
    """
    i, n, out = 0, len(glob), []
    while i < n:
        c = glob[i]
        if glob[i : i + 3] == "**/":
            out.append("(?:.*/)?")
            i += 3
        elif glob[i : i + 2] == "**":
            out.append(".*")
            i += 2
        elif c == "*":
            out.append("[^/]*")
            i += 1
        elif c == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(c))
            i += 1
    return "^" + "".join(out) + "$"


def _match_globs(rel: str, globs: list[str]) -> bool:
    if not globs:
        return Path(rel).suffix.lower() in TEXT_EXTS
    return any(re.match(_glob_to_regex(g), rel) for g in globs)


# --------------------------------------------------------------------------- #
# GitHub
# --------------------------------------------------------------------------- #
def github_revision(src: dict) -> str | None:
    """Cheap revision probe via `git ls-remote` (no clone)."""
    gh = src["github"]
    url = _github_url(gh, src["name"])
    ref = gh.get("ref") or "HEAD"
    try:
        out = subprocess.run(
            ["git", "ls-remote", url, ref],
            check=True, capture_output=True, text=True, timeout=60,
        ).stdout.strip()
    except subprocess.CalledProcessError:
        return None
    if not out:
        return None
    return out.split()[0]


def _github_url(gh: dict, source_name: str) -> str:
    repo = gh["repo"]
    token = os.environ.get(f"GITHUB_TOKEN_{source_name}")
    if token:
        return f"https://x-access-token:{token}@github.com/{repo}.git"
    return f"https://github.com/{repo}.git"


def fetch_github(src: dict, dest: Path) -> SourceDocs:
    gh = src["github"]
    url = _github_url(gh, src["name"])
    ref = gh.get("ref")
    globs = gh.get("includeGlobs") or []

    # Blobless + sparse clone: with includeGlobs we fetch only the matching
    # paths' blobs instead of the whole (potentially huge) repo. This turns a
    # multi-minute checkout of a 10k-file repo into a few seconds.
    cmd = ["git", "clone", "--depth", "1", "--filter=blob:none"]
    if globs:
        cmd += ["--no-checkout"]
    if ref:
        cmd += ["--branch", ref]
    cmd += [url, str(dest)]
    subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    if globs:
        # Non-cone sparse patterns are gitignore-style, matching our globs.
        subprocess.run(
            ["git", "-C", str(dest), "sparse-checkout", "set", "--no-cone", *globs],
            check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        subprocess.run(
            ["git", "-C", str(dest), "checkout"],
            check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )

    sha = subprocess.run(
        ["git", "-C", str(dest), "rev-parse", "HEAD"],
        check=True, capture_output=True, text=True,
    ).stdout.strip()

    globs = gh.get("includeGlobs") or []
    docs: list[tuple[str, str]] = []
    for p in sorted(dest.rglob("*")):
        if not p.is_file():
            continue
        rel = str(p.relative_to(dest))
        if not _match_globs(rel, globs):
            continue
        text = p.read_text(encoding="utf-8", errors="ignore")
        docs.append((f"{gh['repo']}/{rel}", text))
    return SourceDocs(revision=sha, docs=docs)


# --------------------------------------------------------------------------- #
# S3
# --------------------------------------------------------------------------- #
def _s3_client(s3: dict, source_name: str):
    import boto3
    from botocore.config import Config

    kwargs = {}
    if s3.get("region"):
        kwargs["region_name"] = s3["region"]
    if s3.get("endpoint"):
        kwargs["endpoint_url"] = s3["endpoint"]
        # S3-compatible stores (MinIO, etc.) need path-style addressing; the
        # default virtual-host style resolves bucket.host which doesn't exist.
        kwargs["config"] = Config(s3={"addressing_style": "path"})
    ak = os.environ.get(f"S3_ACCESS_KEY_{source_name}")
    sk = os.environ.get(f"S3_SECRET_KEY_{source_name}")
    if ak and sk:
        kwargs["aws_access_key_id"] = ak
        kwargs["aws_secret_access_key"] = sk
    return boto3.client("s3", **kwargs)


def _s3_list(src: dict):
    s3 = src["s3"]
    client = _s3_client(s3, src["name"])
    paginator = client.get_paginator("list_objects_v2")
    globs = s3.get("includeGlobs") or []
    prefix = s3.get("prefix", "")
    objs = []
    for page in paginator.paginate(Bucket=s3["bucket"], Prefix=prefix):
        for obj in page.get("Contents", []):
            key = obj["Key"]
            if key.endswith("/"):
                continue
            if not _match_globs(key, globs):
                continue
            objs.append((key, obj["ETag"], obj["Size"]))
    return client, sorted(objs)


def s3_revision(src: dict) -> str | None:
    try:
        _, objs = _s3_list(src)
    except Exception:
        return None
    return _hash(*[f"{k}:{e}:{s}" for k, e, s in objs])


def fetch_s3(src: dict, dest: Path) -> SourceDocs:
    s3 = src["s3"]
    client, objs = _s3_list(src)
    docs: list[tuple[str, str]] = []
    for key, _, _ in objs:
        body = client.get_object(Bucket=s3["bucket"], Key=key)["Body"].read()
        text = body.decode("utf-8", errors="ignore")
        docs.append((f"s3://{s3['bucket']}/{key}", text))
    return SourceDocs(revision=_hash(*[f"{k}:{e}:{s}" for k, e, s in objs]), docs=docs)


# --------------------------------------------------------------------------- #
# Web crawl
# --------------------------------------------------------------------------- #
_TAG_RE = re.compile(r"<[^>]+>")
_HREF_RE = re.compile(r'href=["\']([^"\'#]+)', re.IGNORECASE)


def _strip_html(html: str) -> str:
    html = re.sub(r"<(script|style)[^>]*>.*?</\1>", " ", html, flags=re.DOTALL | re.IGNORECASE)
    text = _TAG_RE.sub(" ", html)
    return re.sub(r"\s+", " ", text).strip()


def fetch_web(src: dict) -> SourceDocs:
    import requests

    web = src["web"]
    seeds = web["urls"]
    max_depth = web.get("maxDepth", 1)
    same_domain = web.get("sameDomainOnly", True)
    max_pages = web.get("maxPages", 200)

    seen: set[str] = set()
    queue: list[tuple[str, int]] = [(u, 0) for u in seeds]
    seed_domains = {urllib.parse.urlparse(u).netloc for u in seeds}
    docs: list[tuple[str, str]] = []

    while queue and len(docs) < max_pages:
        url, depth = queue.pop(0)
        if url in seen:
            continue
        seen.add(url)
        try:
            resp = requests.get(url, timeout=15, headers={"User-Agent": "kuberag/1.0"})
        except requests.RequestException:
            continue
        if resp.status_code != 200 or "text/html" not in resp.headers.get("content-type", ""):
            continue
        docs.append((url, _strip_html(resp.text)))
        if depth < max_depth:
            for href in _HREF_RE.findall(resp.text):
                nxt = urllib.parse.urljoin(url, href)
                if same_domain and urllib.parse.urlparse(nxt).netloc not in seed_domains:
                    continue
                if nxt.startswith("http") and nxt not in seen:
                    queue.append((nxt, depth + 1))

    revision = _hash(*[f"{u}:{_hash(t)}" for u, t in sorted(docs)])
    return SourceDocs(revision=revision, docs=docs)


# --------------------------------------------------------------------------- #
def probe_revision(src: dict) -> str | None:
    """Return a cheap revision marker, or None if the source must be fetched."""
    t = src["type"]
    if t == "github":
        return github_revision(src)
    if t == "s3":
        return s3_revision(src)
    return None  # web must be fetched to know its content hash


def fetch(src: dict, dest: Path) -> SourceDocs:
    t = src["type"]
    if t == "github":
        return fetch_github(src, dest)
    if t == "s3":
        return fetch_s3(src, dest)
    if t == "web":
        return fetch_web(src)
    raise ValueError(f"unsupported source type: {t}")
