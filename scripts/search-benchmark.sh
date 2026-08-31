#!/bin/bash
# Сравнение поиска по тексту: Elasticsearch против LIKE в PostgreSQL и
# перебора таблицы в Cassandra. Один и тот же запрос, один и тот же набор
# данных, замер через API приложения.
# Запускать из корня репозитория при поднятом стенде хранилищ:
#   bash scripts/search-benchmark.sh
set -u

BASE=${BASE:-http://localhost:8080}
FILL=${FILL:-20000}
QUERIES=${QUERIES:-"отчет квартальный доставка сообщение"}
REPEATS=${REPEATS:-5}

WORDS=("квартальный отчет по продажам" "ежедневный отчет о доставке"
       "сообщение об ошибке доставки" "служебная запись без смысла"
       "отчет об инциденте в проде" "уведомление о новом сообщении")

echo "== Наполняю базу: $FILL сообщений"
start=$(date +%s)
for i in $(seq 1 "$FILL"); do
  text="${WORDS[$((RANDOM % ${#WORDS[@]}))]} № $i"
  curl -s -o /dev/null -X POST "$BASE/messages" -H 'Content-Type: application/json' \
    -d "{\"text\":\"$text\"}" &
  if [ $((i % 50)) -eq 0 ]; then wait; fi
done
wait
echo "    записано за $(( $(date +%s) - start ))с, жду разбора очереди воркером"
sleep 30

count_pg=$(docker exec otus-storage-postgres-1 psql -U otus -d otus -tAc 'SELECT count(*) FROM messages')
count_cas=$(docker exec otus-storage-cassandra-1-1 cqlsh -k otus -e 'SELECT count(*) FROM events_by_id;' 2>/dev/null | sed -n '4p' | tr -d ' ')
count_es=$(curl -s "localhost:9200/otus-messages/_count" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')
echo "    строк: PostgreSQL $count_pg, Cassandra $count_cas, Elasticsearch $count_es"
echo

printf "%-14s %-14s %10s %10s %10s\n" "запрос" "движок" "найдено" "мс (среднее)" "просмотрено"
for q in $QUERIES; do
  for engine in elasticsearch postgres cassandra; do
    total=0; found=0; scanned=0
    for _ in $(seq 1 "$REPEATS"); do
      body=$(curl -s "$BASE/search?q=$q&engine=$engine&limit=10")
      ms=$(echo "$body" | grep -o '"elapsed_ms":[0-9]*' | cut -d: -f2)
      found=$(echo "$body" | grep -o '"total":[0-9]*' | cut -d: -f2)
      scanned=$(echo "$body" | grep -o '"scanned":[0-9]*' | cut -d: -f2)
      total=$((total + ${ms:-0}))
    done
    printf "%-14s %-14s %10s %10s %10s\n" "$q" "$engine" "${found:-0}" "$((total / REPEATS))" "${scanned:-—}"
  done
done

echo
echo "== Морфология: Elasticsearch ищет по основе слова"
for q in сообщение сообщения сообщений; do
  echo -n "    '$q': "
  curl -s "$BASE/search?q=$q&engine=elasticsearch&limit=1" | grep -o '"total":[0-9]*' | cut -d: -f2
done
echo "    тот же запрос в PostgreSQL через LIKE:"
for q in сообщение сообщения сообщений; do
  echo -n "    '$q': "
  curl -s "$BASE/search?q=$q&engine=postgres&limit=1" | grep -o '"total":[0-9]*' | cut -d: -f2
done

echo
echo "== План запроса в PostgreSQL"
docker exec otus-storage-postgres-1 psql -U otus -d otus -c \
  "EXPLAIN ANALYZE SELECT id FROM messages WHERE text ILIKE '%отчет%' LIMIT 10" | sed 's/^/    /'
