#!/bin/sh
# Внутренний CA и сертификаты для mTLS между сервисами.
# Ключи защищены правами каталога (700), в git не попадают.
set -e

cd "$(dirname "$0")"
chmod 700 .

days=825

if [ ! -f ca.key ]; then
  openssl genrsa -out ca.key 4096
  openssl req -x509 -new -key ca.key -sha256 -days 1825 \
    -subj "/CN=otus-internal-ca" -out ca.crt
  chmod 600 ca.key
fi

issue() {
  name="$1"
  san="$2"
  eku="$3"
  [ -f "$name.crt" ] && return 0
  openssl genrsa -out "$name.key" 2048
  openssl req -new -key "$name.key" -subj "/CN=$name" -out "$name.csr"
  {
    [ -n "$san" ] && echo "subjectAltName=$san"
    echo "extendedKeyUsage=$eku"
    echo "keyUsage=digitalSignature,keyEncipherment"
  } > "$name.ext"
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -days "$days" -sha256 -extfile "$name.ext" -out "$name.crt"
  rm -f "$name.csr" "$name.ext"
  chmod 644 "$name.key" "$name.crt"
}

issue app "DNS:app,DNS:otus-app,DNS:localhost,IP:127.0.0.1" "serverAuth,clientAuth"
issue gateway "" "clientAuth"
issue nginx "" "clientAuth"
issue prometheus "" "clientAuth"

echo "Сертификаты готовы: $(pwd)"
