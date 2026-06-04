# Security Policy

kuberag is an early-stage (`v1alpha1`) project and is **not** recommended for
production use yet.

## Reporting a vulnerability

Please report security issues privately via GitHub's
[private vulnerability reporting](https://github.com/furkandogmus/kuberag/security/advisories/new)
rather than opening a public issue. I'll acknowledge within a few days.

## Notes for operators

- Worker and retriever pods talk to your vector stores, embedding/LLM providers,
  and sources. Scope the credentials you put in referenced `Secret`s to least
  privilege.
- The operator's `ClusterRole` is namespaced to its own resources plus the Jobs,
  ConfigMaps, Deployments and Services it manages; review `config/rbac/` before
  deploying to a shared cluster.
- Never expose embedding/LLM backends (e.g. a local Ollama) on `0.0.0.0` on an
  untrusted network.
