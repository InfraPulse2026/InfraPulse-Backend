# InfraPulse Backend

InfraPulse Backend — серверный Go-компонент NetOps Platform InfraPulse.
Он принимает данные инфраструктуры от Agent, хранит их в PostgreSQL и
предоставляет REST API для просмотра устройств, их состояния и истории метрик.

Agent отвечает за сбор данных на хостах и сетевых устройствах. Backend
объединяет результаты collectors в одном хранилище и предоставляет интерфейс
для клиентских приложений InfraPulse. Web UI находится вне этого репозитория.

## Возможности

- Регистрация Agent, учёт подключений, история сессий и приём heartbeat.
- Хранение inventory и последнего наблюдённого состояния устройств.
- Приём и история telemetry: CPU, RAM, interfaces, counters и rates,
  переданных Agent.
- Application ACK после сохранения в PostgreSQL и дедупликация повторной доставки.
- REST API: устройства, фильтрация по Agent, inventory, последние метрики
  и история измерений.
- Оценка свежести telemetry и настройка ожидаемых интервалов мониторинга.
- TLS/mTLS для gRPC, автоматические миграции БД, health/readiness endpoints.

## Как это работает

```text
Хосты и сетевые устройства
          │
          ▼
InfraPulse Agent / Collector
          │ исходящее gRPC-соединение
          ▼
Backend Controller → PostgreSQL → REST API → клиенты InfraPulse
          │
          └── ACK после сохранения → Agent
```

Agent сам устанавливает bidirectional stream `infrapulse.v1.AgentService/Connect`.
Backend регистрирует Agent и его session, принимает inventory, heartbeat и
telemetry. Сбор Linux/OpenWrt и опрос SNMP/RouterOS выполняются на стороне Agent.

При согласованном application ACK Backend подтверждает сообщение после COMMIT.
Durable Agent сохраняет его в outbox до ACK и повторяет отправку после потери
соединения. Повтор сообщения с тем же ID не создаёт вторую запись telemetry;
использование того же ID для другого содержимого отклоняется.

### Данные в PostgreSQL

| Сущность | Содержимое |
|---|---|
| `agents`, `agent_sessions` | Регистрация, heartbeat, состояние подключения и история сессий |
| `devices` | Inventory и последнее наблюдённое состояние устройства |
| `telemetry` | Измерения, время наблюдения, CPU/RAM и полный sample в JSONB |
| `agent_messages` | Принятые сообщения Agent и состояние дедупликации |
| `monitoring_policies` | Ожидаемые интервалы мониторинга устройств |

Устройство идентифицируется парой `(agent_id, device_id)`: одинаковые ID
у разных collectors не перезаписывают друг друга. Сообщение и его изменения
устройств/метрик сохраняются в одной транзакции.

## Структура проекта

```text
cmd/server/           запуск Backend
internal/controller/  gRPC, регистрация и ACK
internal/storage/     PostgreSQL, миграции и запросы
internal/api/         REST API и OpenAPI
internal/monitoring/  интервалы и оценка свежести данных
internal/config/      конфигурация из environment variables
api/proto/, api/gen/  protobuf-контракт и generated Go sources
configs/              примеры подключения Agent
scripts/, docs/       инструменты и документация
```

## Быстрый старт

Для контейнерного запуска нужен Docker с Compose. Из корня Backend:

```sh
cp .env.example .env
docker compose up --build -d
docker compose logs -f backend
```

В PowerShell вместо `cp` используйте `Copy-Item .env.example .env`.
Compose запускает Backend и PostgreSQL; миграции применяются автоматически.
Данные БД сохраняются в volume `postgres_data`.

По умолчанию REST доступен на `http://127.0.0.1:8080`, gRPC — на
`127.0.0.1:9443`, PostgreSQL — на `127.0.0.1:5432`.

```sh
curl http://127.0.0.1:8080/readyz
curl http://127.0.0.1:8080/api/devices
```

До подключения Agent список устройств пуст. Остановка:
`docker compose down`; volume с данными сохраняется.

### Подключение Agent

Для Linux local collector с durable delivery подготовлен
[agent.linux.durable.yaml](configs/agent.linux.durable.yaml).
Он использует `queue.mode: durable`, `ack_fallback: reject` и telemetry каждые 3s.

Если репозитории расположены рядом, из корня `InfraPulseAgent`:

```sh
mkdir -p bin
go build -o bin/infrapulse-agent ./cmd/infrapulse-agent
./bin/infrapulse-agent validate-config --config ../InfraPulseBackend/configs/agent.linux.durable.yaml
./bin/infrapulse-agent run --config ../InfraPulseBackend/configs/agent.linux.durable.yaml
```

Пример рассчитан на Agent и Backend на одной Linux-машине. Сохраняйте каталог
состояния Agent между запусками: в нём находятся постоянный Agent ID и outbox.
Для memory mode есть [agent.linux.yaml](configs/agent.linux.yaml), для Simulator —
[agent.simulator.yaml](configs/agent.simulator.yaml).

### Запуск из исходников

Нужны Go 1.27 или новее и доступный PostgreSQL. Для локальной БД можно использовать
Compose:

```sh
docker compose up -d postgres
go run ./cmd/server
```

При стандартных настройках `.env.example` параметры БД совпадают с defaults
Backend. При изменении адреса или credentials задайте `DATABASE_URL`.
Файл `.env` читает Compose; `go run` его автоматически не загружает.
`protoc` для обычной сборки не требуется.

## Конфигурация

Backend настраивается через environment variables. В Compose порты и параметры
PostgreSQL задаются в [.env.example](.env.example).

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL` | Локальная БД с credentials из `.env.example` | Подключение к PostgreSQL |
| `HTTP_ADDRESS` | `127.0.0.1:8080` | REST listener |
| `GRPC_ADDRESS` | `127.0.0.1:9443` | gRPC listener |
| `AGENT_TIMEOUT` | `90s` | Срок актуальности связи с Agent |
| `DEFAULT_TELEMETRY_INTERVAL` | `3s` | Ожидаемый интервал telemetry |
| `DEFAULT_INVENTORY_INTERVAL`, `DEFAULT_TOPOLOGY_INTERVAL` | `5m` | Ожидаемые интервалы inventory/topology |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Не заданы | Сертификат и ключ gRPC-сервера |
| `TLS_CLIENT_CA_FILE` | Не задан | CA для проверки клиентов; включает mTLS |

Monitoring policies описывают ожидаемые интервалы от 3s до 24h и используются
для оценки свежести данных. Изменение policy не меняет расписание Agent:
его YAML настраивается вручную.

## Интерфейсы

### gRPC

`AgentService.Connect` принимает Hello, heartbeat, inventory, telemetry и другие
известные сообщения Agent. Backend возвращает HelloAck и, при согласованной
поддержке, MessageACK. Topology, logs, events, telephony, command results и error
reports сохраняются в архиве сообщений.

### REST API

| Метод и путь | Назначение |
|---|---|
| `GET /api/agents` | Зарегистрированные агенты и состояние связи |
| `GET /api/devices` | Список устройств с `agent_id`, `limit`, `offset` |
| `GET /api/devices/{id}` | Карточка устройства |
| `GET /api/devices/{id}/inventory` | Текущий inventory устройства |
| `GET /api/devices/{id}/telemetry` | Последнее измерение; `/metrics` — alias |
| `GET /api/devices/{id}/metrics/history` | История измерений |
| `GET`, `PUT /api/devices/{id}/monitoring` | Ожидаемые интервалы мониторинга |
| `GET /healthz`, `GET /readyz` | Состояние процесса и доступность БД |
| `GET /openapi.json` | OpenAPI-контракт |

В запросах конкретного устройства передавайте `agent_id`, если его ID встречается
у нескольких collectors. `online` отражает последнее наблюдённое состояние
устройства, а `agent_connected` — доступность его collector.

## Текущие ограничения

- Поддерживается один экземпляр Backend; автоматическая очистка истории отсутствует.
- Для архивируемых topology/logs/events/telephony/results пока нет отдельных
  REST API и поиска.
- Отправка команд, ConfigUpdate, production enrollment, AI и incident engine
  не реализованы.
- REST API не имеет встроенной авторизации и HTTPS. Без TLS-сертификатов gRPC
  работает в development-режиме; Compose публикует порты только на loopback.
  mTLS проверяет сертификат клиента, но привязка к заявленному Agent ID отсутствует.

## Документация

- [Архитектура](docs/architecture.md) — компоненты и модели хранения.
- [REST API](docs/api.md) — ответы, параметры и ошибки.
- [Интервалы мониторинга](docs/monitoring-intervals.md) — policies и freshness.
- [Надёжная доставка](docs/reliable-delivery.md) — ACK, replay и integration tests.
- [Генерация protobuf](scripts/README.md) — инструменты и проверка контракта.
