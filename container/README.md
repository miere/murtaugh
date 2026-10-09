# Murtaugh Gateway container

One image, no rebuilding per deployment. It downloads the prebuilt Linux release binary
(`murtaugh-gateway-<version>-x86_64-unknown-linux-gnu.tar.gz` from this repo's GitHub
releases) onto a distroless runtime, and reads everything else — Slack tokens, database
backend — from a `murtaugh.toml` you mount at `/etc/murtaugh/murtaugh.toml`. The image
itself never changes between standalone and cloud setups.

`murtaugh.toml` in this folder is the default template: SQLite backend, tokens from env
vars, listening on `0.0.0.0:9000` so nodes outside the container's own network namespace
can reach it.

## Getting the image

Every release publishes `ghcr.io/miere/murtaugh:<tag>`, and the newest one is also `latest`:

```
docker pull ghcr.io/miere/murtaugh:v1.2.3
```

The Release workflow does that once the binaries are up. To republish an older tag, run the
**Container** workflow by hand with that tag and `push` ticked.

## Building

```
docker buildx build \
  --build-arg VERSION=v1.2.3 \
  -t ghcr.io/miere/murtaugh:v1.2.3 \
  container
```

The build reads the release through GitHub's API, which allows 60 anonymous requests an hour
per address. On a shared address, add `--secret id=gh_token,env=GH_TOKEN` with any GitHub
token: it is a BuildKit secret, so it never lands in an image layer or the build cache.

## Run it standalone (SQLite)

The default `murtaugh.toml` works as-is. The database path in it (`/data/murtaugh.db`)
needs a volume, or every admin and node grant resets on restart:

```
docker run \
  -e SLACK_APP_TOKEN=xapp-... \
  -e SLACK_BOT_TOKEN=xoxb-... \
  -v "$(pwd)/murtaugh.toml:/etc/murtaugh/murtaugh.toml:ro" \
  -v murtaugh-data:/data \
  -p 9000:9000 \
  ghcr.io/miere/murtaugh:v1.2.3
```

## Run it on Google Cloud (Cloud Run + Firestore)

Cloud Run won't mount a volume from outside the platform, so the config file itself
becomes a Secret Manager secret, mounted read-only at the same `/etc/murtaugh/murtaugh.toml`
path the image already expects:

1. Write a Firestore variant of the config — same file, `backend = "firestore"` under
   `[database]`, a `[database.firestore]` block with your `project_id`, and no
   `credentials_file`. Leave that out entirely: with nothing set, the gateway falls back to
   Application Default Credentials, which on Cloud Run is whatever service account is
   attached to the revision. No key file to bake in or rotate.
2. `gcloud secrets create murtaugh-config --data-file=murtaugh.firestore.toml`
3. Create a service account, grant it a Firestore role (`roles/datastore.user` is enough),
   and attach it to the Cloud Run service.
4. Deploy with that secret mounted as a volume at `/etc/murtaugh/murtaugh.toml`, and Slack
   tokens passed as regular secret-backed env vars.

One gotcha specific to Cloud Run: `nodes.listen` in the toml is not one of the fields that
expands `${VAR}` placeholders (only the Slack tokens do), so it can't reference Cloud Run's
dynamic `$PORT`. Instead, keep `nodes.listen` a fixed value (`0.0.0.0:9000` is fine) and
pass `--port 9000` on `gcloud run deploy` so Cloud Run routes to the same port the gateway
is actually listening on.
