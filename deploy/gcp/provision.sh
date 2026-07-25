#!/usr/bin/env bash
# Provision the GCP footprint for the runtime distributed deployment:
#   - one custom-mode VPC + subnet
#   - firewall: SSH from anywhere, all agent/CP ports VPC-internal only
#   - Cloud Router + Cloud NAT so the (public-IP-less) VMs have OUTBOUND access
#     to the LLM proxy endpoint and to pull container images
#   - three e2-standard-2 VMs (control-plane, agent-go, agent-python), no public IP
#
# Idempotent: re-running skips resources that already exist.
#
#   PROJECT=my-proj REGION=asia-southeast1 ZONE=asia-southeast1-a ./provision.sh
#
# The reserved internal IPs match control-plane/runtime.remote.yaml.
set -euo pipefail

PROJECT="${PROJECT:?set PROJECT}"
REGION="${REGION:-asia-southeast1}"
ZONE="${ZONE:-asia-southeast1-a}"
NET=runtime-net
SUBNET=runtime-subnet
ROUTER=runtime-router
NAT=runtime-nat
MACHINE="${MACHINE:-e2-standard-2}"
IMAGE_FAMILY=debian-12
IMAGE_PROJECT=debian-cloud

g() { gcloud --project "$PROJECT" "$@"; }
expected_ip() {
  case "$1" in
    control-plane) printf '%s\n' 10.10.0.2 ;;
    agent-go) printf '%s\n' 10.10.0.3 ;;
    agent-python) printf '%s\n' 10.10.0.4 ;;
    *) echo "unknown VM role: $1" >&2; return 1 ;;
  esac
}

# --- VPC + subnet ---
g compute networks describe "$NET" >/dev/null 2>&1 || \
  g compute networks create "$NET" --subnet-mode=custom
g compute networks subnets describe "$SUBNET" --region "$REGION" >/dev/null 2>&1 || \
  g compute networks subnets create "$SUBNET" --network "$NET" \
    --region "$REGION" --range 10.10.0.0/24

# Reserve the exact internal addresses referenced by runtime.remote.yaml. This
# makes a recreate deterministic and prevents an unrelated VM from taking an
# agent address while the runtime VM is stopped or replaced.
for vm in control-plane agent-go agent-python; do
  address_name="runtime-$vm-internal"
  ip="$(expected_ip "$vm")"
  if g compute addresses describe "$address_name" --region "$REGION" >/dev/null 2>&1; then
    actual="$(g compute addresses describe "$address_name" --region "$REGION" --format='get(address)')"
    if [ "$actual" != "$ip" ]; then
      echo "$address_name is $actual, expected $ip; refusing a non-deterministic deployment" >&2
      exit 1
    fi
  else
    g compute addresses create "$address_name" --region "$REGION" \
      --subnet "$SUBNET" --addresses "$ip"
  fi
done

# --- Firewall: SSH only from IAP, all agent/CP ports VPC-internal only ---
g compute firewall-rules describe runtime-allow-ssh >/dev/null 2>&1 || \
  g compute firewall-rules create runtime-allow-ssh --network "$NET" \
    --allow tcp:22 --source-ranges 35.235.240.0/20
g compute firewall-rules describe runtime-allow-internal >/dev/null 2>&1 || \
  g compute firewall-rules create runtime-allow-internal --network "$NET" \
    --allow tcp:8080,tcp:8302,tcp:5432,tcp:9090,tcp:3000,tcp:16686 \
    --source-ranges 10.10.0.0/24
# Allow Google's IAP range to reach the control-plane + dashboard ports, so
# `gcloud compute start-iap-tunnel <vm> 8080 ...` works (the VMs have no public
# IP; the internal rule above only covers VPC-internal traffic). Safe: IAP
# authenticates every connection before it reaches the VM, so only authorized
# identities in this project can open the tunnel — nothing is exposed publicly.
# Without this, start-iap-tunnel to 8080 fails ("failed to connect to backend")
# and you must fall back to SSH local-forward (ssh -L) over port 22.
g compute firewall-rules describe runtime-allow-iap >/dev/null 2>&1 || \
  g compute firewall-rules create runtime-allow-iap --network "$NET" \
    --direction INGRESS --allow tcp:8080,tcp:16686,tcp:3000,tcp:9090 \
    --source-ranges 35.235.240.0/20

# Public HTTPS for the console: allow 80/443 from the internet, but ONLY to VMs
# tagged `runtime-https` (the control plane, instance A). Caddy on A terminates
# TLS and reverse-proxies to runtimed:8080; runtimed and the dashboards are NOT
# published publicly (they stay on the IAP rule above). This is safe because
# runtime identity is ON, so every console/API request is authenticated. Tag the
# control plane to opt it in:
#   gcloud compute instances add-tags runtime-control-plane --zone "$ZONE" --tags runtime-https
# Port 80 is needed for the ACME HTTP-01 challenge + HTTP->HTTPS redirect.
g compute firewall-rules describe runtime-allow-https >/dev/null 2>&1 || \
  g compute firewall-rules create runtime-allow-https --network "$NET" \
    --direction INGRESS --allow tcp:80,tcp:443 \
    --source-ranges 0.0.0.0/0 --target-tags runtime-https

# --- Cloud Router + Cloud NAT: outbound egress for VMs that have NO public IP ---
# Required (not optional): the VMs are created with --no-address, so without NAT
# they cannot reach the LLM proxy endpoint or pull container images. NAT keeps the
# VMs private (no inbound public surface) while granting outbound internet.
# NOTE: this gives the VPC a route to the PUBLIC internet. It assumes the LiteLLM
# endpoint is public-internet-reachable from GCP. If that endpoint is on a private
# gov network instead, NAT alone is not enough — you would need VPN/Interconnect
# and can skip this block.
g compute routers describe "$ROUTER" --region "$REGION" >/dev/null 2>&1 || \
  g compute routers create "$ROUTER" --network "$NET" --region "$REGION"
g compute routers nats describe "$NAT" --router "$ROUTER" --region "$REGION" >/dev/null 2>&1 || \
  g compute routers nats create "$NAT" --router "$ROUTER" --region "$REGION" \
    --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges

# --- Three VMs (Docker installed via startup script) ---
# Install Docker CE + the compose plugin from Docker'\''s OWN apt repo. Debian 12'\''s
# default repos do NOT carry docker-compose-plugin (only the legacy docker.io),
# so we add Docker'\''s repo to get `docker compose` (v2). Uses `apt-get update`
# against that repo specifically so a transient mirror hiccup is easy to spot.
STARTUP='#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ca-certificates curl gnupg git
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
usermod -aG docker $(getent passwd 1000 | cut -d: -f1) || true'

for vm in control-plane agent-go agent-python; do
  ip="$(expected_ip "$vm")"
  if g compute instances describe "runtime-$vm" --zone "$ZONE" >/dev/null 2>&1; then
    actual="$(g compute instances describe "runtime-$vm" --zone "$ZONE" \
      --format='get(networkInterfaces[0].networkIP)')"
    if [ "$actual" != "$ip" ]; then
      echo "runtime-$vm uses $actual, expected reserved address $ip" >&2
      exit 1
    fi
    echo "runtime-$vm exists at $ip, skipping"
    continue
  fi
  g compute instances create "runtime-$vm" \
    --zone "$ZONE" --machine-type "$MACHINE" \
    --image-family "$IMAGE_FAMILY" --image-project "$IMAGE_PROJECT" \
    --network "$NET" --subnet "$SUBNET" --private-network-ip "$ip" --no-address \
    --metadata startup-script="$STARTUP"
done

echo "--- reserved internal IPs (already match runtime.remote.yaml) ---"
for vm in control-plane agent-go agent-python; do
  ip=$(g compute instances describe "runtime-$vm" --zone "$ZONE" \
    --format='get(networkInterfaces[0].networkIP)')
  printf '  runtime-%-14s %s\n' "$vm" "$ip"
done

cat <<'EOF'

--- egress check (run on a VM after it boots) ---
  gcloud compute ssh runtime-agent-go --zone "$ZONE" --tunnel-through-iap \
    --command 'curl -sS -o /dev/null -w "%{http_code}\n" "$OPENAI_BASE_URL"'
  # A non-000 HTTP status confirms Cloud NAT egress + route to the LLM endpoint.
EOF
