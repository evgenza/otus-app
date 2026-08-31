#!/bin/bash
# Задание со звездочкой: запросы к Cassandra как к БД.
# Что умеет и чего не умеет CQL, чем платит за запрос не по ключу,
# как ведут себя уровни консистентности при отказе узла.
# Запускать из корня репозитория при поднятом стенде хранилищ:
#   bash scripts/cassandra-queries-test.sh
set -u

CQL="docker exec otus-storage-cassandra-1-1 cqlsh"
DAY=$(date -u +%F)

q() { $CQL -k otus -e "$1" 2>&1 | sed 's/^/    /'; }

# Время меряем не секундомером снаружи (старт cqlsh стоит сотни
# миллисекунд и смазал бы картину), а трассировкой самой Cassandra:
# TRACING ON печатает, сколько микросекунд запрос шел на сервере.
timed() { # timed <подпись> <запрос>
  local out us
  out=$($CQL -k otus -e "TRACING ON; $2" 2>&1)
  echo "$out" | grep -v -E 'Tracing session|Request complete|^ *activity|^ *-+\+|\| +[0-9]+\.[0-9]+\.|^ *$' | head -12 | sed 's/^/    /'
  us=$(echo "$out" | grep 'Request complete' | grep -oE '\| +[0-9]+ +\| [0-9.]+$' | grep -oE '[0-9]+' | head -1)
  if [ -n "$us" ]; then
    echo "    [$1: $((us / 1000)).$(( (us % 1000) / 100 )) мс на сервере]"
  else
    echo "    [$1: время не получено]"
  fi
  echo
}

# Индекс из прошлого прогона сломал бы демонстрацию: запрос по
# неключевой колонке стал бы разрешен еще до раздела про индексы.
q "DROP INDEX IF EXISTS events_by_producer;" > /dev/null

echo "== Схема"
q "DESCRIBE TABLE message_events;"
echo
q "DESCRIBE TABLE events_by_id;"

echo
echo "== Сколько строк накоплено"
timed "count(*) по всей таблице" "SELECT count(*) FROM events_by_id;"

echo "== 1. Запрос по ключу партиции: то, ради чего Cassandra и нужна"
timed "лента за сутки, LIMIT 5" \
  "SELECT id, producer, created_at FROM message_events WHERE day = '$DAY' LIMIT 5;"

echo "== 2. Запрос по первичному ключу денормализованной таблицы"
id=$($CQL -k otus -e "SELECT id FROM events_by_id LIMIT 1;" 2>/dev/null | sed -n '4p' | tr -d ' ')
timed "выборка по id = $id" "SELECT id, text, producer FROM events_by_id WHERE id = $id;"

echo "== 3. Запрос по неключевому полю: без ALLOW FILTERING запрещен"
q "SELECT id FROM events_by_id WHERE producer = 'kafka' LIMIT 5;"
echo
timed "то же с ALLOW FILTERING (перебор всех партиций)" \
  "SELECT id FROM events_by_id WHERE producer = 'kafka' LIMIT 5 ALLOW FILTERING;"

echo "== 4. Вторичный индекс на колонке с низкой кардинальностью"
q "CREATE INDEX IF NOT EXISTS events_by_producer ON events_by_id (producer);"
echo "    жду, пока индекс построится:"
for _ in $(seq 1 30); do
  built=$($CQL -e "SELECT count(*) FROM system.\"IndexInfo\" WHERE table_name = 'otus' AND index_name = 'events_by_producer';" 2>/dev/null | sed -n '4p' | tr -d ' ')
  [ "$built" = "1" ] && break
  sleep 2
done
echo "    построен: ${built:-нет}"
timed "тот же запрос по вторичному индексу" \
  "SELECT id FROM events_by_id WHERE producer = 'kafka' LIMIT 5;"

echo "== 5. Диапазон по кластерному ключу внутри партиции - можно"
timed "события за последний час" \
  "SELECT id, created_at FROM message_events WHERE day = '$DAY' AND created_at > '$(date -u -d '1 hour ago' +'%F %H:%M:%S')' LIMIT 5;"

echo "== 6. Чего в CQL нет"
echo "    JOIN двух таблиц:"
q "SELECT e.id FROM events_by_id e JOIN message_events m ON e.id = m.id LIMIT 1;"
echo
echo "    группировка по неключевому полю:"
q "SELECT producer, count(*) FROM events_by_id GROUP BY producer;"
echo
echo "    OR в условии:"
q "SELECT id FROM events_by_id WHERE id = 1 OR id = 2;"

echo
echo "== 7. Запись: INSERT это upsert, уникальность только через LWT"
q "INSERT INTO events_by_id (id, text, producer, created_at) VALUES (-1, 'первая запись', 'test', toTimestamp(now()));"
q "INSERT INTO events_by_id (id, text, producer, created_at) VALUES (-1, 'вторая запись затерла первую', 'test', toTimestamp(now()));"
q "SELECT id, text FROM events_by_id WHERE id = -1;"
echo
echo "    условная вставка (Paxos, 4 круга согласования):"
q "INSERT INTO events_by_id (id, text, producer, created_at) VALUES (-1, 'третья', 'test', toTimestamp(now())) IF NOT EXISTS;"

echo
echo "== 8. Уровни консистентности при живом кластере"
for cl in ONE QUORUM ALL; do
  echo "    CONSISTENCY $cl:"
  $CQL -k otus -e "CONSISTENCY $cl; SELECT count(*) FROM events_by_id;" 2>&1 |
    grep -vE '^\s*$|Consistency level set' | head -4 | sed 's/^/      /'
done

echo
echo "== 9. Те же уровни при погашенном узле cassandra-3"
docker stop otus-storage-cassandra-3-1 > /dev/null
sleep 25
docker exec otus-storage-cassandra-1-1 nodetool status 2>/dev/null | sed -n '/^--/,$p' | sed 's/^/    /'
echo
for cl in ONE QUORUM ALL; do
  echo "    CONSISTENCY $cl:"
  $CQL -k otus -e "CONSISTENCY $cl; SELECT id FROM events_by_id WHERE id = $id;" 2>&1 |
    grep -vE '^\s*$|Consistency level set' | head -4 | sed 's/^/      /'
done
echo
echo "    запись с CONSISTENCY ALL при двух живых узлах из трех:"
$CQL -k otus -e "CONSISTENCY ALL; INSERT INTO events_by_id (id, text, producer, created_at) VALUES (-2, 'запись при отказе', 'test', toTimestamp(now()));" 2>&1 | sed 's/^/      /'
echo
echo "    та же запись с QUORUM:"
$CQL -k otus -e "CONSISTENCY QUORUM; INSERT INTO events_by_id (id, text, producer, created_at) VALUES (-2, 'запись при отказе', 'test', toTimestamp(now()));" 2>&1 | sed 's/^/      /'
echo "      (пустой вывод означает успешную запись)"

docker start otus-storage-cassandra-3-1 > /dev/null
echo
echo "Узел возвращен. Чиним расхождение: nodetool repair отдаст догоняющему узлу пропущенные записи."
