#!/usr/bin/env bash
# Idempotently prepares an Ubuntu x86_64 host for running the VoxKube E2E suite:
#   - 2G swapfile (if none exists)
#   - Docker, jq, curl, git, openssl, Helm
#   - Lean k3s (without traefik/servicelb/metrics-server) + Ubuntu 26.04 AppArmor fix
#   - Local test images: voxkube-e2e-agent:el9 (UBI9 + openvox-agent 8) and
#     voxkube-e2e-git:latest (self-contained HTTP Basic Auth + SSH Git server)
set -euo pipefail

BUILD_DIR="${BUILD_DIR:-/tmp/voxkube-e2e-build}"

echo "=== [00] Bootstrapping host for VoxKube E2E ==="

# 1. Swap safety net (2G)
if ! swapon --show | grep -q '/swapfile'; then
  echo "Creating 2G swapfile..."
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile
  swapon /swapfile
  grep -q '/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

# 2. Base packages + Docker
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq jq curl ca-certificates git openssl docker.io
systemctl enable --now docker

# 3. Helm
if ! command -v helm >/dev/null 2>&1; then
  echo "Installing Helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
fi

# 4. k3s (lean, kubeconfig readable, flannel + local-path-provisioner)
# Ubuntu 26.04 AppArmor stacking breaks intra-container signals under cri-containerd.apparmor.d.
mkdir -p /var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d
AA_CFG=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/disable-apparmor.toml
AA_CHANGED=0
if [ ! -f "${AA_CFG}" ]; then
  cat <<'EOF' > "${AA_CFG}"
[plugins.'io.containerd.cri.v1.runtime']
  disable_apparmor = true
EOF
  AA_CHANGED=1
fi

if ! command -v k3s >/dev/null 2>&1; then
  echo "Installing k3s..."
  curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--disable traefik --disable servicelb --disable metrics-server --write-kubeconfig-mode 644" sh -
elif [ "${AA_CHANGED}" -eq 1 ]; then
  echo "Restarting k3s to apply containerd AppArmor setting..."
  systemctl restart k3s
fi

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
echo "Waiting for k3s node to be Ready..."
for _ in $(seq 1 60); do
  if kubectl get nodes 2>/dev/null | grep -q ' Ready'; then
    break
  fi
  sleep 2
done
kubectl get nodes

# 5. Build reusable openvox-agent Docker image (EL9 / UBI9 + OpenVox 8 agent + staged node_exporter binary)
AGENT_DIR="${BUILD_DIR}/agent-image"
mkdir -p "${AGENT_DIR}"
cat <<'EOF' > "${AGENT_DIR}/Dockerfile"
FROM registry.access.redhat.com/ubi9/ubi:latest
ARG NODE_EXPORTER_VERSION=1.9.0
RUN dnf install -y https://yum.voxpupuli.org/openvox8-release-el-9.noarch.rpm && \
    dnf install -y --setopt=install_weak_deps=False openvox-agent procps-ng iproute tar gzip && \
    dnf clean all && \
    curl -fsSL "https://github.com/prometheus/node_exporter/releases/download/v${NODE_EXPORTER_VERSION}/node_exporter-${NODE_EXPORTER_VERSION}.linux-amd64.tar.gz" \
      | tar -xzf - -C /tmp && \
    install -d -m 0755 /usr/local/share/node_exporter && \
    install -m 0755 "/tmp/node_exporter-${NODE_EXPORTER_VERSION}.linux-amd64/node_exporter" /usr/local/share/node_exporter/node_exporter && \
    rm -rf "/tmp/node_exporter-${NODE_EXPORTER_VERSION}.linux-amd64"
ENV PATH="/opt/puppetlabs/bin:/opt/puppetlabs/puppet/bin:${PATH}"
CMD ["sleep", "infinity"]
EOF

echo "Building voxkube-e2e-agent:el9..."
docker build -q -t voxkube-e2e-agent:el9 "${AGENT_DIR}" >/dev/null

# 6. Build tiny self-contained Git HTTP+SSH test server image (for g10k httpsToken + sshKey tests)
GIT_DIR="${BUILD_DIR}/git-server-image"
mkdir -p "${GIT_DIR}"
cat <<'EOF' > "${GIT_DIR}/Dockerfile"
FROM docker.io/library/alpine:3.21
RUN apk add --no-cache git git-daemon openssh-server lighttpd lighttpd-mod_auth apache2-utils bash && \
    ssh-keygen -A && \
    adduser -D -s /bin/sh git && \
    passwd -u git || true && \
    sed -i 's/^git:!/git:*/' /etc/shadow && \
    mkdir -p /srv/git /home/git/.ssh /run/lighttpd && \
    chown -R git:git /srv/git /home/git/.ssh && \
    chmod 700 /home/git/.ssh && \
    git config --system --add safe.directory '*'
COPY start.sh /start.sh
RUN chmod +x /start.sh
EXPOSE 8080 2222
CMD ["/start.sh"]
EOF

cat <<'EOF' > "${GIT_DIR}/start.sh"
#!/bin/sh
set -eu

# Configure SSH authorized_keys if mounted
if [ -f /auth/authorized_keys ]; then
  cp /auth/authorized_keys /home/git/.ssh/authorized_keys
  chown git:git /home/git/.ssh/authorized_keys
  chmod 600 /home/git/.ssh/authorized_keys
fi

# Initialize bare control-repo with a production branch if not present
if [ ! -d /srv/git/control.git ]; then
  git init --bare /srv/git/control.git
  git --git-dir=/srv/git/control.git config http.receivepack true
  WORK=/tmp/seed
  rm -rf "$WORK"
  git clone /srv/git/control.git "$WORK"
  cd "$WORK"
  git config user.email "e2e@voxkube.local"
  git config user.name "VoxKube E2E"
  git checkout -b production
  mkdir -p manifests data/roles site/role/manifests site/profile/manifests
  cat <<'ENVCONF' > environment.conf
modulepath = site:modules:$basemodulepath
ENVCONF
  cat <<'PUPPETFILE' > Puppetfile
forge 'https://forgeapi.puppet.com'

mod 'puppetlabs-stdlib', '9.6.0'
PUPPETFILE
  cat <<'HIERA' > hiera.yaml
---
version: 5
defaults:
  datadir: data
  data_hash: yaml_data
hierarchy:
  - name: "Per-role data"
    path: "roles/%{role}.yaml"
  - name: "Common data"
    path: "common.yaml"
HIERA
  cat <<'COMMONYAML' > data/common.yaml
---
profile::node_exporter::listen_port: 9100
COMMONYAML
  cat <<'ROLEYAML' > data/roles/webserver.yaml
---
profile::node_exporter::listen_port: 9100
profile::node_exporter::extra_flags: "--collector.textfile.directory=/var/lib/node_exporter/textfile_collector"
ROLEYAML
  cat <<'ROLEPP' > site/role/manifests/webserver.pp
class role::webserver {
  include profile::nginx
  include profile::node_exporter
}
ROLEPP
  cat <<'NGINXPP' > site/profile/manifests/nginx.pp
class profile::nginx {
  $port = getvar('http_port') ? { undef => '80', default => getvar('http_port') }
  file { '/etc/voxkube-nginx.conf':
    ensure  => file,
    content => "# Managed by Git control-repo\nserver { listen ${port}; }\n",
    mode    => '0644',
  }
}
NGINXPP
  cat <<'EXPORTERPP' > site/profile/manifests/node_exporter.pp
class profile::node_exporter (
  Integer $listen_port = 9100,
  String  $extra_flags = '',
) {
  $role  = getvar('role') ? { undef => 'unknown', default => getvar('role') }
  $clust = getvar('voxkube_cluster') ? { undef => 'unknown', default => getvar('voxkube_cluster') }

  file { ['/etc/node_exporter', '/var/lib/node_exporter', '/var/lib/node_exporter/textfile_collector']:
    ensure => directory,
    mode   => '0755',
  }

  # Exercises puppetlabs-stdlib custom resource type (deployed via Puppetfile + g10k + puppet generate types)
  file { '/etc/node_exporter/exporter.env':
    ensure => file,
    mode   => '0644',
  }
  file_line { 'node_exporter_port':
    path    => '/etc/node_exporter/exporter.env',
    line    => "NODE_EXPORTER_PORT=${listen_port}",
    match   => '^NODE_EXPORTER_PORT=',
    require => File['/etc/node_exporter/exporter.env'],
  }

  file { '/var/lib/node_exporter/textfile_collector/voxkube.prom':
    ensure  => file,
    mode    => '0644',
    content => "# HELP voxkube_managed_node_info Node configured by VoxKube Puppet control-repo\n# TYPE voxkube_managed_node_info gauge\nvoxkube_managed_node_info{role=\"${role}\",cluster=\"${clust}\"} 1\n",
    require => File['/var/lib/node_exporter/textfile_collector'],
  }

  file { '/usr/local/bin/node_exporter':
    ensure => file,
    source => '/usr/local/share/node_exporter/node_exporter',
    mode   => '0755',
  }

  exec { 'start-node-exporter':
    command => "/bin/sh -c 'setsid /usr/local/bin/node_exporter --web.listen-address=:${listen_port} ${extra_flags} </dev/null >/var/log/node_exporter.log 2>&1 &'",
    unless  => '/usr/bin/pgrep -x node_exporter',
    require => [
      File['/usr/local/bin/node_exporter'],
      File['/var/lib/node_exporter/textfile_collector/voxkube.prom'],
      File_line['node_exporter_port'],
    ],
  }
}
EXPORTERPP
  cat <<'SITEPP' > manifests/site.pp
node default {
  $role  = getvar('role') ? { undef => 'default', default => getvar('role') }
  $clust = getvar('voxkube_cluster') ? { undef => 'unknown', default => getvar('voxkube_cluster') }
  $token = lookup('secret_token', Optional[Sensitive[String]], 'first', undef)
  $has_secret = $token ? { undef => 'no', default => 'yes' }

  if $role == 'webserver' {
    include role::webserver
  }

  file { '/etc/voxkube-git.txt':
    ensure  => file,
    content => "GitRev=v1\nNode=${trusted['certname']}\nCluster=${clust}\nRole=${role}\nHasSecret=${has_secret}\n",
    mode    => '0644',
  }
  if $token != undef {
    file { '/etc/voxkube-secret.txt':
      ensure  => file,
      content => $token,
      mode    => '0600',
    }
  }
}
SITEPP
  git add .
  git commit -m "Initial control-repo v1"
  git push origin production
  git --git-dir=/srv/git/control.git symbolic-ref HEAD refs/heads/production
  cd /
  rm -rf "$WORK"
  chown -R git:git /srv/git/control.git
fi

# Configure HTTP Basic Auth htpasswd (user: gituser, pass: e2e-token-123)
htpasswd -bc /tmp/htpasswd "${GIT_HTTP_USER:-gituser}" "${GIT_HTTP_PASS:-e2e-token-123}"

cat <<'CONF' > /tmp/lighttpd.conf
server.document-root = "/srv/git"
server.port = 8080
server.modules = ("mod_alias", "mod_cgi", "mod_setenv", "mod_auth", "mod_authn_file")
auth.backend = "htpasswd"
auth.backend.htpasswd.userfile = "/tmp/htpasswd"
auth.require = ( "/" => ( "method" => "basic", "realm" => "git", "require" => "valid-user" ) )
alias.url += ( "/git" => "/usr/libexec/git-core/git-http-backend" )
$HTTP["url"] =~ "^/git" {
  cgi.assign = ( "" => "" )
  setenv.add-environment = (
    "GIT_PROJECT_ROOT" => "/srv/git",
    "GIT_HTTP_EXPORT_ALL" => "1"
  )
}
CONF

# Start sshd on port 2222
cat <<'SSHD' > /tmp/sshd_config
Port 2222
HostKey /etc/ssh/ssh_host_ed25519_key
HostKey /etc/ssh/ssh_host_rsa_key
PidFile /tmp/sshd.pid
AuthorizedKeysFile .ssh/authorized_keys
PasswordAuthentication no
PubkeyAuthentication yes
AllowUsers git
Subsystem sftp internal-sftp
SSHD

/usr/sbin/sshd -f /tmp/sshd_config
exec lighttpd -D -f /tmp/lighttpd.conf
EOF

echo "Building voxkube-e2e-git:latest..."
docker build -q -t voxkube-e2e-git:latest "${GIT_DIR}" >/dev/null

echo "Importing test images into k3s containerd..."
docker save voxkube-e2e-agent:el9 | k3s ctr images import - >/dev/null
docker save voxkube-e2e-git:latest | k3s ctr images import - >/dev/null

echo "=== [00] Host bootstrap complete ==="
