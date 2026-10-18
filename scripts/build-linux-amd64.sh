#!/usr/bin/env bash
# Builds dist/alpha-search-model-v<version>.so for the CPA container
# (Debian 12, glibc 2.36). Building on bookworm keeps libc symbols loadable
# there. Modules come from the host module cache, so the container needs no
# network.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(sed -n 's/^[[:space:]]*pluginVersion[[:space:]]*=[[:space:]]*"\([^"]*\)"/\1/p' "${repo_root}/main.go")"
artifact="${repo_root}/dist/alpha-search-model-v${version}.so"
builder="${BUILD_IMAGE:-golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437}"
modcache="${GOMODCACHE:-$(go env GOMODCACHE)}"

mkdir -p -- "${repo_root}/dist"
staging="$(mktemp -d "${repo_root}/dist/.build.XXXXXX")"
trap 'rm -rf -- "${staging}"' EXIT

docker run --rm -i --platform linux/amd64 \
  -v "${repo_root}:/src:ro" -v "${modcache}:/go/pkg/mod:ro" -v "${staging}:/out" -w /src \
  -e GOPROXY=off -e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache \
  "${builder}" bash -s <<'BUILD'
set -euo pipefail
export CGO_ENABLED=1 GOOS=linux GOARCH=amd64
go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags '-s -w' -o /out/plugin.so .
readelf -h /out/plugin.so | grep -Eq 'Machine:.*X86-64'
readelf --dyn-syms --wide /out/plugin.so | grep -Eq '[[:space:]]cliproxy_plugin_init$'
if ldd -r /out/plugin.so 2>&1 | grep -E 'not found|undefined symbol'; then
  exit 1
fi
# Highest glibc symbol version the library needs; must not exceed 2.36.
objdump -T /out/plugin.so | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1 > /out/glibc.txt

cat > /tmp/abi-smoke.c <<'C'
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>

typedef struct {
  uint32_t abi_version;
  void *call;
  void *free_buffer;
  void (*shutdown)(void);
} plugin_api;

int main(void) {
  void *library = dlopen("/out/plugin.so", RTLD_NOW | RTLD_LOCAL);
  if (!library) { fprintf(stderr, "%s\n", dlerror()); return 1; }
  int (*initialize)(void *, plugin_api *) = dlsym(library, "cliproxy_plugin_init");
  plugin_api api = {0};
  if (!initialize || initialize(NULL, &api) != 0 || api.abi_version != 1 ||
      !api.call || !api.free_buffer || !api.shutdown) return 1;
  api.shutdown();
  puts("linux/amd64 plugin ABI load passed");
  return 0;
}
C
gcc -Wall -Wextra -Werror /tmp/abi-smoke.c -ldl -o /tmp/abi-smoke
/tmp/abi-smoke
go version
BUILD

echo "max glibc symbol: $(cat "${staging}/glibc.txt")"
mv -f -- "${staging}/plugin.so" "${artifact}"
(cd -- "${repo_root}/dist" && shasum -a 256 "$(basename -- "${artifact}")" > "$(basename -- "${artifact}").sha256")
cat "${artifact}.sha256"
