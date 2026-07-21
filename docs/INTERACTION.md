# Протокол проверки взаимодействия между сервисами

Домашнее задание: RESTful-приложение на роутере, контроль доступа через
middleware, Swagger, расширенное интеграционное тестирование, статусная
страница на шаблонизаторе, gRPC с разными видами стриминга и (задание со
звёздочкой) прокси-микросервис с метриками трафика и управляемыми помехами.

## Схема взаимодействия

```
клиент ──HTTPS──> nginx ──┬── /            ──mTLS──> app :8080  (REST + Swagger + /status)
                          │                            app :9091  (gRPC)
                          └── /gw/         ────────> gateway :8090
                                                        │
                                    HTTP+JSON: ──mTLS──> chaosproxy :8080 ──mTLS──> app :8080
                                    gRPC:      ──mTLS──────────────────────────────> app :9091
```

- **app** — RESTful API + gRPC-сервер (оба порта под mTLS);
- **gateway** — ходит в app двумя способами: HTTP+JSON через chaos-прокси и
  напрямую по gRPC (мост `/gw/grpc/...`);
- **chaosproxy** — прокси между gateway и app: считает трафик в обе стороны и
  по командам вносит помехи.

## 1. RESTful-приложение на роутере

Роутер — `http.ServeMux` из стандартной библиотеки Go 1.22+ с
method-паттернами: маршруты объявляются как `GET /messages`,
`POST /messages`, метод не тот — роутер сам вернёт 405. Ресурс `messages`
обслуживается по REST: `POST` создаёт, `GET` читает список, статусы
201/400/401/500 по смыслу ([internal/handlers/handlers.go](../internal/handlers/handlers.go)).

## 2. Контроль доступа через middleware

Закрытый раздел — создание сообщений. Middleware
([internal/security/jwt.go](../internal/security/jwt.go)) проверяет JWT из
Keycloak (подпись RS256 по JWKS, срок действия, издателя) и на HTTP, и на
gRPC (интерцепторами — `CreateMessage` и `BatchCreate` закрыты, чтение
открыто):

```
$ curl -s -X POST https://zhemchugovei.duckdns.org/messages -d '{"text":"без токена"}'
{"error":"требуется токен авторизации"}          # 401

$ curl -s -X POST https://zhemchugovei.duckdns.org/messages \
    -H "Authorization: Bearer $TOKEN" -d '{"text":"с токеном"}'
{"id":...,"text":"с токеном","checksum_ok":true} # 201
```

Проверено юнит-тестами (валидный/просроченный/чужой токен, чужой издатель —
`internal/security/jwt_test.go`, gRPC-варианты — `internal/grpcserver/server_test.go`)
и интеграционным тестом с реальной БД (`TestAPIAccessControlWithDatabase`).

## 3. Swagger

Спецификация OpenAPI 3.0 написана руками и вшита в бинарь
([internal/handlers/apidocs/openapi.yaml](../internal/handlers/apidocs/openapi.yaml)),
Swagger UI отдаётся самим приложением:

- <https://zhemchugovei.duckdns.org/swagger/> — интерфейс;
- <https://zhemchugovei.duckdns.org/swagger/openapi.yaml> — спецификация.

В спецификации описаны все маршруты app и gateway, включая gRPC-мост. Для
закрытых маршрутов объявлена схема `bearerAuth`: кнопка **Authorize** →
вставить access_token из Keycloak → запросы уходят с заголовком
`Authorization`. Так из UI корректно тестируются и открытые эндпоинты, и
закрытые (401 без токена, 201 с токеном).

![Swagger UI](screenshots/18-swagger.png)

![Запрос с авторизацией из Swagger](screenshots/19-swagger-auth.png)

![401 без токена из Swagger](screenshots/20-swagger-401.png)

## 4. Интеграционное тестирование

Интеграционный трек (`go test -tags=integration`, job в CI на каждый PR)
расширен:

| Тест | Что проверяет |
|---|---|
| `TestStorageCreateAndList` | запись и чтение через реальный Postgres |
| `TestChecksumDetectsTampering` | подмена данных в обход API ловится по SHA-256 |
| `TestAPIWithDatabase` | HTTP API поверх реальной БД |
| `TestAPIAccessControlWithDatabase` (новый) | 401 без токена, 201 с валидным JWT |
| `TestGRPCWithDatabase` (новый) | client streaming `BatchCreate` + server streaming `ListMessages` с реальной БД |

Плюс юнит-тесты новых частей: статусная страница, Swagger-маршруты, все
четыре вида gRPC-стриминга, авторизация в gRPC (покрытие
`internal/grpcserver` — 84.8%, `internal/handlers` — 85.5%).

## 5. Статусная страница на шаблонизаторе

`GET /status` — HTML-страница на `html/template`
([internal/handlers/status.html](../internal/handlers/status.html)): версия и
дата сборки, аптайм, доступность БД, число сообщений, сходимость контрольных
сумм, состояние mTLS и JWT-авторизации. Страница перерисовывается
шаблонизатором на каждый запрос и сама обновляется каждые 10 секунд.

```
$ curl -s --cacert ca.crt --cert gateway.crt --key gateway.key https://localhost:8080/status
<h1>Состояние сервиса otus-app</h1>
...Аптайм... база данных: доступна ... контрольные суммы: все сходятся
```

![Статусная страница](screenshots/21-status.png)

## 6. gRPC с разными видами стриминга

Контракт — [api/proto/messages.proto](../api/proto/messages.proto), код
сгенерирован `protoc` в `internal/grpcapi`. App поднимает gRPC-сервер на
порту 9091 под тем же mTLS, что и HTTP. Gateway выступает клиентом и
пробрасывает результаты наружу через `/gw/grpc/...` — все четыре вида
взаимодействия можно потрогать обычным curl:

**Unary** — `CreateMessage`:

```
$ curl -X POST .../gw/grpc/messages -d '{"text":"грпц юнари"}'
{"id":1,"text":"грпц юнари","checksum":"dcfc208f...","checksum_ok":true,...}
```

**Server streaming** — `ListMessages`, gateway читает поток и отдаёт NDJSON
(по строке на сообщение по мере прихода):

```
$ curl .../gw/grpc/messages
{"id":4,"text":"пакет три","checksum_ok":true,...}
{"id":3,"text":"пакет два","checksum_ok":true,...}
{"id":2,"text":"пакет раз","checksum_ok":true,...}
```

**Client streaming** — `BatchCreate`, каждый текст уходит отдельным
сообщением потока, в ответ одна сводка:

```
$ curl -X POST .../gw/grpc/messages/batch -d '{"texts":["пакет раз","пакет два","пакет три"]}'
{"created":3,"ids":[2,3,4]}
```

**Bidirectional streaming** — `Chat`, реплики и ответы ходят по одному
потоку в обе стороны:

```
$ curl -X POST .../gw/grpc/chat -d '{"notes":["привет","как дела"]}'
{"replies":["эхо: привет","эхо: как дела"]}
```

## 7. Сравнение производительности HTTP+JSON и gRPC

Замер — `make bench` (`internal/grpcserver/bench_test.go`): одинаковые
операции против одного и того же хранилища, HTTP через `httptest` +
`encoding/json`, gRPC через реальный TCP-сокет. AMD Ryzen 5 8400F:

| Операция | HTTP+JSON | gRPC | Разница |
|---|---|---|---|
| Создание сообщения (unary) | 55.9 мкс/оп | 63.5 мкс/оп | HTTP на ~12% быстрее |
| Список из 100 сообщений | 394.0 мкс/оп | 216.9 мкс/оп | **gRPC в 1.8 раза быстрее** |

Вывод: на единичных мелких запросах выигрыша у gRPC нет — накладные расходы
HTTP/2-фреймов съедают экономию на сериализации, HTTP+JSON даже чуть быстрее.
Но чем больше данных в ответе, тем сильнее protobuf выигрывает у JSON: на
списке из 100 сообщений gRPC почти вдвое быстрее. Для внутренних вызовов с
заметными объёмами данных и для стриминга gRPC — правильный выбор, для
редких мелких запросов разницы на практике нет.

## 8. Задание со звёздочкой: chaos-прокси

Микросервис [cmd/chaosproxy](../cmd/chaosproxy/main.go) встроен в цепочку
`gateway → chaosproxy → app` (mTLS на обоих плечах). Управляется по
внутреннему порту 8091 (наружу не публикуется), там же `/metrics` для
Prometheus.

**Метрики трафика в обе стороны:**

```
$ curl -s localhost:8091/metrics | grep otus_proxy
otus_proxy_requests_total{result="ok"} 6
otus_proxy_requests_total{result="dropped"} 1
otus_proxy_traffic_bytes_total{direction="to_app"} 34
otus_proxy_traffic_bytes_total{direction="from_app"} 187
otus_proxy_faults_total{type="delay"} 1
otus_proxy_faults_total{type="drop"} 1
otus_proxy_faults_total{type="repeat"} 1
```

**Управляющие команды** (`POST /control`, каждая команда задаёт полный набор
помех, `{}` — всё выключить):

задержка каждого запроса:

```
$ curl -X POST localhost:8091/control -d '{"delay_ms":1000}'
$ time curl -s -o /dev/null localhost:8090/gw/messages
real    0m1,008s          # без помехи — ~5 мс
```

повтор запросов (write-запрос уходит в app дважды — видно по данным):

```
$ curl -X POST localhost:8091/control -d '{"repeat":true}'
$ curl -X POST localhost:8090/gw/messages -d '{"text":"задублируется"}'
# сообщений было: 5, стало: 7 — прокси повторил запрос
```

обрыв запросов:

```
$ curl -X POST localhost:8091/control -d '{"drop_rate":1}'
$ curl localhost:8090/gw/messages
{"error":"запрос оборван прокси"}   # 502
```

снижение скорости отдачи:

```
$ curl -X POST localhost:8091/control -d '{"throttle_bps":500}'
$ time curl -s -o /dev/null localhost:8090/gw/messages
real    0m2,513s          # ответ ~1.2 КБ капает по 500 Б/с
```

![Метрики прокси в Prometheus](screenshots/22-proxy-metrics.png)

## Оценка результатов

- REST-приложение работает на стандартном роутере с method-паттернами,
  закрытые разделы защищены middleware — доступ проверяется одинаково для
  HTTP и gRPC.
- Swagger вшит в бинарь, спецификация покрывает оба сервиса, из UI
  тестируются и открытые, и закрытые маршруты.
- gRPC развёрнут со всеми четырьмя видами взаимодействия и живёт под тем же
  mTLS, что и HTTP; наружу выведен мостом через gateway, так что стриминг
  видно обычным браузером/curl.
- Сравнение протоколов дало предсказуемый, но наглядный результат: gRPC
  окупается на объёме и стриминге, а не на единичных мелких вызовах.
- Chaos-прокси оказался удобным инструментом: помехи включаются на лету
  одной командой, эффект сразу виден и в данных, и в метриках. В связке с
  Prometheus/Grafana это готовый стенд для проверки устойчивости.
