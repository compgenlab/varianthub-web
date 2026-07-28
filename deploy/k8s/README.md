# Kubernetes deploy (reference)

**These manifests are a starting point, not the deployed configuration.** The
production deployment is managed in a separate k8s deploy repo, which owns the
real secrets, storage classes, ingress, and Postgres. Treat this directory as
worked examples for anyone self-hosting from the published repo — and as seed
material for that deploy repo — rather than something to keep in lockstep with
production.

The supported published path is [`../compose`](../compose), which ships with the
code and works out of the box.

What differs between the compose stack and a real k8s deployment:

| | compose (dev) | k8s (production) |
|---|---|---|
| Postgres | in-stack container, throwaway volume | managed instance; `postgres.yaml` is dev-only |
| Migrations | `migrate` service on every `up` | one-shot Job, run before rolling api/worker |
| Annotation config | `seed` service writes a starter snapshot | real catalog; no seeding |
| Secrets | defaults in the compose file | Secret manifest / external secret manager |
| `varhub` | built from your local checkout | baked into the released image |
| Replicas | 1 api, 1 worker | 2+ api, N workers |

Plain manifests, no Helm — apply in order:

```sh
kubectl apply -f namespace.yaml
kubectl apply -f secret.example.yaml   # copy and edit first; do NOT commit real secrets
kubectl apply -f configmap.yaml
kubectl apply -f postgres.yaml         # dev only; use managed Postgres in production
kubectl apply -f migrate-job.yaml      # wait for completion before the next two
# optional: `varianthub-web seed` for a starter snapshot on an empty catalog
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

**The image contains both binaries.** `deploy/compose/Dockerfile` builds
`varianthub-web` and `varhub` into one image, so the worker needs no bind mount
and no init container. For dev it compiles `varhub` from a local checkout via the
`varhub-src` build context; for production, replace that stage with one that
fetches a released `varhub` binary, so the image is reproducible from tags alone
rather than from whatever happens to be checked out.

**The worker's PVC holds source *data*, not config.** Annotation config is
materialized per job from the Postgres catalog into a temp dir, so there is no
config tree to mount or keep in sync across pods. The PVC holds downloaded
source files and the indexes built from them, which are expensive to refetch.
Chunk 4b removes it by reading sources from S3 by range request, at which point
the worker is stateless.
