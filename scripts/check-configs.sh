#!/bin/bash
# Валидация конфигов безопасности на тестовых значениях: Alertmanager, realm
# Keycloak, nginx, oauth2-proxy и compose-файлы. Реальные секреты не нужны.
set -euo pipefail
cd "$(dirname "$0")/.."
obs=observability

cleanup() {
  rm -f "$obs/alertmanager/ci-check.yml" "$obs/keycloak/ci-check.json" "$obs/nginx/ci-check.conf"
  docker rm -f o2p-ci >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Сертификаты для проверки"
sh "$obs/certs/gen-certs.sh" >/dev/null

echo "==> Alertmanager"
SMTP_USERNAME=ci@example.com SMTP_PASSWORD=dummy SMTP_TO=ci@example.com \
  envsubst '$SMTP_USERNAME $SMTP_PASSWORD $SMTP_TO' \
  < "$obs/alertmanager/alertmanager.yml.tmpl" > "$obs/alertmanager/ci-check.yml"
docker run --rm -v "$PWD/$obs/alertmanager:/cfg:ro" \
  --entrypoint /bin/amtool prom/alertmanager:v0.27.0 check-config /cfg/ci-check.yml >/dev/null
echo "    ok"

echo "==> Realm Keycloak"
GRAFANA_OAUTH_SECRET=dummy OAUTH2_PROXY_CLIENT_SECRET=dummy DEMO_USER_PASSWORD=dummy \
  envsubst '$GRAFANA_OAUTH_SECRET $OAUTH2_PROXY_CLIENT_SECRET $DEMO_USER_PASSWORD' \
  < "$obs/keycloak/realm-otus.json.tmpl" > "$obs/keycloak/ci-check.json"
for client in otus-app grafana oauth2-proxy; do
  jq -e --arg c "$client" '.clients[] | select(.clientId == $c)' \
    "$obs/keycloak/ci-check.json" >/dev/null || { echo "    нет клиента $client"; exit 1; }
done
echo "    ok"

echo "==> nginx"
sed -E 's#(proxy_pass https?://)[a-z0-9-]+:#\1127.0.0.1:#' "$obs/nginx/default.conf" \
  | sed 's#/etc/letsencrypt/live/zhemchugovei.duckdns.org/fullchain.pem#/etc/nginx/certs/nginx.crt#; s#/etc/letsencrypt/live/zhemchugovei.duckdns.org/privkey.pem#/etc/nginx/certs/nginx.key#' \
  > "$obs/nginx/ci-check.conf"
docker run --rm \
  -v "$PWD/$obs/nginx/ci-check.conf:/etc/nginx/conf.d/default.conf:ro" \
  -v "$PWD/$obs/certs/nginx.crt:/etc/nginx/certs/nginx.crt:ro" \
  -v "$PWD/$obs/certs/nginx.key:/etc/nginx/certs/nginx.key:ro" \
  -v "$PWD/$obs/certs/ca.crt:/etc/nginx/certs/ca.crt:ro" \
  nginx:1.27-alpine nginx -t 2>&1 | grep -q "test is successful"
echo "    ok"

echo "==> oauth2-proxy"
docker run -d --name o2p-ci -p 127.0.0.1:4180:4180 \
  -e OAUTH2_PROXY_PROVIDER=oidc \
  -e OAUTH2_PROXY_CLIENT_ID=oauth2-proxy \
  -e OAUTH2_PROXY_CLIENT_SECRET=dummy \
  -e OAUTH2_PROXY_COOKIE_SECRET="$(openssl rand -base64 32 | tr -- '+/' '-_')" \
  -e OAUTH2_PROXY_OIDC_ISSUER_URL=https://zhemchugovei.duckdns.org/auth/realms/otus \
  -e OAUTH2_PROXY_SKIP_OIDC_DISCOVERY=true \
  -e OAUTH2_PROXY_LOGIN_URL=https://zhemchugovei.duckdns.org/auth/realms/otus/protocol/openid-connect/auth \
  -e OAUTH2_PROXY_REDEEM_URL=http://keycloak:8080/auth/realms/otus/protocol/openid-connect/token \
  -e OAUTH2_PROXY_OIDC_JWKS_URL=http://keycloak:8080/auth/realms/otus/protocol/openid-connect/certs \
  -e OAUTH2_PROXY_REDIRECT_URL=https://zhemchugovei.duckdns.org/oauth2/callback \
  -e OAUTH2_PROXY_EMAIL_DOMAINS='*' \
  -e OAUTH2_PROXY_REVERSE_PROXY=true \
  -e OAUTH2_PROXY_HTTP_ADDRESS=0.0.0.0:4180 \
  -e OAUTH2_PROXY_UPSTREAMS=static://202 \
  quay.io/oauth2-proxy/oauth2-proxy:v7.6.0 >/dev/null
sleep 3
curl -fsS -m 5 http://127.0.0.1:4180/ping >/dev/null
echo "    ok"

echo "==> docker compose"
docker compose -f "$obs/docker-compose.yml" config -q
KEYCLOAK_ADMIN_PASSWORD=dummy GRAFANA_ADMIN_PASSWORD=dummy GRAFANA_OAUTH_SECRET=dummy \
OAUTH2_PROXY_CLIENT_SECRET=dummy OAUTH2_PROXY_COOKIE_SECRET=dummy TAG=latest \
  docker compose -f "$obs/docker-compose.server.yml" config -q
echo "    ok"

echo "Все конфиги валидны"
