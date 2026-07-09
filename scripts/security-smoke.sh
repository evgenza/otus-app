#!/bin/bash
# Негативные проверки защиты на развёрнутом сервере: интерфейсы наблюдаемости
# без авторизации закрыты, API без токена не пишет, HTTP уводится на HTTPS.
set -u
base="${1:-https://zhemchugovei.duckdns.org}"
host="${base#https://}"
fail=0

expect() {
  local desc="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then
    echo "ok   $desc — $got"
  else
    echo "FAIL $desc — ожидалось $want, получено $got"
    fail=1
  fi
}

for path in prometheus alertmanager jaeger kibana; do
  code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' "$base/$path/")
  expect "/$path/ без сессии закрыт" "302" "$code"
  loc=$(curl -s -m 15 -o /dev/null -w '%{redirect_url}' "$base/$path/")
  case "$loc" in
    "$base/oauth2/start"*) echo "ok   /$path/ уводит на логин Keycloak" ;;
    *) echo "FAIL /$path/ редирект не на логин: $loc"; fail=1 ;;
  esac
done

code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' -X POST "$base/messages" -d '{"text":"ci"}')
expect "POST /messages без токена" "401" "$code"

code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' -X POST "$base/messages" \
  -H "Authorization: Bearer подделка" -d '{"text":"ci"}')
expect "POST /messages с поддельным токеном" "401" "$code"

code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' "http://$host/health")
expect "HTTP уводится на HTTPS" "301" "$code"

code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' "$base/health")
expect "GET /health открыт" "200" "$code"

if [ "$fail" != 0 ]; then
  echo "Есть провалы — смотри выше"
  exit 1
fi
echo "Все проверки защиты прошли"
