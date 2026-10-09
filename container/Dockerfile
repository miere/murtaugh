# syntax=docker/dockerfile:1

# Downloads the prebuilt Linux release binary instead of compiling from source, so this build
# never needs the RAX_READ_TOKEN the release workflow uses for Cargo. The releases are public;
# a token is optional, passed as a BuildKit secret (never baked into the image), and only
# there to lift GitHub's anonymous API rate limit.
FROM debian:trixie-slim AS fetch
ARG VERSION
ARG TARGET=x86_64-unknown-linux-gnu
ARG REPO=miere/murtaugh
SHELL ["/bin/bash", "-c"]
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /dist
# Assets come from the API's asset-by-id endpoint rather than the releases/download links,
# because that is the route that accepts a bearer token when one is given.
RUN --mount=type=secret,id=gh_token \
    test -n "$VERSION" || { echo "pass --build-arg VERSION=vX.Y.Z" >&2; exit 1; } \
    && auth=() \
    && if [ -s /run/secrets/gh_token ]; then auth=(-H "Authorization: Bearer $(cat /run/secrets/gh_token)"); fi \
    && name="murtaugh-gateway-${VERSION}-${TARGET}" \
    && release="$(curl -fsSL "${auth[@]}" -H 'Accept: application/vnd.github+json' \
         "https://api.github.com/repos/${REPO}/releases/tags/${VERSION}")" \
    && fetch_asset() { \
         id="$(jq -r --arg n "$1" '.assets[] | select(.name==$n) | .id' <<<"$release")"; \
         test -n "$id" || { echo "no asset named $1 on release $VERSION" >&2; exit 1; }; \
         curl -fsSL "${auth[@]}" -H 'Accept: application/octet-stream' \
           "https://api.github.com/repos/${REPO}/releases/assets/$id" -o "$1"; \
       } \
    && fetch_asset "$name.tar.gz" \
    && fetch_asset "$name.tar.gz.sha256" \
    && sha256sum -c "$name.tar.gz.sha256" \
    && tar -xzf "$name.tar.gz" \
    && mv "$name/murtaugh-gateway" /dist/murtaugh-gateway
# Pre-owned by the nonroot UID below, so a fresh named volume mounted here inherits
# write access instead of showing up root-owned and unwritable on first run.
RUN mkdir /dist/data && chown 65532:65532 /dist/data

# glibc target binary, so this needs the "cc" distroless variant (not "static"); it ships
# ca-certificates too, which reqwest's platform verifier needs for the Slack API calls.
# Debian 13 because the release runner (ubuntu-latest) links against glibc 2.39, newer than
# Debian 12 carries.
FROM gcr.io/distroless/cc-debian13:nonroot
ARG REPO=miere/murtaugh
# Links the package on ghcr.io to the repository it was built from.
LABEL org.opencontainers.image.source="https://github.com/${REPO}"
COPY --from=fetch /dist/murtaugh-gateway /usr/local/bin/murtaugh-gateway
COPY --from=fetch --chown=65532:65532 /dist/data /data
ENTRYPOINT ["/usr/local/bin/murtaugh-gateway", "--config", "/etc/murtaugh/murtaugh.toml"]
CMD ["run"]
