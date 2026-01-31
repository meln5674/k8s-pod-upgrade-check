# Kubernetes Pod Upgrade Check

This tool watches the Pods of a Kubernetes cluster and attempts to find pods that have "updates available",
defined as images with a higher semantic version available. These findings are communicated to the user
via events associated the pods themselves, so can be viewed using `kubectl describe pod <name>` or `kubectl get events`

```

# kubectl describe pod postgresql-1
Name:             postgresql-1
Namespace:        postgresql
...
Events:
  Type    Reason  Age        From                   Message
  ----    ------  ----       ----                   -------
  Normal          2m48s      k8s-pod-upgrade-check  New image version(s) available: ghcr.io/cloudnative-pg/postgresql:18.0-system-trixie -> 18.1-minimal-bookworm

# kubectl get events
LAST SEEN   TYPE     REASON   OBJECT             MESSAGE
5m47s       Normal            pod/postgresql-1   New image version(s) available: ghcr.io/cloudnative-pg/postgresql:18.0-system-trixie -> 18.1-minimal-bookworm
```

## Configuration and Credentials

This tool is built on the same shared library as Podman, Buildah, and Skopeo.
