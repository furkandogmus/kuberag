# Security Policy

## Status

kuberag is at `v1alpha1`. The API is not stable, the operator has not
been security-audited by a third party, and the dependency tree
includes a deliberately-pinned `setuptools==80.10.2` that carries
known CVEs (pinned because `pymilvus` breaks on 81+). **We do not
recommend running kuberag in production with sensitive data** until:

- the `v1` API lands,
- a third-party security review has been completed,
- the `setuptools` pin is removed (either by upgrading `pymilvus` to a
  version that supports 81+ or by adopting a `pymilvus` release that
  doesn't need the pin).

## Image integrity

All container images published to `ghcr.io` are signed with
[cosign](https://github.com/sigstore/cosign) keyless signing using the
GitHub Actions OIDC identity and the public
[Fulcio](https://fulcio.sigstore.dev/) CA. You can verify any image with:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/furkandogmus/kuberag/.github/workflows/release.yaml@refs/[^/]+/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/furkandogmus/kuberag:<tag>
```

Substitute `<tag>` with the desired version tag or `latest`.

## Supported versions

| Version | Supported | Notes |
|---------|-----------|-------|
| `main` (unreleased) | yes | New features land here first. Security fixes backported. |
| `v0.4.x` | yes | Current release line. |
| `v0.3.x` | best-effort | Critical CVEs only. |
| `<= v0.2.x` | no | EOL. Upgrade. |

We commit to supporting the latest minor release and the one before
it. Older releases receive critical security patches for 90 days
after a new minor is cut. Anything older is end-of-life.

## Reporting a vulnerability

**Please do not open a public GitHub issue for security problems.**

Use one of the following:

- **GitHub private vulnerability reporting**:
  <https://github.com/furkandogmus/kuberag/security/advisories/new>
- **Email**: `security@furkandogmus.dev` (PGP key fingerprint:
  `TODO: paste fingerprint once key is published`)

Include in the report:

1. A short description of the issue.
2. Steps to reproduce, or a proof-of-concept.
3. The component affected (operator, worker, retriever, specific CRD
   field, etc.).
4. The version(s) affected.
5. Any known mitigations.

We do **not** require a PGP-encrypted report, but if your report
includes sensitive details (live tokens, customer data) please
encrypt it.

## Disclosure timeline

We follow a 90-day responsible-disclosure model, modeled on
[Google's Project Zero](https://googleprojectzero.blogspot.com/p/vulnerability-disclosure-fair.html).

| Day | Event |
|-----|-------|
| 0  | Report received. We acknowledge within 3 business days. |
| 7  | Initial triage and severity classification. Reporter updated. |
| 14 | Confirmation of validity; severity assigned (Critical / High / Medium / Low). |
| 30 | Patch drafted in a private fork. |
| 60 | Patch merged to `main`. CVE assigned (if applicable). |
| 75 | Pre-release advisory draft shared with reporter for comment. |
| 90 | Public advisory + patch release. Earlier if mutually agreed. |

If a vulnerability is actively being exploited in the wild, the
timeline compresses: we ship a fix and the public advisory within
hours, not days, and disclose the 0-day at the time of patch
release. The reporter is credited unless they request otherwise.

## Severity classification

We use CVSS v3.1 base scores:

| Severity | CVSS range | Examples |
|----------|-----------|----------|
| Critical | 9.0 – 10.0 | Auth bypass on the retriever, RCE in the operator, secret exfiltration via CRD |
| High     | 7.0 – 8.9  | SSRF in the web crawler, privilege escalation across KBs, container breakout via NetworkPolicy gap |
| Medium   | 4.0 – 6.9  | DoS via malicious spec, log injection, missing rate limit |
| Low      | 0.1 – 3.9  | Information disclosure, missing security headers, verbose error pages |

## Known accepted risks

These are intentional, not vulnerabilities to report:

- **`setuptools==80.10.2` pin.** Required by `pymilvus 2.x`. Tracked
  under `pymilvus` upgrade in the Production Readiness section of
  `ROADMAP.md`. Each release re-runs `pip-audit`; CVEs are listed
  in the release notes.
- **Worker ServiceAccount sharing.** By default, all KBs in a
  namespace share a single `kuberag-worker` ServiceAccount. Per-KB
  SA isolation is in `ROADMAP.md` as "API maturity" work.
- **No retriever authentication.** The FastAPI server has no
  built-in auth. Users are expected to front it with an auth proxy
  (oauth2-proxy, ambassador, etc.) or restrict via NetworkPolicy.
  Tracked in `ROADMAP.md`.
- **No retriever TLS.** The Service is HTTP. cert-manager / Ingress
  with TLS is the deployment pattern. Tracked in `ROADMAP.md`.

## CVE history

| CVE | Severity | Affected | Fixed in | Notes |
|-----|----------|----------|----------|-------|
| _none yet_ |  |  |  | First public CVE will be added here at disclosure. |

## For operators

- Worker and retriever pods talk to your vector stores, embedding
  / LLM providers, and sources. **Scope the credentials in
  referenced `Secret`s to least privilege.** A token with
  `repo:read` is not the same as one with `repo:write`.
- The operator's `ClusterRole` (see `config/rbac/role.yaml`) is
  restricted to its own CRDs plus the Jobs, ConfigMaps,
  Deployments and Services it manages. **Review it before deploying
  to a shared cluster.** The cluster-scoped permissions
  (`secrets: get;list;watch`) are required for secret-rotation
  reconciliation; if you don't use that feature, you can
  remove the verbs.
- The web crawler validates that targets are public (RFC1918
  blocks are rejected unless `allowPrivateNetworks: true` is set
  on the source). DNS results are cached for 60s for performance,
  which is best-effort rebinding protection — **do not point the
  crawler at internal services on a hostile network.**
- NetworkPolicy default-deny is enabled
  (`config/rbac/network-policy.yaml`). Egress is whitelisted to
  DNS, the API server, vector stores, and external APIs. **If
  your environment adds new egress destinations (e.g. a private
  PyPI mirror, an internal LLM endpoint), update the policy.**
- Embedding API calls are made from worker pods. **If you use
  hosted providers (OpenAI, Gemini), egress is to those APIs over
  HTTPS; no private keys are exposed.**
- Never expose embedding/LLM backends (e.g. a local Ollama) on
  `0.0.0.0` on an untrusted network. Bind them to `127.0.0.1` and
  expose via the retriever's NetworkPolicy-controlled path.

## Security audit status

No third-party security audit has been performed. The code has
been reviewed by the maintainer and contributors for obvious
issues, but a paid audit (Trail of Bits, Cure53, NCC Group, etc.)
is required before we can recommend production use with
sensitive data. See `ROADMAP.md` for the "Production Readiness"
checklist.
