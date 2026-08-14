#!/bin/bash
# Тесты отказов узлов СУБД под нагрузкой.
# Запускать из корня репозитория при поднятом стенде ha/docker-compose.ha.yml:
#   bash scripts/ha-failover-test.sh
set -u

BASE=${BASE:-http://localhost:8080}
COMPOSE="docker compose -f ha/docker-compose.ha.yml"
LOG=$(mktemp -d)/load.log

load_start() {
  : > "$LOG"
  (
    while :; do
      code=$(curl -s -o /dev/null -w '%{http_code}' -m 3 -X POST "$BASE/messages" \
        -H 'Content-Type: application/json' -d '{"text":"нагрузка"}')
      echo "$(date +%H:%M:%S) POST $code" >> "$LOG"
      code=$(curl -s -o /dev/null -w '%{http_code}' -m 3 "$BASE/messages")
      echo "$(date +%H:%M:%S) GET $code" >> "$LOG"
      sleep 0.2
    done
  ) &
  LOAD_PID=$!
}

load_stop_and_report() {
  kill "$LOAD_PID" 2>/dev/null
  wait "$LOAD_PID" 2>/dev/null
  total=$(wc -l < "$LOG")
  ok=$(grep -c -E '(POST 201|GET 200)' "$LOG")
  limited=$(grep -c ' 429$' "$LOG")
  fail=$((total - ok - limited))
  echo "    запросов: $total, успешных: $ok, ошибок: $fail"
  [ "$limited" -gt 0 ] && echo "    срезано рейт-лимитером: $limited (не считается ошибкой отказа)"
  if [ "$fail" -gt 0 ]; then
    first=$(grep -v -E '(POST 201|GET 200| 429$)' "$LOG" | head -1 | cut -d' ' -f1)
    last=$(grep -v -E '(POST 201|GET 200| 429$)' "$LOG" | tail -1 | cut -d' ' -f1)
    echo "    окно ошибок: с $first по $last"
  fi
}

scenario() {
  name=$1; container=$2; downtime=$3
  echo "==> $name"
  load_start
  sleep 5
  echo "    останавливаю $container"
  docker stop "$container" > /dev/null
  sleep "$downtime"
  echo "    возвращаю $container"
  docker start "$container" > /dev/null
  sleep 10
  load_stop_and_report
  echo
}

pg_primary() {
  for n in 0 1; do
    state=$(docker exec otus-ha-pg-$n-1 psql -U otus -d otus -tAc 'select pg_is_in_recovery()' 2>/dev/null)
    [ "$state" = "f" ] && { echo "otus-ha-pg-$n-1"; return; }
  done
}

mongo_primary() {
  docker exec otus-ha-mongo-0-1 mongosh --quiet --eval 'print(rs.hello().primary)' 2>/dev/null \
    | cut -d: -f1 | sed 's/^/otus-ha-/; s/$/-1/'
}

valkey_master() {
  docker exec otus-ha-sentinel-0-1 valkey-cli -p 26379 sentinel get-master-addr-by-name mymaster 2>/dev/null \
    | head -1 | sed 's/^/otus-ha-/; s/$/-1/'
}

tarantool_leader() {
  for n in 0 1 2; do
    ro=$(docker exec otus-ha-tarantool-$n-1 tarantool -e \
      "local c = require('net.box').connect('app:app-secret@tarantool-$n:3301'); print(c:eval('return box.info.ro')); os.exit(0)" \
      2>/dev/null | tail -1)
    [ "$ro" = "false" ] && { echo "otus-ha-tarantool-$n-1"; return; }
  done
}

echo "Стенд: $($COMPOSE ps --format '{{.Name}}' | wc -l) контейнеров запущено"
# Поднимаю лимит запросов на время теста, чтобы нагрузка не упиралась в рейт-лимитер
docker exec otus-ha-etcd-0-1 etcdctl put /otus/config/rate_limit 100000 > /dev/null
echo

scenario "Отказ первичного узла PostgreSQL (repmgr переключает на реплику)" "$(pg_primary)" 25
scenario "Отказ primary MongoDB (replica set выбирает нового)" "$(mongo_primary)" 20
scenario "Отказ мастера Valkey (sentinel переключает реплику)" "$(valkey_master)" 20
scenario "Отказ лидера Tarantool (raft выбирает нового)" "$(tarantool_leader)" 20

echo "==> Потеря кворума etcd (останавливаю 2 узла из 3)"
load_start
sleep 3
docker stop otus-ha-etcd-1-1 otus-ha-etcd-2-1 > /dev/null
echo "    пробую изменить лимит без кворума:"
docker exec otus-ha-etcd-0-1 etcdctl --command-timeout=3s put /otus/config/rate_limit 5 2>&1 | sed 's/^/    /'
sleep 5
echo "    приложение при этом отвечает:"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/hello")
echo "    GET /hello -> $code"
docker start otus-ha-etcd-1-1 otus-ha-etcd-2-1 > /dev/null
sleep 8
echo "    кворум восстановлен, повторяю запись лимита:"
docker exec otus-ha-etcd-0-1 etcdctl put /otus/config/rate_limit 100000 2>&1 | sed 's/^/    /'
load_stop_and_report
docker exec otus-ha-etcd-0-1 etcdctl put /otus/config/rate_limit 100 > /dev/null
echo
echo "Готово, лимит запросов возвращен к 100. Лог нагрузки: $LOG"
