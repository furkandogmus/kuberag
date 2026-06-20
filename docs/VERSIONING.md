# Versioning & Deprecation Policy

This document defines how kuberag versions its API, the operator, the
worker, and the retriever, and what guarantees users get when we
deprecate something.

## Components

kuberag has four independently versioned artifacts:

| Component | Identifier | Versioning |
|-----------|-----------|------------|
| API | `apiVersion: rag.furkan.dev/v1alpha1` | Kubernetes API versioning (see below) |
| Operator binary | Image `kuberag` | [SemVer 2.0.0](https://semver.org) |
| Worker image | `kuberag-worker` | SemVer 2.0.0 |
| Retriever image | `kuberag-retriever` | SemVer 2.0.0 |
| Helm chart | `deploy/helm/kuberag/` | [Helm chart SemVer](https://helm.sh/docs/chart_best_practices/conventions/#version-numbers) |

The four artifacts share a release tag (`v0.X.Y`) but are
independently built and can be mixed. The Helm chart's
`appVersion` field pins the image tags it deploys.

## Release cadence

- **Minor releases** (`v0.X.0`): roughly monthly. Allowed to add
  fields, add sources/stores, change defaults (with a note in
  the changelog).
- **Patch releases** (`v0.X.Y`): as needed for bugfixes and
  security patches. Allowed to fix behavior, never to break the
  CRD shape.
- **Major releases** (`vX.0.0`): tied to API stabilization
  (`v1alpha1 → v1beta1 → v1`). Contain a conversion webhook.

## API versioning

We follow Kubernetes API conventions, not SemVer for the CRD group.

### Group stability

The group is `rag.furkan.dev` for `v1alpha1` and is **not** promised
to stay under that domain. The first `v1` release will either keep
the same group (and the maintainer will need to retain control of
`furkandogmus.dev` for the API group) or move to a project-owned
domain (e.g. `rag.kuberag.io`). See the Production Readiness
section of `ROADMAP.md`.

### Version skew policy

- `v1alpha1` (current): no compatibility guarantee. Field additions
  and renames are allowed at any time. Defaults can change.
- `v1beta1` (planned, no release date): the schema is frozen; only
  additive changes (new fields, new enum values) allowed.
  Defaults can change only with a `DeprecationWarning` in release
  notes. Removal of any field requires a `v1beta1 → v1` conversion
  webhook.
- `v1` (planned): the schema is stable. Additive changes still
  allowed; removals only via the API deprecation policy below.

### Conversion between versions

We use [Kubernetes conversion webhooks](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/#webhook-conversion)
to convert stored objects between API versions. Today this is
absent (no version besides `v1alpha1` exists). It will be required
before any `v1beta1` or `v1` release.

Conversion must be **lossless across versions**: a `v1beta1` object
converted to `v1` and back must produce a byte-identical object.

## Deprecation policy

When a field, value, or behavior is deprecated, it follows the
[Kubernetes API deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/)
(documented for the Kubernetes API, applicable here as well):

| Deprecation type | First deprecated in | Removed in | Min stable period |
|------------------|---------------------|------------|-------------------|
| Field (alpha / `v1alpha1`) | any minor | next major | none |
| Field (`v1beta1`) | minor | after 9 months OR 3 releases, whichever is longer | 9 months |
| Field (`v1`) | minor | after 12 months OR 3 releases, whichever is longer | 12 months |
| Enum value | minor | after 6 months OR 2 releases, whichever is longer | 6 months |
| Default value change | minor | immediately on next minor + changelog note | 1 release |
| Behavior change (non-default) | minor | next major | 1 release |
| Image removal | minor | 12 months after deprecation notice | 12 months |
| Helm chart removal | minor | 6 months after deprecation notice | 6 months |

For a field deprecation, the controller MUST:

1. **Mark the field** with `// Deprecated:` in the Go type and a
   `+kubebuilder:deprecated` marker.
2. **Emit a `Deprecated` event** on the CR the first time the
   controller observes the deprecated field.
3. **Emit a Prometheus counter** `deprecated_field_usage_total{field="..."}`
   for monitoring.
4. **Log a warning** every Nth reconcile (not every time, to avoid
   log spam).
5. **Document in `CHANGELOG.md`** under a `Deprecated` section.

## Image and chart support matrix

When a release is cut, the following are simultaneously supported:

| Release | CRD | Operator | Worker | Retriever | Helm |
|---------|-----|----------|--------|-----------|------|
| `v0.4.0` | `v1alpha1` | yes | yes | yes | yes |
| `v0.3.x` | `v1alpha1` | yes | yes | yes | best-effort |
| `<= v0.2.x` | `v1alpha1` | no | no | no | no |

We support the **current minor + previous minor**. Older minors
receive critical security patches for 90 days. See
`SECURITY.md` for the full supported-versions table.

## Backwards-compatibility guarantees

For SemVer releases (`v0.X.Y`), we commit to:

- **Operators, workers, retrievers, and the chart in the same
  release** are tested against each other. A `v0.4.0` operator
  with a `v0.4.0` chart is the only combination the maintainer
  tests in CI.
- **Workers and retrievers in the same minor** (e.g. operator
  `v0.4.0` + worker `v0.4.1` + retriever `v0.4.0`) are tested
  together and supported.
- **Cross-minor combinations** (e.g. operator `v0.4.0` + worker
  `v0.3.5`) may work but are not tested. We document known
  incompatibilities in the `CHANGELOG.md` if they exist.
- **The CRD shape within a minor is additive-only** after the
  first release of that minor. We will not rename or remove a
  field without bumping to a new minor and following the
  deprecation policy above.

## Breaking-change procedure

A breaking change in any component goes through:

1. **Issue** with the `breaking-change` label, linking the
   motivation.
2. **PR** with a deprecation plan (timeline, migration code,
   backward-compat shim).
3. **Review** by at least two maintainers. The PR must include a
   `Migration:` section in its description with concrete
   upgrade / downgrade steps.
4. **Release** in a new minor (or major for the CRD), with a
   `CHANGELOG.md` `BREAKING:` section.
5. **Changelog callout** in the GitHub release, linking to the
   upgrade guide.

## When `v1` ships

The `v1` release freezes the API. After `v1`:

- The `v1beta1 → v1` conversion webhook is mandatory.
- The `v1alpha1` API is **not** supported in `v1.X` releases.
  Users have 12 months after `v1` ships to migrate.
- Field additions are still allowed (additive changes).
- Field removals go through the full deprecation policy.

See `ROADMAP.md` for the current `v1` target timeline.
