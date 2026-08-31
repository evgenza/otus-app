#!/bin/bash
# Нагрузочные тесты распределенных хранилищ с отказами узлов.
# Запускать из корня репозитория при поднятом стенде:
#   cd ds && docker compose -f docker-compose.storage.yml up -d && cd ..
#   bash scripts/storage-failover-test.sh
set -u

COMPOSE="docker compose -f ds/docker-compose.storage.yml"
NETWORK=otus-storage_default
OUT=$(mktemp -d)

load() { # load <секунды> <лог>
  docker run --rm --network "$NETWORK" \
    -e BASE_URL=http://app:8080 -e DURATION="${1}s" \
    -e RATE_MESSAGES="${RATE_MESSAGES:-60}" -e RATE_UPLOAD="${RATE_UPLOAD:-20}" -e RATE_READ="${RATE_READ:-20}" \
    -v "$PWD/loadtest:/loadtest:ro" grafana/k6 run /loadtest/storage.js > "$2" 2>&1
}

summary() { # summary <лог>
  local ok fail
  ok=$(sed -n 's/^ *ops_ok[. ]*: *\([0-9]*\).*/\1/p' "$1" | head -1)
  fail=$(sed -n 's/^ *ops_failed[. ]*: *\([0-9]*\).*/\1/p' "$1" | head -1)
  echo "    успешных операций: ${ok:-0}, неуспешных: ${fail:-0}"
  sed -n 's/^ *http_req_duration[. ]*: *\(.*\)/    задержка: \1/p' "$1" | head -1
  # Проверки по видам операций: где именно прошли отказы. Строка со
  # стрелкой появляется только у проваленных проверок - в ней доли и счет.
  grep -E -A1 '^ *[✓✗] (сообщение создано|объект загружен|кусок получен)' "$1" |
    grep -v '^--$' | sed 's/^ */    /'
}

scenario() { # scenario <название> <контейнер> <простой>
  local name=$1 container=$2 downtime=$3
  echo "==> $name"
  echo "    гашу $container на ${downtime}с"
  load 45 "$OUT/k6.log" &
  local pid=$!
  sleep 12
  docker stop "$container" > /dev/null
  sleep "$downtime"
  docker start "$container" > /dev/null
  wait "$pid"
  summary "$OUT/k6.log"
  echo
}

# Административные команды mc требуют алиаса с ключами.
docker exec otus-storage-minio-1-1 mc alias set otus http://localhost:9000 minioadmin minioadmin > /dev/null 2>&1

echo "Стенд: $($COMPOSE ps --format '{{.Name}}' | wc -l) контейнеров"
echo "MinIO: $(docker exec otus-storage-minio-1-1 mc admin info otus 2>/dev/null | grep -c '^●') узлов в пуле"
echo "HDFS:  $(docker exec otus-storage-namenode-1 hdfs dfsadmin -report 2>/dev/null | sed -n 's/Live datanodes (\([0-9]*\)).*/\1/p') живых датанод"
echo "Cassandra: $(docker exec otus-storage-cassandra-1-1 nodetool status 2>/dev/null | grep -c '^UN') узлов UN"
echo "Elasticsearch: $(curl -s localhost:9200/_cluster/health | sed -n 's/.*"status":"\([a-z]*\)".*/\1/p')"
echo

echo "==> Базовый прогон без отказов"
load 30 "$OUT/k6.log"
summary "$OUT/k6.log"
echo

scenario "Отказ узла MinIO (erasure coding, 3 узла из 4)" otus-storage-minio-2-1 20
scenario "Отказ датаноды HDFS (репликация 3)" otus-storage-datanode-2-1 20
scenario "Отказ узла Cassandra (QUORUM на RF=3)" otus-storage-cassandra-2-1 25
scenario "Отказ узла Elasticsearch (реплики шардов)" otus-storage-es-2-1 25

echo "==> Отказ двух узлов MinIO из четырех (кворум записи потерян)"
load 45 "$OUT/k6.log" &
pid=$!
sleep 12
docker stop otus-storage-minio-2-1 otus-storage-minio-3-1 > /dev/null
sleep 20
docker start otus-storage-minio-2-1 otus-storage-minio-3-1 > /dev/null
wait "$pid"
summary "$OUT/k6.log"
echo

echo "==> Состояние хранилищ после тестов"
sleep 20
docker exec otus-storage-minio-1-1 mc admin info otus 2>/dev/null | grep -E '^●|Drives' | sed 's/^/    /' 
docker exec otus-storage-namenode-1 hdfs dfsadmin -report 2>/dev/null | sed -n '1,8p' | sed 's/^/    /'
docker exec otus-storage-cassandra-1-1 nodetool status 2>/dev/null | sed -n '/^--/,$p' | sed 's/^/    /'
echo -n "    Elasticsearch: "; curl -s localhost:9200/_cluster/health | sed 's/^/    /'
echo
echo "Готово. Логи прогонов: $OUT"
