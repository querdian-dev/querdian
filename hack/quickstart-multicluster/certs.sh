#!/usr/bin/env bash
# Generates the certs of the multi-cluster quickstart with openssl. DEMO ONLY: all CA keys stay in one
# directory on the laptop. In a real deployment only the hub holds the agent CA key (PLAN 18.5).
#
#   hack/quickstart-multicluster/certs.sh DIR HUB_IP TRUST_DOMAIN SPOKE...
#
# DIR must not exist or must be empty. The script writes (EC P-256 keys, 30 days):
#
#   hub-server-ca.crt/.key         CA of the two server certs on the hub
#   receiver.crt/.key              server cert of the receiver (IP SAN HUB_IP)
#   gateway.crt/.key               server cert of the ingest gateway (IP SAN HUB_IP)
#   agent-ca.crt/.key              hub-controlled CA of the agent certs (receiver --client-ca-file)
#   agent-SPOKE.crt/.key           agent cert, one URI SAN spiffe://TRUST_DOMAIN/agent/1/SPOKE
#   ingest-ca-SPOKE.crt/.key       a separate ingest CA for each spoke (the gateway trusts it on the port
#                                  of that spoke only)
#   ingest-SPOKE.crt/.key          ingest client cert, URI SAN spiffe://TRUST_DOMAIN/ingest/SPOKE
#
# The agent certs and the ingest certs have different CAs and different identities, so the receiver
# rejects an ingest cert and the gateway rejects an agent cert (PLAN 18.5, "use a dedicated agent cert").
# At the end the script verifies the chains and the separation with openssl verify.
set -euo pipefail

[ $# -ge 4 ] || { echo "usage: $0 DIR HUB_IP TRUST_DOMAIN SPOKE..." >&2; exit 2; }
D="$1" HUB_IP="$2" TD="$3"
shift 3
command -v openssl >/dev/null 2>&1 || { echo "ERROR: missing required tool: openssl" >&2; exit 1; }
case "$HUB_IP" in
  *[!0-9.]* | '') echo "ERROR: HUB_IP must be an IPv4 address, got '$HUB_IP'" >&2; exit 1 ;;
esac
if [ -d "$D" ] && [ -n "$(ls -A "$D")" ]; then echo "ERROR: $D is not empty" >&2; exit 1; fi
umask 077
mkdir -p "$D"
DAYS=30
CNF="$D/openssl.cnf"
cat >"$CNF" <<'EOF'
[req]
distinguished_name = dn
prompt = no
[dn]
[v3_ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[client]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = clientAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = $ENV::SAN
[server]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = $ENV::SAN
EOF
# The config refers to $ENV::SAN, so the variable must have a value for each command.
export SAN="DNS:unused.invalid"

serial() { od -An -N8 -tu8 /dev/urandom | tr -d ' \n'; }
key() { openssl ecparam -name prime256v1 -genkey -noout -out "$D/$1.key"; }
ca() { # name common-name
  key "$1"
  openssl req -new -x509 -config "$CNF" -extensions v3_ca -key "$D/$1.key" -out "$D/$1.crt" \
    -days "$DAYS" -subj "/O=querdian quickstart/CN=$2" -set_serial "$(serial)"
}
leaf() { # name ca extensions common-name subject-alt-name
  key "$1"
  openssl req -new -config "$CNF" -key "$D/$1.key" -out "$D/$1.csr" -subj "/O=querdian quickstart/CN=$4"
  SAN="$5" openssl x509 -req -in "$D/$1.csr" -CA "$D/$2.crt" -CAkey "$D/$2.key" -set_serial "$(serial)" \
    -days "$DAYS" -extfile "$CNF" -extensions "$3" -out "$D/$1.crt" 2>/dev/null
  rm -f "$D/$1.csr"
}

ca hub-server-ca "hub server CA"
leaf receiver hub-server-ca server "querdian receiver" "IP:$HUB_IP,DNS:querdian-receiver.querdian.svc"
leaf gateway hub-server-ca server "ingest gateway" "IP:$HUB_IP,DNS:ingest-gateway.observability.svc"
ca agent-ca "agent CA (hub)"
for s in "$@"; do
  leaf "agent-$s" agent-ca client "agent $s" "URI:spiffe://$TD/agent/1/$s"
  ca "ingest-ca-$s" "ingest CA $s"
  leaf "ingest-$s" "ingest-ca-$s" client "ingest $s" "URI:spiffe://$TD/ingest/$s"
done
rm -f "$CNF"
chmod 0644 "$D"/*.crt

# --- self-check -----------------------------------------------------------------------------------------
ok() { openssl verify -purpose "$1" -CAfile "$D/$2.crt" "$D/$3.crt" >/dev/null 2>&1; }
fail() { echo "ERROR: cert check: $*" >&2; exit 1; }
ok sslserver hub-server-ca receiver || fail "receiver"
ok sslserver hub-server-ca gateway || fail "gateway"
for s in "$@"; do
  ok sslclient agent-ca "agent-$s" || fail "agent-$s does not verify against agent-ca"
  ok sslclient "ingest-ca-$s" "ingest-$s" || fail "ingest-$s does not verify against ingest-ca-$s"
  ! ok sslclient agent-ca "ingest-$s" || fail "the receiver CA accepts ingest-$s"
  ! ok sslclient "ingest-ca-$s" "agent-$s" || fail "the ingest CA of $s accepts agent-$s"
  for o in "$@"; do
    [ "$o" = "$s" ] && continue
    ! ok sslclient "ingest-ca-$o" "ingest-$s" || fail "the ingest CA of $o accepts ingest-$s"
  done
done
echo "certs written to $D"
