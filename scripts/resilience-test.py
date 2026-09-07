#!/usr/bin/env python3
"""Проверки на отдельном стенде otus-resilience; результаты сохраняются в JSON."""
import json
import os
import random
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts/resilience"
COMPOSE = ["docker", "compose", "-f", str(ROOT / "resilience/docker-compose.yml")]
API = "http://127.0.0.1:18080"
PROM = "http://127.0.0.1:19090"
ES = "http://127.0.0.1:19200"
JAEGER = "http://127.0.0.1:16687"
RUN = "устойчивость" + uuid.uuid4().hex
RESULT = {"run": RUN, "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
          "scenarios": [], "samples": [], "latencies_ms": [], "ids": [], "http_retries": []}
START = time.monotonic()


def compose(*args):
    return subprocess.check_output(COMPOSE + list(args), text=True, stderr=subprocess.STDOUT)


def sql(query):
    return compose("exec", "-T", "postgres", "psql", "-U", "otus", "-d", "otus", "-Atc", query).strip()


def request(url, data=None, headers=None, method=None):
    req = urllib.request.Request(url, data=None if data is None else json.dumps(data).encode(),
                                 headers={"Content-Type": "application/json", **(headers or {})}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


def wait_for(description, check, timeout=120):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = check()
            if last:
                return last
        except (OSError, ValueError, subprocess.CalledProcessError) as error:
            last = str(error)
        time.sleep(1)
    raise AssertionError(f"Не дождались: {description}; последнее значение: {last}")


def prom(query):
    status, body = request(PROM + "/api/v1/query?" + urllib.parse.urlencode({"query": query}))
    assert status == 200 and body["status"] == "success", body
    return body["data"]["result"]


def metric(query):
    return sum(float(row["value"][1]) for row in prom(query))


def assert_final_state():
    rows = json.loads(sql("SELECT json_agg(json_build_object('id',id,'text',text,'checksum',text_hash)) "
                          f"FROM messages WHERE text='{RUN}'"))
    expected = {row["id"]: row for row in rows}
    assert len(rows) == len(RESULT["ids"]) == len(expected), "Повторы создали лишние сообщения в PostgreSQL"
    assert set(expected) == set(RESULT["ids"]), "Набор сообщений после восстановления изменился"
    ids = ",".join(str(x) for x in expected)
    assert int(sql(f"SELECT count(*) FROM message_outbox WHERE message_id IN ({ids})")) == len(expected), "Потеряны или продублированы задания outbox"
    assert int(sql(f"SELECT count(*) FROM message_outbox o JOIN messages m ON m.id=o.message_id "
                   f"WHERE m.id IN ({ids}) AND (o.payload->>'text' IS DISTINCT FROM m.text "
                   "OR o.payload->>'checksum' IS DISTINCT FROM m.text_hash)")) == 0, "Данные outbox отличаются от сообщений"
    status, body = request(ES + "/otus-resilience/_mget", {"ids": RESULT["ids"]})
    assert status == 200 and len(body["docs"]) == len(expected), "Не удалось сверить документы поиска"
    for doc in body["docs"]:
        assert doc.get("found"), "После восстановления документ отсутствует в поиске"
        source = doc["_source"]
        assert {field: source[field] for field in ("id", "text", "checksum")} == expected[source["id"]], "Поисковая проекция содержит неверные данные"


def pending():
    return int(sql("SELECT count(*) FROM message_outbox WHERE delivered_at IS NULL"))


def sample(phase):
    value = pending()
    RESULT["samples"].append({"seconds": round(time.monotonic() - START, 2), "pending": value, "phase": phase})
    return value


def created(phase, n):
    for _ in range(n):
        key = uuid.uuid4().hex
        begin = time.monotonic()
        for attempt in range(3):
            try:
                status, message = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": key})
            except OSError as error:
                status, message = 0, str(error)
            if status not in (0, 502, 503, 504) or attempt == 2:
                break
            # При смене DNS ответ может потеряться. Повторяем тот же ключ, без новой операции.
            RESULT["http_retries"].append({"phase": phase, "status": status, "attempt": attempt + 1})
            delay = 0.1 * 2 ** attempt
            time.sleep(random.uniform(delay / 2, delay))
        elapsed = (time.monotonic() - begin) * 1000
        assert status == 201, (status, message)
        RESULT["latencies_ms"].append({"phase": phase, "ms": round(elapsed, 2)})
        RESULT["ids"].append(message["id"])
    sample(phase)


def passed(name, **details):
    RESULT["scenarios"].append({"name": name, "status": "пройден", **details})
    print(f"ПРОЙДЕНО: {name}", flush=True)


def search_count():
    query = {"query": {"ids": {"values": RESULT["ids"]}}}
    status, body = request(ES + "/otus-resilience/_count", query)
    return body["count"] if status == 200 else -1


def alerts():
    return request(PROM + "/api/v1/alerts")[1]["data"]["alerts"]


def traces():
    status, body = request(JAEGER + "/api/traces?" + urllib.parse.urlencode({"service": "otus-app", "limit": "100"}))
    return body.get("data", []) if status == 200 else []


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    wait_for("готовность API", lambda: request(API + "/ready")[0] == 200)
    wait_for("все цели Prometheus", lambda: len([x for x in prom('up{job=~"app|consumer"}') if x["value"][1] == "1"]) == 4)
    created("исходное состояние", 20)
    wait_for("доставка исходных событий", lambda: sample("исходное состояние") == 0)
    wait_for("поисковая проекция", lambda: search_count() == len(RESULT["ids"]))
    passed("Исходная доставка и четыре цели Prometheus")

    key = uuid.uuid4().hex
    first = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": key})[1]
    second = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": key})[1]
    assert first["id"] == second["id"], "Повтор создал дубликат"
    assert request(API + "/messages", {"text": "другой текст"}, {"Idempotency-Key": key})[0] == 409
    RESULT["ids"].append(first["id"])
    passed("Повтор команды и конфликт Idempotency-Key")

    pg_retries = metric('otus_retry_attempts_total{dependency="postgres",operation="outbox_claim"}')
    compose("stop", "postgres")
    failed_key = uuid.uuid4().hex
    status, _ = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": failed_key})
    assert status == 500, "Запись без PostgreSQL должна завершаться ошибкой"
    assert request(API + "/health")[0] == 200, "Отказ БД остановил приложение"
    assert request(API + "/ready")[0] == 503, "Readiness не обнаружил отказ БД"
    wait_for("метрика повторов после отказа PostgreSQL", lambda: metric('otus_retry_attempts_total{dependency="postgres",operation="outbox_claim"}') > pg_retries)
    compose("start", "postgres")
    wait_for("PostgreSQL восстановился", lambda: request(API + "/ready")[0] == 200)
    status, recovered = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": failed_key})
    assert status == 201, "Команда не выполнилась после восстановления БД"
    RESULT["ids"].append(recovered["id"])
    repeated = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": failed_key})[1]
    assert repeated["id"] == recovered["id"], "Повтор после отказа БД создал дубликат"
    assert request(API + "/messages", {"text": RUN}, {"Idempotency-Key": key})[1]["id"] == first["id"], "Перезапуск БД потерял ранее сохраненный ключ"
    passed("Отказ PostgreSQL, восстановление и повтор команды без дубликатов")

    redis_trips = metric('otus_circuit_breaker_trips_total{dependency="redis"}')
    redis_rejections = metric('otus_circuit_breaker_calls_total{dependency="redis",result="rejected"}')
    compose("stop", "redis")
    created("недоступен Redis", 10)
    assert request(API + "/messages", {"text": RUN}, {"Idempotency-Key": key})[1]["id"] == first["id"], "Отказ Redis нарушил идемпотентность"
    wait_for("открытие circuit breaker Redis", lambda: metric('otus_circuit_breaker_trips_total{dependency="redis"}') > redis_trips)
    wait_for("отклонения circuit breaker Redis", lambda: metric('otus_circuit_breaker_calls_total{dependency="redis",result="rejected"}') > redis_rejections)
    compose("start", "redis")

    def redis_recovered():
        for _ in range(2):
            request(API + "/hello")
        return len(prom('otus_circuit_breaker_state{dependency="redis",job="app"} == 0')) == 2

    wait_for("обе цепи Redis закрылись после пробного запроса", redis_recovered)
    passed("Отказ Redis: прием команд продолжается, circuit breaker восстанавливается")

    compose("stop", "app-1")
    created("отказ реплики приложения", 10)
    compose("start", "app-1")
    passed("Запись через балансировщик при отказе одной реплики")

    compose("stop", "nats-2", "nats-3")
    created("потеря кворума брокера", 20)
    assert pending() >= 20, "Задания не сохранились во время отказа брокера"
    compose("restart", "app-1", "app-2")
    wait_for("API после перезапуска без кворума брокера", lambda: request(API + "/ready")[0] == 200)
    created("старт без кворума брокера", 10)
    firing = wait_for("алерт задержки outbox", lambda: next((a for a in alerts() if a["labels"]["alertname"] == "OutboxDeliveryDelayed" and a["state"] == "firing"), None))
    (OUT / "alert-firing.json").write_text(json.dumps(firing, ensure_ascii=False, indent=2))
    sample("алерт сработал")
    outage_latencies = [x["ms"] for x in RESULT["latencies_ms"] if "кворума" in x["phase"]]
    assert max(outage_latencies) < 2000, "Брокер задерживает HTTP-команду более двух секунд"
    passed("Потеря кворума и запуск приложения без брокера", max_write_ms=max(outage_latencies), pending=pending())
    passed("Prometheus обнаружил задержку outbox")

    begin = time.monotonic()
    compose("start", "nats-2", "nats-3")
    wait_for("очередь после восстановления кворума", lambda: sample("восстановление брокера") == 0, 180)
    wait_for("все события в поиске", lambda: search_count() == len(RESULT["ids"]), 120)
    passed("Доставка всех событий после восстановления", recovery_seconds=round(time.monotonic() - begin, 2))
    wait_for("сброс алерта outbox", lambda: not any(a["labels"]["alertname"] == "OutboxDeliveryDelayed" for a in alerts()))

    compose("stop", "consumer-1", "consumer-2")
    created("остановлены потребители", 10)
    wait_for("события приняты брокером без потребителей", lambda: sample("остановлены потребители") == 0)
    assert search_count() < len(RESULT["ids"]), "Проекция неожиданно обновилась без потребителей"
    compose("start", "consumer-1", "consumer-2")
    wait_for("проекция после перезапуска потребителей", lambda: search_count() == len(RESULT["ids"]))
    passed("Брокер сохраняет события при остановке всех потребителей")

    es_trips = metric('otus_circuit_breaker_trips_total{dependency="elasticsearch"}')
    exhausted = metric('otus_retry_exhausted_total{dependency="elasticsearch"}')
    compose("stop", "elasticsearch")
    created("недоступен поиск", 5)
    wait_for("ошибки проекции видны в метриках", lambda: bool(prom('sum(otus_broker_consumed_total{result="error"}) > 0')))
    wait_for("исчерпание повторов Elasticsearch", lambda: metric('otus_retry_exhausted_total{dependency="elasticsearch"}') > exhausted)
    wait_for("открытие circuit breaker Elasticsearch", lambda: metric('otus_circuit_breaker_trips_total{dependency="elasticsearch"}') > es_trips)
    compose("start", "elasticsearch")
    wait_for("поиск восстановился без потерь", lambda: search_count() == len(RESULT["ids"]), 180)
    wait_for("закрытие circuit breaker Elasticsearch", lambda: not prom('otus_circuit_breaker_state{dependency="elasticsearch"} != 0'))
    passed("Повтор обработки после отказа Elasticsearch")

    # Повторно выдаем только задания данного прогона, имитируя потерю фиксации ACK.
    ids = ",".join(str(x) for x in RESULT["ids"])
    sql(f"UPDATE message_outbox SET delivered_at=NULL, lease_token=NULL, available_at=now() WHERE message_id IN ({ids})")
    wait_for("повторная доставка", lambda: sample("повторная доставка") == 0, 180)
    assert search_count() == len(RESULT["ids"]), "Повтор доставки изменил число документов"
    assert_final_state()
    passed("Повтор доставки сохраняет число документов", expected=len(RESULT["ids"]), actual=search_count())
    passed("PostgreSQL, outbox и Elasticsearch содержат одинаковые данные без дубликатов")

    trace_data = wait_for("трейсы outbox в Jaeger", lambda: next((t for t in traces() if any(s["operationName"] == "outbox.publish" for s in t["spans"])), None))
    (OUT / "trace.json").write_text(json.dumps(trace_data, ensure_ascii=False, indent=2))
    logs = compose("logs", "--no-color", "app-1", "app-2")
    assert '"msg":"событие outbox доставлено"' in logs, "Нет структурных логов доставки"
    (OUT / "delivery.log").write_text("\n".join(line for line in logs.splitlines() if "outbox" in line))
    (OUT / "metrics.json").write_text(json.dumps(prom('{__name__=~"otus_outbox_.*|otus_broker_consumed_total|otus_retry_.*|otus_circuit_breaker_.*"}'), ensure_ascii=False, indent=2))
    passed("JSON-логи, метрики и трейсы Jaeger")
    RESULT["status"] = "пройден"
    RESULT["duration_seconds"] = round(time.monotonic() - START, 2)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        RESULT["status"] = "ошибка"
        RESULT["error"] = str(error)
        raise
    finally:
        OUT.mkdir(parents=True, exist_ok=True)
        (OUT / "results.json").write_text(json.dumps(RESULT, ensure_ascii=False, indent=2))
        # Возвращаем остановленные сервисы даже при неуспешной проверке.
        if os.environ.get("RESILIENCE_KEEP_FAILURE") != "1":
            subprocess.run(COMPOSE + ["start"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
