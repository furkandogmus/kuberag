"""Source backends: github, s3, web. Each yields documents plus a revision
marker the operator stores so unchanged sources can be skipped on the next run."""
from __future__ import annotations

import hashlib
import os
import re
import subprocess
import urllib.parse
from dataclasses import dataclass, field
from html.parser import HTMLParser
from pathlib import Path

TEXT_EXTS = {".md", ".mdx", ".txt", ".rst", ".html", ".htm", ".pdf"}


def _read_pdf(path: Path) -> str:
    from pypdf import PdfReader
    try:
        reader = PdfReader(path)
        return "\n".join(page.extract_text() for page in reader.pages if page.extract_text())
    except Exception as e:
        print(f"Error parsing PDF file {path}: {e}")
        return ""


def _read_pdf_bytes(body: bytes) -> str:
    import io
    from pypdf import PdfReader
    try:
        reader = PdfReader(io.BytesIO(body))
        return "\n".join(page.extract_text() for page in reader.pages if page.extract_text())
    except Exception as e:
        print(f"Error parsing PDF bytes: {e}")
        return ""



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
        if p.suffix.lower() == ".pdf":
            text = _read_pdf(p)
        else:
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
        if key.lower().endswith(".pdf"):
            text = _read_pdf_bytes(body)
        else:
            text = body.decode("utf-8", errors="ignore")
        docs.append((f"s3://{s3['bucket']}/{key}", text))
    return SourceDocs(revision=_hash(*[f"{k}:{e}:{s}" for k, e, s in objs]), docs=docs)


# --------------------------------------------------------------------------- #
# Web crawl
# --------------------------------------------------------------------------- #
class _HTMLPageParser(HTMLParser):
    """Extract visible text and crawlable links without an HTML dependency."""

    _SKIP_TAGS = {"script", "style", "noscript", "template"}

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.text_parts: list[str] = []
        self.links: list[str] = []
        self.base_href: str | None = None
        self._skip_depth = 0

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        tag = tag.lower()
        if tag in self._SKIP_TAGS:
            self._skip_depth += 1
            return
        values = dict(attrs)
        if tag == "a" and values.get("href"):
            self.links.append(values["href"])
        elif tag == "base" and self.base_href is None and values.get("href"):
            self.base_href = values["href"]

    def handle_endtag(self, tag: str) -> None:
        if tag.lower() in self._SKIP_TAGS and self._skip_depth > 0:
            self._skip_depth -= 1

    def handle_data(self, data: str) -> None:
        if self._skip_depth == 0 and data.strip():
            self.text_parts.append(data)


def _parse_html(html: str) -> tuple[str, list[str], str | None]:
    parser = _HTMLPageParser()
    parser.feed(html)
    parser.close()
    text = re.sub(r"\s+", " ", " ".join(parser.text_parts)).strip()
    return text, parser.links, parser.base_href


def _strip_html(html: str) -> str:
    text, _, _ = _parse_html(html)
    return text


def _normalize_web_url(url: str) -> str | None:
    """Return a stable HTTP(S) URL without fragments or userinfo."""
    try:
        parsed = urllib.parse.urlsplit(url)
        scheme = parsed.scheme.lower()
        hostname = (parsed.hostname or "").lower().rstrip(".")
        port = parsed.port
    except ValueError:
        return None
    if scheme not in {"http", "https"} or not hostname:
        return None

    display_host = f"[{hostname}]" if ":" in hostname else hostname
    default_port = (scheme == "http" and port == 80) or (scheme == "https" and port == 443)
    netloc = display_host if port is None or default_port else f"{display_host}:{port}"
    return urllib.parse.urlunsplit((scheme, netloc, parsed.path or "/", parsed.query, ""))


def _web_hostname(url: str) -> str:
    return (urllib.parse.urlsplit(url).hostname or "").lower().rstrip(".")


def fetch_web(src: dict) -> SourceDocs:
    import requests

    web = src["web"]
    seeds = web["urls"]
    max_depth = web.get("maxDepth", 1)
    same_domain = web.get("sameDomainOnly", True)
    max_pages = web.get("maxPages", 200)

    normalized_seeds = [url for seed in seeds if (url := _normalize_web_url(seed)) is not None]
    seen: set[str] = set()
    queued: set[str] = set(normalized_seeds)
    indexed: set[str] = set()
    queue: list[tuple[str, int]] = [(url, 0) for url in normalized_seeds]
    seed_domains = {_web_hostname(url) for url in normalized_seeds}
    docs: list[tuple[str, str]] = []
    fetched_pages = 0

    while queue and fetched_pages < max_pages:
        url, depth = queue.pop(0)
        queued.discard(url)
        if url in seen:
            continue
        seen.add(url)
        fetched_pages += 1
        try:
            resp = requests.get(url, timeout=15, headers={"User-Agent": "kuberag/1.0"})
        except requests.RequestException:
            continue
        if resp.status_code != 200 or "text/html" not in resp.headers.get("content-type", ""):
            continue

        final_url = _normalize_web_url(resp.url)
        if final_url is None:
            continue
        if same_domain and _web_hostname(final_url) not in seed_domains:
            continue
        if final_url in indexed:
            continue

        text, links, base_href = _parse_html(resp.text)
        if not text:
            continue
        docs.append((final_url, text))
        indexed.add(final_url)

        if depth < max_depth:
            link_base = urllib.parse.urljoin(final_url, base_href) if base_href else final_url
            for href in links:
                nxt = _normalize_web_url(urllib.parse.urljoin(link_base, href))
                if nxt is None:
                    continue
                if same_domain and _web_hostname(nxt) not in seed_domains:
                    continue
                if nxt not in seen and nxt not in queued:
                    queue.append((nxt, depth + 1))
                    queued.add(nxt)

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
