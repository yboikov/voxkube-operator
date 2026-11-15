#!/bin/bash
set -o errexit
set -o pipefail
set -o nounset

JAVA_BIN="/usr/bin/java"
JAVA_ARGS="${JAVA_ARGS:--Xms256m -Xmx512m}"
INSTALL_DIR="/opt/puppetlabs/server/apps/puppetdb"
CONFIG="/etc/puppetlabs/puppetdb/conf.d"
BOOTSTRAP_CONFIG="/etc/puppetlabs/puppetdb/bootstrap.cfg"

echo "Starting OpenVox DB (direct java, PID $$)"

# shellcheck disable=SC2086
exec "${JAVA_BIN}" ${JAVA_ARGS} \
    -Dlogappender=STDOUT \
    -cp "${INSTALL_DIR}/puppetdb.jar" \
    clojure.main -m puppetlabs.puppetdb.core services \
    -c "${CONFIG}" \
    --bootstrap-config "${BOOTSTRAP_CONFIG}"
