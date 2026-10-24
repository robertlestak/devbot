# devbot-chart

The Helm chart for [devbot](https://github.com/robertlestak/devbot), as an OCI artifact. It's published as `devbot-chart` because Docker Hub keeps a chart in the repository named after it, which as `devbot` would overwrite the image's tags.

```bash
helm install devbot oci://registry-1.docker.io/robertlestak/devbot-chart \
  --version X.Y.Z -n devbot --create-namespace -f values.yaml --wait=legacy
```

It needs Kubernetes 1.30+, KEDA 2.16+ and Helm 4. The chart's default images are `robertlestak/devbot` and `robertlestak/devbot-codex` at its appVersion; a role on the `claude` harness needs an image you build ([Agent images](https://github.com/robertlestak/devbot/blob/main/docs/operating.md#agent-images)). Start with the [quickstart](https://github.com/robertlestak/devbot/blob/main/docs/quickstart.md).
