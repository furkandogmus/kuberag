---
name: Bug report
about: Something isn't working as expected
labels: bug
---

**What happened**
<!-- A clear description of the bug. -->

**Expected**
<!-- What you expected to happen. -->

**Repro**
<!-- Minimal KnowledgeBase/Retriever YAML + steps. -->

```yaml
# spec here
```

**Environment**
- kuberag image/tag:
- Kubernetes (e.g. k3d/kind/EKS) + version:
- Vector store / embedding provider:

**Logs**
<!-- Operator logs (`kubectl -n kuberag-system logs deploy/kuberag-controller-manager`)
     and the relevant worker Job pod logs. -->
