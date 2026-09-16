---
name: Bug report
about: Something does not work as documented
labels: bug
---

**What happened**

**What you expected**

**Versions**
- pelican-k8s chart / images:
- Kubernetes distribution and version:
- Pelican Panel version:
- Egg (image and name):

**Diagnostics**

```
kubectl -n pelican-servers describe gameserver gs-<uuid>
kubectl -n pelican-servers logs gs-<uuid>-0 -c agent
kubectl -n pelican-system logs deploy/pelican-k8s-gateway
kubectl -n pelican-system logs deploy/pelican-k8s-operator
```
