# InfraPulse Backend — первый сквозной MVP

Backend принимает сообщения существующего InfraPulse Agent по gRPC, сохраняет
их в PostgreSQL и отдаёт устройства и метрики через HTTP REST API.

```text
Linux Agent / SNMP Collector / Simulator
                 │ AgentService.Connect, protocol v1
                 ▼
         Go Controller + REST API
                 │
                 ▼
             PostgreSQL
                 ▲
                 │
       Frontend → HTTP REST API
```

Реализованы регистрация **до HelloAck**, heartbeat, история сессий, inventory,
telemetry, поиск устройств и история измерений. Команды устройствам не отправляются.
Подробности для frontend: [docs/api.md](docs/api.md).
Следующие этапы backend с учётом уточнений организаторов:
[docs/backend-roadmap.md](docs/backend-roadmap.md).
Машиночитаемый контракт: [internal/api/openapi.json](internal/api/openapi.json),
также доступен по `GET /openapi.json`.
Настраиваемая частота сбора и оценка свежести данных:
[docs/monitoring-intervals.md](docs/monitoring-intervals.md).

## 1. Запуск одной командой через Docker

На Windows установи Docker Desktop и запусти его. На Linux нужен Docker Engine
с Compose. Go на компьютере для этого варианта не требуется.

Открой терминал **в папке InfraPulse-Backend-main**, где находится этот README:

```powershell
Copy-Item .env.example .env
docker compose up --build -d
docker compose logs -f backend
```

На Linux вместо `Copy-Item` используй `cp .env.example .env`.
Первый запуск скачивает образы и зависимости. После запуска появится
`backend ready`. База будет готова до старта backend; миграции применяются самим
backend автоматически. Файлы SQL находятся в `internal/storage/migrations` и
включаются в бинарник, поэтому рабочая папка запуска не влияет на миграции.

Открой в браузере:

- `http://127.0.0.1:8080/healthz` — процесс работает;
- `http://127.0.0.1:8080/readyz` — база доступна;
- `http://127.0.0.1:8080/api/agents` — зарегистрированные агенты;
- `http://127.0.0.1:8080/api/devices` — устройства, пока пустой `items: []`.

Для остановки: `docker compose down`. Данные сохраняются в отдельном Docker
volume. Повторный `up` сохраняет историю. Порты опубликованы только на локальном
компьютере: HTTP 8080, gRPC 9443, PostgreSQL 5432. Не запускай Mock Controller
Agent одновременно на том же порту 9443.

## 2. Проверка с готовым Simulator на Windows или Linux

Сначала запусти backend. Нужен Go **1.27 или новее**, как у Agent. Официальная
загрузка: [go.dev/dl](https://go.dev/dl/). Если Go установлен только что,
переоткрой терминал и проверь `go version`.

Из **папки Agent** собери существующий Simulator:

```powershell
go build -o bin/infrapulse-simulator.exe ./cmd/infrapulse-simulator
```

Для распакованных здесь папок запусти его из той же папки Agent:

```powershell
./bin/infrapulse-simulator.exe --config ../../InfraPulse-Backend-main/configs/agent.simulator.yaml --scenario all --duration 10s --interval 1s --devices 4 --output controller --start 2026-10-05T16:00:00Z
```

На Linux собери `-o bin/infrapulse-simulator` и запусти без `.exe`.
Если у команды Agent и Backend лежат рядом, укажи путь
`../InfraPulse-Backend/configs/agent.simulator.yaml` вместо `../../...`.
Для актуальных дат можно задать `--start` текущим временем RFC3339. Без него
Simulator использует свою фиксированную временную шкалу.

Теперь `GET /api/devices` покажет устройства. Выбери `device_id` из списка;
например, `simulated:device-001`:

```powershell
Invoke-RestMethod 'http://127.0.0.1:8080/api/devices/simulated:device-001/telemetry'
Invoke-RestMethod 'http://127.0.0.1:8080/api/devices/simulated:device-001/metrics/history?limit=20'
```

В данных будет `source: simulated`. Это демонстрационные измерения. После
завершения Simulator его `agent_connected` становится `false`, а карточки и
история остаются в базе.

## 3. Настоящие CPU/RAM/network metrics на Linux

Для локального сбора Agent нужен Linux. В этом примере Agent и backend работают
на **одной Linux-машине**: Agent на хосте, backend через Compose или `go run`.

Из папки Agent:

```sh
go build -o bin/infrapulse-agent ./cmd/infrapulse-agent
./bin/infrapulse-agent validate-config --config ../../InfraPulse-Backend-main/configs/agent.linux.yaml
./bin/infrapulse-agent run --config ../../InfraPulse-Backend-main/configs/agent.linux.yaml
```

Исправь только путь к конфигурации, если папки расположены иначе. Конфигурация
включает CPU/RAM/interfaces каждые 3 секунды, heartbeat каждые 10 секунд,
inventory каждые 5 минут. Она не включает удалённые устройства и команды.
Постоянный `agent_id` хранится в каталоге состояния Agent; при новом запуске
создаётся новый `session_id`. Не удаляй каталог состояния между запусками,
если хочешь сохранить идентичность агента.

Критерий проверки: Agent появляется в `/api/agents`, устройство `local:...` —
в `/api/devices`, а `/api/devices/{id}/telemetry` показывает новые реальные
измерения. Получи ID из списка, не придумывай его вручную.

На Windows можно запустить обычный Agent с `local.enabled: false` и проверить
регистрацию/heartbeat, но локальных Linux-метрик в этом режиме не будет.

## 4. Разработка backend без контейнера Go

Подними только базу и запусти Go-проект локально:

```powershell
docker compose up -d postgres
go run ./cmd/server
```

При настройках `.env.example` дополнительных переменных не требуется.
Файл `.env` читает **Docker Compose**; `go run` его автоматически не загружает.
Если изменил имя базы/пароль/порт, задай `DATABASE_URL` явно:

```powershell
$env:DATABASE_URL='postgres://infrapulse:infrapulse_dev@127.0.0.1:5432/infrapulse?sslmode=disable'
go run ./cmd/server
```

В этой рабочей папке Go также подготовлен отдельно в `../tools/go/bin/go.exe`.
Его можно вызвать вместо `go`, например `../tools/go/bin/go.exe test ./...`.
Папка tools не является частью backend и не нужна в репозитории команды.

### Настройки backend

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL` | локальная база из `.env.example` | строка подключения PostgreSQL |
| `HTTP_ADDRESS` | `127.0.0.1:8080` | HTTP API |
| `GRPC_ADDRESS` | `127.0.0.1:9443` | входящий stream Agent |
| `AGENT_TIMEOUT` | `90s` | свежесть последнего полученного сообщения |
| `DEFAULT_TELEMETRY_INTERVAL` | `3s` | ожидаемый период метрик, 3s..24h |
| `DEFAULT_INVENTORY_INTERVAL` | `5m` | ожидаемый период inventory, 3s..24h |
| `DEFAULT_TOPOLOGY_INTERVAL` | `5m` | ожидаемый период topology, 3s..24h |
| `TLS_CERT_FILE` | пусто | сертификат gRPC-сервера |
| `TLS_KEY_FILE` | пусто | ключ gRPC-сервера |
| `TLS_CLIENT_CA_FILE` | пусто | CA для проверки сертификатов Agent, включает mTLS |

Конфигурация проверяется при старте. Без сертификата gRPC работает в явно
локальном development-режиме. Для подключения с другой машины потребуется
настроить TLS/mTLS на backend и `security.mode: production` у Agent.
HTTP API в MVP предназначен для локальной разработки и не имеет пользовательской
авторизации. Для внешнего доступа понадобятся HTTPS и авторизация.

## 5. Проверки перед передачей команде

```powershell
go fmt ./...
go vet ./...
go test ./...
go build ./cmd/server
docker compose config --quiet
```

По умолчанию тесты PostgreSQL пропускаются с явным сообщением. Для их запуска
подними отдельную тестовую базу и задай:

```powershell
$env:TEST_DATABASE_URL='postgres://infrapulse:infrapulse_dev@127.0.0.1:5432/infrapulse?sslmode=disable'
go test ./... -count=1
```

Тесты создают собственные временные схемы и удаляют их после завершения. Не
используй production-базу для тестов. Проверяются rollback целого batch, дубли,
порядок времени, одинаковые ID у разных агентов, heartbeat, сессии и timeout.
CI в `.github/workflows/check.yml` запускает тесты с PostgreSQL, race detector
и сборку Docker. Он выполнится после загрузки кода в GitHub; здесь commit/push
не выполнялись. Реально выполненные локальные проверки: [docs/validation.md](docs/validation.md).

## 6. Где что лежит

- `cmd/server` — запуск, подключение БД, два сервера, корректная остановка;
- `internal/config` — настройки окружения;
- `internal/controller` — протокол регистрации и приём stream;
- `internal/storage` — SQL, миграции и запросы;
- `internal/api` — REST и OpenAPI;
- `api/proto`, `api/gen` — точные копии контракта и generated sources Agent;
- `configs` — примеры подключения существующего Agent;
- `docs` — контракт frontend, архитектура и результаты проверок.

Передавай команде содержимое **InfraPulse-Backend-main**, а не всю папку проекта
с reference/tools. Сохрани существующий LICENSE. Не добавляй `.env`, state или
собранные `.exe` в Git. Для просмотра своих изменений достаточно `git status`
и `git diff`; commit и push выполняются отдельным осознанным шагом.

## Текущие границы MVP

Один экземпляр Controller, один PostgreSQL. Agents/devices/telemetry — отдельные
сущности; история соединений — `agent_sessions`. Полные protobuf JSON envelopes
сохраняются в `agent_messages`, включая logs/topology/events/telephony/results.
Для них пока нет поиска, отдельных REST-экранов или аналитики.

Нет incident engine, AI, отчётов, отправки команд и глобального reconciliation
устройств разных collectors. Нет retention и гарантированной доставки Agent:
его текущий протокол не содержит durable delivery ACK. Дедупликация в базе не
превращает транспорт в exactly-once. mTLS проверяет CA, но связь сертификата
с заявленным `agent_id` требует отдельной политики enrollment.
