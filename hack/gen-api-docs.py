#!/usr/bin/env python3
"""Generate docs/API.md from the rendered CRD YAML files.

Reads config/crd/*.yaml (the output of `make manifests`) and writes
docs/API.md with a per-field reference. This is a more robust
alternative to gen-crd-api-reference-docs (which segfaults on
Go 1.26 due to old gengo).

Run from the repo root:  python3 hack/gen-api-docs.py
Or:                     make api-docs
"""
from __future__ import annotations

import os
import sys
import yaml
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
CRD_DIR = REPO / "config" / "crd"
OUT = REPO / "docs" / "API.md"

# Group by name, then by version (v1alpha1 only today).
CRD_ORDER = [
    ("knowledgebases", "KnowledgeBase", "kb"),
    ("retrievers", "Retriever", "rtr"),
    ("vectorindices", "VectorIndex", "vi"),
    ("ingestionruns", "IngestionRun", "ir"),
]


def load_crd(name: str) -> dict:
    path = CRD_DIR / f"rag.furkan.dev_{name}.yaml"
    with path.open() as f:
        return yaml.safe_load(f)


def render_type(t: dict, depth: int = 0) -> str:
    """Return a human-readable type name."""
    if not t:
        return "any"
    if "type" in t and not (t.get("properties") or t.get("items") or t.get("additionalProperties")):
        if t["type"] == "array" and "items" in t:
            return f"array({render_type(t['items'])})"
        if t["type"] == "object" and "additionalProperties" in t:
            return f"map({render_type(t['additionalProperties'])})"
        return t["type"]
    if "properties" in t:
        return "object"
    if "items" in t:
        return f"array({render_type(t['items'])})"
    return t.get("type", "any")


def default_value(p: dict) -> str:
    if "default" in p:
        return repr(p["default"])
    return "—"


def required(p: dict) -> bool:
    return bool(p.get("required") or []) or p.get("required") is True


def field_kind(p: dict) -> str:
    if p.get("type") == "array":
        return "list"
    if p.get("type") == "object" and p.get("properties"):
        return "object"
    if p.get("additionalProperties"):
        return "map"
    return "scalar"


def collect_paths(props: dict, prefix: str = "", out: list[tuple[str, dict]] | None = None) -> list[tuple[str, dict]]:
    """Walk properties; emit (path, schema) for every leaf and nested object."""
    if out is None:
        out = []
    for name, p in props.items():
        path = f"{prefix}.{name}" if prefix else name
        kind = field_kind(p)
        if kind == "object":
            out.append((path, p))
            if p.get("properties"):
                collect_paths(p["properties"], path, out)
        elif kind == "array" and p.get("items", {}).get("properties"):
            out.append((path, p))
            collect_paths(p["items"]["properties"], path + "[]", out)
        elif kind == "map":
            out.append((path, p))
        else:
            out.append((path, p))
    return out


def render_crd(name: str, crd: dict, short: str) -> str:
    spec = crd["spec"]
    group = spec["group"]  # rag.furkan.dev
    kind = spec["names"]["kind"]
    version = spec["versions"][0]["name"]  # v1alpha1

    out: list[str] = []
    plural = spec["names"]["plural"]
    singular = spec["names"]["singular"]
    out.append(f"## {kind} (`{short}`)\n")
    out.append(
        f"Group `{group}` · version `{version}` · "
        f"`{singular}` / `{plural}` / `{short}`\n"
    )
    versions = spec.get("versions", [])
    if versions:
        v0 = versions[0]
        if v0.get("subresources", {}).get("status"):
            out.append("Status is a subresource (`/status`).\n")
        if v0.get("subresources", {}).get("scale"):
            out.append("Scale subresource exposed (`/scale`).\n")
        if v0.get("additionalPrinterColumns"):
            cols = v0["additionalPrinterColumns"]
            out.append("**`kubectl get` columns:**\n")
            out.append("| Name | Type | Description |")
            out.append("|------|------|-------------|")
            for c in cols:
                out.append(
                    f"| `{c['name']}` | `{c['type']}` | {c.get('description', '')} |"
                )
            out.append("")

    schema = versions[0]["schema"]["openAPIV3Schema"]
    props = schema.get("properties", {})
    for section in ("spec", "status"):
        if section not in props:
            continue
        s = props[section]
        s_props = s.get("properties", {})
        required_top = s.get("required", [])
        out.append(f"### `{section}`\n")
        if required_top:
            out.append(f"Required top-level: `{', '.join(required_top)}`\n")
        if not s_props:
            continue
        for path, p in collect_paths(s_props):
            t = render_type(p)
            d = default_value(p)
            desc = p.get("description", "").strip().replace("\n", " ")
            enum = ""
            if p.get("enum"):
                enum = f" · enum: `{', '.join(map(str, p['enum']))}`"
            constraints = []
            for k in ("minimum", "maximum", "minLength", "maxLength", "pattern", "minItems", "maxItems", "format"):
                if k in p:
                    constraints.append(f"`{k}={p[k]}`")
            extras = (" · " + ", ".join(constraints)) if constraints else ""
            optional = "" if required_top and path.split(".")[0] in required_top and len(path.split(".")) == 1 else " (optional)"
            if desc:
                out.append(f"- **`{path}`** `{t}`{optional} — {desc}{enum}{extras}")
            else:
                out.append(f"- **`{path}`** `{t}`{optional}{enum}{extras}")
        out.append("")
    return "\n".join(out)


def main() -> int:
    OUT.parent.mkdir(parents=True, exist_ok=True)
    parts: list[str] = []
    parts.append(
        """<!--
  This file is auto-generated by hack/gen-api-docs.py. Do not edit
  by hand. To regenerate: make api-docs.
  Source: config/crd/rag.furkan.dev_*.yaml (rendered from api/v1alpha1).
-->

# API Reference

API group `rag.furkan.dev`, version `v1alpha1`. Short names: `kb`,
`rtr`, `vi`, `ir`.

> Looking for *which value to pick* rather than the full field list?
> See the [Configuration & tuning guide](TUNING.md).

"""
    )

    for plural, kind, short in CRD_ORDER:
        path = CRD_DIR / f"rag.furkan.dev_{plural}.yaml"
        if not path.exists():
            print(f"skip: {path} not found (run `make manifests` first)", file=sys.stderr)
            continue
        crd = load_crd(plural)
        parts.append(render_crd(plural, crd, short))

    parts.append("---")
    parts.append("")
    parts.append(
        "Generated from `config/crd/*.yaml`. To regenerate after "
        "changing `api/v1alpha1/*_types.go`: `make manifests api-docs`."
    )

    OUT.write_text("\n".join(parts) + "\n")
    print(f"Wrote {OUT}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
