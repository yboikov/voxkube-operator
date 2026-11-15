#!/bin/bash
set -o errexit
set -o pipefail
set -o nounset

JAVA_BIN="/usr/bin/java"
JAVA_ARGS="${JAVA_ARGS:--Xms512m -Xmx512m}"
INSTALL_DIR="/opt/puppetlabs/server/apps/puppetserver"
CONFIG="/etc/puppetlabs/puppetserver/conf.d"
BOOTSTRAP_CONFIG="/etc/puppetlabs/puppetserver/services.d/,/opt/puppetlabs/server/apps/puppetserver/config/services.d/"

echo "Starting OpenVox Server (direct java, PID $$)"

# shellcheck disable=SC2086
exec "${JAVA_BIN}" ${JAVA_ARGS} \
    --add-opens java.base/sun.nio.ch=ALL-UNNAMED \
    --add-opens java.base/java.io=ALL-UNNAMED \
    -Dlogappender=STDOUT \
    -cp "${INSTALL_DIR}/puppet-server-release.jar" \
    clojure.main -m puppetlabs.trapperkeeper.main \
    --config "${CONFIG}" \
    --bootstrap-config "${BOOTSTRAP_CONFIG}"
