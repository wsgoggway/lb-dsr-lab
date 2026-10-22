#!/usr/bin/env bash
# Generate the pfkeeper lab PKI: one CA, node certs (lb1/lb2, role node) and
# client certs per role (admin/operator/viewer). Output: certs/<name>.crt/.key
# and certs/ca.crt. Nodes trust only ca.crt; the CA key stays on the host.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=certs
DAYS=${DAYS:-365}   # short-lived by default; rotate by re-running the script
mkdir -p "$OUT"

openssl req -x509 -newkey ed25519 -nodes -keyout "$OUT/ca.key" -out "$OUT/ca.crt" \
  -days "$DAYS" -subj "/CN=pfkeeper-lab-ca" 2>/dev/null

issue() { # issue <name> <cn> <role>
  local name=$1 cn=$2 role=$3
  openssl req -newkey ed25519 -nodes -keyout "$OUT/$name.key" -out "$OUT/$name.csr" \
    -subj "/CN=$cn/O=pfkeeper-$role" 2>/dev/null
  openssl x509 -req -in "$OUT/$name.csr" -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" \
    -CAcreateserial -out "$OUT/$name.crt" -days "$DAYS" \
    -extfile <(printf "subjectAltName=DNS:%s,URI:pfkeeper://role/%s\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth,serverAuth" "$cn" "$role") \
    2>/dev/null
  rm -f "$OUT/$name.csr"
  echo "issued: $name (CN=$cn role=$role)"
}

# node identities - role "node" may call PeeringService only
issue lb1 lb1 node
issue lb2 lb2 node
# client identities - role from the CN
issue client-admin admin admin
issue client-operator operator operator
issue client-viewer viewer viewer

chmod 600 "$OUT"/*.key
echo "CA + certificates written to $OUT/ (DAYS=$DAYS)"
