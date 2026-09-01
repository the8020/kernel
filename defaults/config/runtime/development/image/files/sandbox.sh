#!/usr/bin/env bash
set -euo pipefail

/usr/bin/install -d -m 1777 /run/lock # /run is a fresh tmpfs on every sandbox start; restore Debian's standard lock directory.
exec /usr/bin/sleep infinity
