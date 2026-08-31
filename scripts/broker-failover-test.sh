#!/bin/bash
# Нагрузочные тесты брокеров сообщений с отказами узлов и масштабированием
# обработки. Запускать из корня репозитория при поднятом стенде:
#   cd ds && docker compose -f docker-compose.brokers.yml up -d && cd ..
#   bash scripts/broker-failover-test.sh
set -u

COMPOSE="docker compose -f ds/docker-compose.brokers.yml"
NETWORK=otus-brokers_default
ES=${ES:-http://localhost:9200}
RATE=${RATE:-150}
OUT=$(mktemp -d)

INDEXES="otus-kafka otus-rabbit otus-nats"

es_count() {
  curl -s -XPOST "$ES/$1/_refresh" > /dev/null
  curl -s "$ES/$1/_count" | sed -n 's/.*"count":\([0-9]*\).*/\1/p'
}

# Ждем, пока воркеры разгребут очередь: счетчик документов перестал расти.
drain() {
  local prev=-1 same=0 total
  for _ in $(seq 1 60); do
    total=0
    for idx in $INDEXES; do total=$((total + $(es_count "$idx"))); done
    if [ "$total" = "$prev" ]; then
      same=$((same + 1))
      [ "$same" -ge 3 ] && return
    else
      same=0
    fi
    prev=$total
    sleep 2
  done
}

load() { # load <секунды> <лог>
  docker run --rm --network "$NETWORK" \
    -e BASE_URL=http://app:8080 -e RATE="$RATE" -e DURATION="${1}s" -e MAX_VUS=300 \
    -v "$PWD/loadtest:/loadtest:ro" grafana/k6 run /loadtest/brokers.js > "$2" 2>&1
}

created_from_log() {
  sed -n 's/^ *messages_created[. ]*: *\([0-9]*\).*/\1/p' "$1" | head -1
}

failed_from_log() {
  sed -n 's/.*http_req_failed[. ]*: *[0-9.]*% *\([0-9]*\) out of.*/\1/p' "$1" | head -1
}

total_from_log() {
  sed -n 's/.*http_req_failed[. ]*: *[0-9.]*% *[0-9]* out of \([0-9]*\).*/\1/p' "$1" | head -1
}

report() { # report <лог> <файл со счетчиками до>
  local created failed total
  created=$(created_from_log "$1"); failed=$(failed_from_log "$1"); total=$(total_from_log "$1")
  echo "    запросов: ${total:-0}, из них ответ 201: ${created:-0}, отказов у клиента: ${failed:-0}"
  drain
  # Считаем доставку от общего числа запросов, а не от числа ответов 201:
  # запрос, по которому клиент отвалился по таймауту, все равно сохранил
  # сообщение и опубликовал событие.
  while read -r idx before; do
    after=$(es_count "$idx")
    echo "    $idx: доставлено $((after - before)) из ${total:-0} (потеряно $(( ${total:-0} - after + before )))"
  done < "$2"
}

snapshot() { # snapshot <файл>
  : > "$1"
  for idx in $INDEXES; do echo "$idx $(es_count "$idx")" >> "$1"; done
}

kafka_leader() {
  # Узел, который ведет больше всего партиций топика: его отказ заставляет
  # кластер переизбрать лидеров.
  local id
  id=$(docker exec otus-brokers-kafka-1-1 /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server localhost:9092 --describe --topic otus.messages 2>/dev/null |
    sed -n 's/.*Leader: \([0-9]*\).*/\1/p' | sort | uniq -c | sort -rn | head -1 | awk '{print $2}')
  [ -n "$id" ] && echo "otus-brokers-kafka-$id-1"
}

rabbit_leader() {
  local node
  node=$(docker exec otus-brokers-rabbit-1-1 rabbitmqctl -q list_queues name leader 2>/dev/null |
    awk '$1 == "otus.messages" {print $2}' | sed 's/^rabbit@//')
  [ -n "$node" ] && echo "otus-brokers-${node}-1"
}

nats_leader() {
  # Лидер потока OTUS в JetStream: берем поле leader из описания потока.
  local node
  node=$(docker exec otus-brokers-nats-1-1 wget -qO- 'http://localhost:8222/jsz?acc=%24G&streams=1' 2>/dev/null |
    tr -d ' \n' | sed 's/.*"name":"OTUS"//' | grep -o '"leader":"[^"]*"' | head -1 | cut -d'"' -f4)
  [ -n "$node" ] && echo "otus-brokers-${node}-1"
}

scenario() { # scenario <название> <контейнер> <простой>
  local name=$1 container=$2 downtime=$3
  echo "==> $name"
  if [ -z "$container" ]; then
    echo "    не удалось определить узел, сценарий пропущен"; echo; return
  fi
  echo "    гашу $container на ${downtime}с под нагрузкой ${RATE} rps"
  snapshot "$OUT/before"
  load 45 "$OUT/k6.log" &
  local k6_pid=$!
  sleep 12
  docker stop "$container" > /dev/null
  sleep "$downtime"
  docker start "$container" > /dev/null
  wait "$k6_pid"
  report "$OUT/k6.log" "$OUT/before"
  echo
}

echo "Стенд: $($COMPOSE ps --format '{{.Name}}' | wc -l) контейнеров"
echo "Лидеры: kafka=$(kafka_leader) rabbit=$(rabbit_leader) nats=$(nats_leader)"
echo

echo "==> Базовый прогон без отказов"
snapshot "$OUT/before"
load 30 "$OUT/k6.log"
sed -n 's/^ *http_req_duration[. ]*: *\(.*\)/    задержка: \1/p' "$OUT/k6.log" | head -1
report "$OUT/k6.log" "$OUT/before"
echo

scenario "Отказ узла Kafka, который ведет больше всего партиций" "$(kafka_leader)" 20
scenario "Отказ узла RabbitMQ с лидером quorum-очереди" "$(rabbit_leader)" 20
scenario "Отказ узла NATS с лидером потока JetStream" "$(nats_leader)" 20

echo "==> Потеря кворума Kafka (гашу 2 узла из 3)"
snapshot "$OUT/before"
load 45 "$OUT/k6.log" &
k6_pid=$!
sleep 10
docker stop otus-brokers-kafka-2-1 otus-brokers-kafka-3-1 > /dev/null
sleep 20
docker start otus-brokers-kafka-2-1 otus-brokers-kafka-3-1 > /dev/null
wait "$k6_pid"
report "$OUT/k6.log" "$OUT/before"
echo

echo "==> Масштабирование обработки: разбор накопленной очереди"
for replicas in 1 3; do
  $COMPOSE up -d --scale consumer-kafka=0 > /dev/null 2>&1
  sleep 3
  before=$(es_count otus-kafka)
  load 20 "$OUT/k6.log"
  sent=$(created_from_log "$OUT/k6.log")
  $COMPOSE up -d --scale consumer-kafka="$replicas" > /dev/null 2>&1
  start=$(date +%s)
  target=$((before + sent))
  for _ in $(seq 1 120); do
    [ "$(es_count otus-kafka)" -ge "$target" ] && break
    sleep 1
  done
  elapsed=$(( $(date +%s) - start ))
  echo "    воркеров: $replicas, накоплено: $sent, разобрано за ${elapsed}с ($((sent / (elapsed > 0 ? elapsed : 1))) сообщений/с)"
done
$COMPOSE up -d --scale consumer-kafka=1 > /dev/null 2>&1

echo
echo "Готово. Логи прогонов: $OUT"
