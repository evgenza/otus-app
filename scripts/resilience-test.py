#!/usr/bin/env python3
"""Проверки на отдельном стенде otus-resilience; результаты сохраняются в JSON."""
import json
import os
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
          "scenarios": [], "samples": [], "latencies_ms": [], "ids": []}
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
        status, message = request(API + "/messages", {"text": RUN}, {"Idempotency-Key": key})
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

    compose("stop", "elasticsearch")
    created("недоступен поиск", 5)
    wait_for("ошибки проекции видны в метриках", lambda: bool(prom('sum(otus_broker_consumed_total{result="error"}) > 0')))
    compose("start", "elasticsearch")
    wait_for("поиск восстановился без потерь", lambda: search_count() == len(RESULT["ids"]), 180)
    passed("Повтор обработки после отказа Elasticsearch")

    # Повторно выдаем только задания данного прогона, имитируя потерю фиксации ACK.
    ids = ",".join(str(x) for x in RESULT["ids"])
    sql(f"UPDATE message_outbox SET delivered_at=NULL, lease_token=NULL, available_at=now() WHERE message_id IN ({ids})")
    wait_for("повторная доставка", lambda: sample("повторная доставка") == 0, 180)
    assert search_count() == len(RESULT["ids"]), "Повтор доставки изменил число документов"
    passed("Повтор доставки сохраняет число документов", expected=len(RESULT["ids"]), actual=search_count())

    trace_data = wait_for("трейсы outbox в Jaeger", lambda: next((t for t in traces() if any(s["operationName"] == "outbox.publish" for s in t["spans"])), None))
    (OUT / "trace.json").write_text(json.dumps(trace_data, ensure_ascii=False, indent=2))
    logs = compose("logs", "--no-color", "app-1", "app-2")
    assert '"msg":"событие outbox доставлено"' in logs, "Нет структурных логов доставки"
    (OUT / "delivery.log").write_text("\n".join(line for line in logs.splitlines() if "outbox" in line))
    (OUT / "metrics.json").write_text(json.dumps(prom('{__name__=~"otus_outbox_.*|otus_broker_consumed_total"}'), ensure_ascii=False, indent=2))
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
