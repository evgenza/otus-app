#!/bin/bash
# Задание со звездочкой: теги и поиск по ним, частичная загрузка,
# версионирование и multipart-загрузка большого файла в S3.
# Запускать из корня репозитория при поднятом стенде хранилищ:
#   bash scripts/s3-features-test.sh
set -u

BASE=${BASE:-http://localhost:8080}
MC="docker exec otus-storage-minio-1-1 mc"
# Алиас с ключами: без него mc ходит в бакет анонимно и получает отказ.
$MC alias set otus http://localhost:9000 minioadmin minioadmin > /dev/null 2>&1
BIG_MB=${BIG_MB:-200}
TMP=$(mktemp -d)

hr() { echo; echo "== $1"; echo; }

hr "1. Теги и поиск по ним"
for spec in "отчет-квартал.txt kind=report,quarter=q3,owner=evgenza" \
            "отчет-год.txt kind=report,quarter=year,owner=evgenza" \
            "лог-сервиса.txt kind=log,owner=system"; do
  set -- $spec
  curl -s -X POST "$BASE/files?name=$1&tags=$2" --data-binary "содержимое $1" \
    -H 'Content-Type: text/plain' -o /dev/null -w "    загружен $1 [%{http_code}]\n"
done
sleep 2
echo
echo "    поиск по тегу kind=report через индекс Elasticsearch:"
curl -s "$BASE/files?tag=kind=report&source=es" | head -c 600; echo
echo
echo "    тот же поиск перебором бакета средствами S3:"
curl -s "$BASE/files?tag=kind=report&source=s3" | head -c 600; echo
echo
echo "    теги объекта глазами MinIO:"
$MC tag list otus/otus-files/отчет-квартал.txt 2>&1 | sed 's/^/    /'
echo
echo "    замена тегов через API:"
curl -s -X PUT "$BASE/files/отчет-квартал.txt/tags" -d '{"kind":"archive","owner":"evgenza"}' | sed 's/^/    /'
echo
$MC tag list otus/otus-files/отчет-квартал.txt 2>&1 | sed 's/^/    /'

hr "2. Версионирование"
for v in "первая версия" "вторая версия" "третья версия"; do
  curl -s -X POST "$BASE/files?name=версии.txt" --data-binary "$v" \
    -o /dev/null -w "    записана $v [%{http_code}]\n"
done
echo
echo "    список версий:"
curl -s "$BASE/files/версии.txt/versions" | sed 's/},{/},\n    {/g' | sed 's/^/    /'
echo
latest=$(curl -s "$BASE/files/версии.txt")
echo "    текущее содержимое: $latest"
old=$(curl -s "$BASE/files/версии.txt/versions" | grep -o '"version_id":"[^"]*"' | tail -1 | cut -d'"' -f4)
echo "    самая старая версия ($old): $(curl -s "$BASE/files/версии.txt?version=$old")"
echo
echo "    состояние бакета в MinIO:"
$MC ls --versions otus/otus-files/версии.txt 2>&1 | sed 's/^/    /'

hr "3. Частичная загрузка (Range)"
curl -s -X POST "$BASE/files?name=диапазон.txt" \
  --data-binary "0123456789abcdefghijklmnopqrstuvwxyz" -o /dev/null
echo "    весь объект:        $(curl -s "$BASE/files/диапазон.txt")"
echo "    байты 0-9:          $(curl -s -H 'Range: bytes=0-9' "$BASE/files/диапазон.txt")"
echo "    байты 10-19:        $(curl -s -H 'Range: bytes=10-19' "$BASE/files/диапазон.txt")"
echo "    байты 26- (хвост):  $(curl -s -H 'Range: bytes=26-' "$BASE/files/диапазон.txt")"
echo "    заголовки ответа:"
curl -s -D - -o /dev/null -H 'Range: bytes=10-19' "$BASE/files/диапазон.txt" | sed -n '1p;/Content-Range/p;/Content-Length/p' | sed 's/^/    /'

hr "4. Огромный файл: multipart-загрузка ${BIG_MB} МБ"
dd if=/dev/urandom of="$TMP/big.bin" bs=1M count="$BIG_MB" status=none
echo "    исходный файл: $(du -h "$TMP/big.bin" | cut -f1), sha256 $(sha256sum "$TMP/big.bin" | cut -c1-16)"
start=$(date +%s%N)
curl -s -X POST "$BASE/files?name=big.bin&tags=kind=bigdata" \
  -H 'Content-Type: application/octet-stream' --data-binary "@$TMP/big.bin" \
  -o "$TMP/upload.json" -w "    загрузка [%{http_code}] за %{time_total}с, %{speed_upload} Б/с\n"
sed 's/^/    /' "$TMP/upload.json"; echo
echo "    как объект выглядит в MinIO (число частей видно по ETag):"
$MC stat otus/otus-files/big.bin 2>&1 | sed -n '1,12p' | sed 's/^/    /'
echo
echo "    читаю только 1 МБ из середины файла:"
offset=$(( BIG_MB / 2 * 1024 * 1024 ))
curl -s -H "Range: bytes=$offset-$((offset + 1048575))" "$BASE/files/big.bin" -o "$TMP/part.bin" \
  -w "    получено %{size_download} байт за %{time_total}с\n"
dd if="$TMP/big.bin" bs=1 skip="$offset" count=1048576 of="$TMP/expect.bin" status=none
if cmp -s "$TMP/part.bin" "$TMP/expect.bin"; then
  echo "    кусок совпадает с оригиналом"
else
  echo "    ОШИБКА: кусок не совпадает с оригиналом"
fi
echo
echo "    скачиваю целиком и сверяю контрольную сумму:"
curl -s "$BASE/files/big.bin" -o "$TMP/back.bin" -w "    скачано %{size_download} байт за %{time_total}с\n"
if cmp -s "$TMP/big.bin" "$TMP/back.bin"; then
  echo "    файл вернулся байт в байт"
else
  echo "    ОШИБКА: файл отличается от исходного"
fi

hr "5. Тот же большой файл в HDFS"
start=$(date +%s%N)
curl -s -X POST "$BASE/files?name=big.bin&backend=hdfs" \
  -H 'Content-Type: application/octet-stream' --data-binary "@$TMP/big.bin" \
  -o "$TMP/hdfs.json" -w "    загрузка [%{http_code}] за %{time_total}с, %{speed_upload} Б/с\n"
sed 's/^/    /' "$TMP/hdfs.json"; echo
echo "    как файл разложен по блокам и датанодам:"
docker exec otus-storage-namenode-1 hdfs fsck /otus/files/big.bin -files -blocks -locations 2>/dev/null |
  sed -n '/^\/otus/,/^Status/p' | head -12 | sed 's/^/    /'
echo
curl -s -H "Range: bytes=$offset-$((offset + 1048575))" "$BASE/files/big.bin?backend=hdfs" -o "$TMP/hpart.bin" \
  -w "    частичное чтение из HDFS: %{size_download} байт за %{time_total}с\n"
cmp -s "$TMP/hpart.bin" "$TMP/expect.bin" && echo "    кусок из HDFS совпадает с оригиналом" || echo "    ОШИБКА: кусок из HDFS не совпадает"

echo
echo "Готово. Временные файлы: $TMP"
