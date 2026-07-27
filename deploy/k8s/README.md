# Kubernetes deploy

Plain manifests, no Helm — apply in order:

```sh
kubectl apply -f namespace.yaml
kubectl apply -f secret.example.yaml   # copy and edit first; do NOT commit real secrets
kubectl apply -f configmap.yaml
kubectl apply -f postgres.yaml         # dev only; use managed Postgres in production
kubectl apply -f migrate-job.yaml      # wait for completion before the next two
kubectl apply -f api.yaml
kubectl apply -f worker.yaml
kubectl apply -f ingress.yaml
```

## Notes

**`postgres.yaml` is for development.** It is a single-replica StatefulSet with a
PVC — no backups, no failover, no connection pooling. Production should point
`VHW_DATABASE_URL` at a managed instance and skip this file entirely.

**Run `migrate-job.yaml` before rolling api/worker.** The Job applies pending
migrations and exits; the Deployments do not migrate on startup, so a rollout
against an unmigrated schema fails fast rather than half-working. On upgrades,
apply the Job with a new `metadata.name` (or delete the old one) since a
completed Job is immutable.

**The worker image needs the `varhub` CLI.** The image built from
`deploy/compose/Dockerfile` deliberately does not include it — see the comment
there. Until varianthub-cli publishes a release binary, either bake it into a
derived image or mount it from an init container.

**Chunk 4b removes the annotation-data volume.** Today the worker needs the
annotation tree on disk; once sources are read from S3 by range request, the
worker becomes stateless and the PVC in `worker.yaml` can go.
