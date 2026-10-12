# InfraPulse Backend

InfraPulse Backend — серверный Go-компонент NetOps Platform InfraPulse.
Он принимает данные инфраструктуры от Agent, хранит их в PostgreSQL и
предоставляет REST API для просмотра устройств, их состояния и истории метрик.

Agent отвечает за сбор данных на хостах и сетевых устройствах. Backend
объединяет результаты collectors в одном хранилище и предоставляет интерфейс
для клиентских приложений InfraPulse. Web UI находится вне этого репозитория.

## Возможности

Переход на окончательный Backend v2 начат с
[B0: аудит и сохранение совместимости](docs/backend-v2-b0.md).
Матрица действующих маршрутов: [REST API baseline](docs/backend-v2-b0-api-matrix.md).
Добавлен [B1: совместимость Syslog/Redfish Collector](docs/backend-v2-b1.md).
Добавлен [B2.1: реестр источников, API и шифрование credentials](docs/backend-v2-b2.md).
Добавлен [B2.2: очередь операций, планировщик и история](docs/backend-v2-b2-jobs.md).
Добавлен [B2.3: политика адресов, DNS и TLS внешних источников](docs/backend-v2-b2-network.md).
Добавлен [B3.1: Asset registry, source bindings и история](docs/backend-v2-b3-registry.md).
Добавлен [B3.2: предложения сопоставления и ручные merge/split](docs/backend-v2-b3-decisions.md).
Добавлен [B3.3: automatic resolver, native publisher и legacy backfill](docs/backend-v2-b3-resolution.md).
Добавлен [B4.1: Zabbix — проверка подключения и inventory выбранных хостов](docs/backend-v2-b4-zabbix.md).
Текущий контракт синхронизирован с финальным Collector `215d33a`:
[совместимость Phase 8A и границы Backend B9](docs/backend-collector-phase8-compatibility.md).
Добавлен [B4.2: выбранные метрики, problems/events и общие инциденты Zabbix](docs/backend-v2-b4-monitoring.md).
Добавлен [B5: Prometheus targets, latest и query-on-demand графики](docs/backend-v2-b5-prometheus.md).
Добавлен [B6: сводная инвентаризация, площадки/помещения/стойки и безопасное размещение](docs/backend-v2-b6-inventory-locations.md).
Добавлен [B7: глобальная topology по Asset, источники связей, двусторонность и история TTL](docs/backend-v2-b7-global-topology.md).
Добавлен [B8: hardware, логи/audit и SIP/VoIP в общей карточке Asset](docs/backend-v2-b8-asset-observability.md).
Добавлен [B9: управляемая конфигурация Collector без перезапуска](docs/backend-v2-b9-collector-configuration.md).
Добавлена [реализация B10: Ansible Worker, read-only задания и зашифрованные снимки](docs/backend-v2-b10-ansible.md).
Добавлен [B11.1: approved baseline, безопасное сравнение конфигураций и история drift](docs/backend-v2-b11-1-configuration-drift.md).
Текущий backend: B0–B10 и диагностическая часть B11.1; дальше — B11.2 approval/policy/восстановление/verification, затем B12–B16. [Проверки B9](docs/backend-v2-b9-validation.md), [открытая Linux/device приёмка B10](docs/backend-v2-b10-validation.md), [проверки B11.1](docs/backend-v2-b11-1-validation.md).
Базовая инфраструктура B2/B3 и коннектор B4 проверены на fixtures. Приёмка на настоящем Zabbix ещё нужна; команда пока не предоставила стенд.

- Регистрация Agent, учёт подключений, история сессий и приём heartbeat.
- Хранение inventory и последнего наблюдённого состояния устройств.
- Приём и история telemetry: CPU, RAM, interfaces, counters и rates,
  переданных Agent.
- Application ACK после сохранения в PostgreSQL и дедупликация повторной доставки.
- REST API: устройства, фильтрация по Agent, inventory, последние метрики
  и история измерений.
- Оценка свежести telemetry и настройка ожидаемых интервалов мониторинга.
- Карта соседей и доступность устройств `online/offline/stale/unknown` с
  источником наблюдения, сводкой и историей сообщённых переходов.
- Логи и Linux audit: отдельные таблицы, полнотекстовый поиск, фильтры,
  пагинация, сводка и группировка строк одного события.
- SIP OPTIONS: отдельные точки телефонии, история response code/latency,
  состояния `up/degraded/down/stale/unknown` и ожидаемые интервалы проверки.
- Локальные пользователи с ролями viewer/operator/admin, cookie/Bearer-сессии,
  проверка CSRF у cookie-мутаций и журнал действий управления доступом/интервалами.
- Подтверждённые команды RouterOS interface enable/disable через существующий
  Agent stream: очередь, idempotency, журнал состояний и проверенный результат.
- Registry наблюдённых Agent snapshot references и typed RollbackAction,
  с ownership, reservation, consumption и явно неизвестным Agent TTL.
- TLS/mTLS для gRPC, автоматические миграции БД, health/readiness endpoints.
- При включённом mTLS: проверка SPIFFE URI сертификата против UUID Agent до
  регистрации, при приёме сообщений и перед отправкой команды.
- Health rules CPU/RAM и условный CPU forecast, incident lifecycle,
  evidence snapshots, операторское acknowledge и автоматическое стабильное recovery.

Этапы 1 (topology), 2 (availability), 3 (logs/audit), 4 (SIP/telephony)
5 (REST auth/roles), 6 (typed commands) и 7 (snapshots/rollback) реализованы в текущей локальной копии.
Они пока не опубликованы на GitHub. Исходники Agent и protobuf-контракт
в этих этапах не изменяются.

Реализован [этап 8: PKI enrollment/renewal](docs/stage8-pki.md): отдельный HTTPS,
одноразовые UUID-bound bootstrap tokens, registry/audit/отзыв сертификатов,
автоматическое обновление и повторное подключение неизменённого Agent.
Live managed configuration исходного перечня реализована в B9 через DesiredConfigSnapshot.
Оставшаяся приёмка incidents/reports/AI и последующие B11–B16 относятся к окончательному плану v2; реальный Ansible/device запуск B10 требует Linux-стенда.

Начат пункт10: [этап9A — health/incidents](docs/stage9a-health-incidents.md).
Расширен [этапом9B — network/SIP incidents](docs/stage9b-network-sip-incidents.md).
Добавлен [этап9C — flap/collector incidents](docs/stage9c-flap-collector-incidents.md).
Первые11rules, история инцидентов и [отчёты этапа9D](docs/stage9d-reports.md)
работают. Добавлен [связанный контекст этапа9E](docs/stage9e-diagnostic-context.md),
который сохраняет ограничения и не объявляет причину доказанной. Добавлен
[этап9F — AI preview/analysis](docs/stage9f-ai-analysis.md), provider выключен
по умолчанию. Live model/key acceptance и итоговый deployment ещё не проверены.
До acceptance не объявляем исходный пункт10 полностью подтверждённым.

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
| `topology_snapshots`, `topology_observations` | Опросы соседей и последние направленные связи с TTL |
| `availability_sources`, `device_availability_current` | Последние наблюдения доступности и выбранное сообщение о состоянии |
| `device_availability_changes`, `availability_event_ids` | Сжатая история сообщённых переходов и дедупликация событий |
| `log_batches`, `log_records`, `audit_records` | Пакеты логов, индексируемые строки и структурированные поля Linux audit |
| `telephony_batches`, `telephony_targets`, `telephony_samples`, `telephony_policies` | SIP-точки, история проверок и ожидаемые интервалы |
| `access_users`, `access_sessions`, `access_audit` | Пользователи, хеши сессий и действия управления доступом/интервалами |
| `controller_commands`, `command_events`, `command_results` | Намерения операторов, журнал dispatch и результаты Agent |
| `command_snapshots` | Наблюдённые ссылки Agent, происхождение, reservation/consumption/supersession |

Устройство идентифицируется парой `(agent_id, device_id)`: одинаковые ID
у разных collectors не перезаписывают друг друга. Сообщение и его изменения
устройств/метрик сохраняются в одной транзакции.

## Структура проекта

```text
cmd/server/           запуск Backend
cmd/backfill-logs/    перенос прежних архивных логов в поисковые таблицы
cmd/access/           создание первого администратора после миграций сервера
internal/controller/  gRPC, регистрация и ACK
internal/storage/     PostgreSQL, миграции и запросы
internal/api/         REST API и OpenAPI
internal/monitoring/  интервалы и оценка свежести данных
internal/availability/ оценка доступности по наблюдениям и состоянию Agent
internal/logbook/     нормализация, присутствие полей и фильтрация секретов в логах
internal/telephony/   модель SIP observations, freshness и ожидаемые интервалы
internal/access/      локальные пользователи, сессии, роли и пароли
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
docker compose exec backend /app/access init-admin --username oleg --generate-password
```

Сохраните выданный `generated_password`: открытой регистрации и стандартного
admin-пароля нет. REST защищён по умолчанию; `/api/devices` без входа теперь
вернёт 401. Вход через `/api/auth/login` выдаёт браузеру cookie, а
`/api/auth/token` — Bearer-токен для API-клиента. Подробный порядок запуска,
роли и примеры запросов — [этап 5](docs/access-stage5.md).

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

После готовности сервера в другом терминале с тем же `DATABASE_URL` создайте
первого администратора:

```sh
go run ./cmd/access init-admin --username oleg --generate-password
```

CLI использует уже созданную схему и не сбрасывает подключённые Agent.

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
| `DEFAULT_TELEPHONY_INTERVAL` | `30s` | Ожидаемый SIP probe interval; overrides задаются отдельно для каждой точки |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Не заданы | Сертификат и ключ gRPC-сервера |
| `TLS_CLIENT_CA_FILE` | Не задан | CA для проверки клиентов; включает mTLS |
| `AUTH_MODE` | `required` | REST требует сессию; `disabled` допустим только на числовом loopback listener |
| `AUTH_SESSION_TTL` | `8h` | Абсолютный срок REST-сессии, допустимо 1m..24h |
| `AUTH_PUBLIC_ORIGIN` | Не задан вне Compose | Точный внешний origin; Compose задаёт `http://127.0.0.1:8080` |
| `AUTH_COOKIE_SECURE` | Зависит от listener | Для HTTPS — true; local Compose явно задаёт false |
| `COMMANDS_ENABLED` | `false` | Явно разрешить backend typed dispatch |
| `COMMAND_ALLOW_DISABLE` | `false` | Дополнительно разрешить interface disable |
| `COMMAND_ALLOW_ROLLBACK` | `false` | Разрешить typed rollback; также нужен AllowDisable |
| `COMMAND_PROTECTED_DEVICES`, `COMMAND_PROTECTED_INTERFACES` | Пустые additions | Дополнительная защита; defaults lo/eth0/ether1 сохраняются |

Monitoring policies описывают ожидаемые интервалы от 3s до 24h и используются
для оценки свежести данных. Изменение policy не меняет расписание Agent:
его YAML настраивается вручную.

Для внешнего HTTPS-стенда задайте `AUTH_PUBLIC_ORIGIN=https://...`,
`AUTH_COOKIE_SECURE=true` и обслуживайте REST через reverse proxy. Встроенные
TLS-параметры относятся к gRPC. Внутренний HTTP listener ограничьте private/loopback
сетью. Если меняете `HTTP_PORT` в Compose, поменяйте порт и в public origin.

## Интерфейсы

### gRPC

`AgentService.Connect` принимает Hello, heartbeat, inventory, telemetry и другие
известные сообщения Agent. Backend возвращает HelloAck и, при согласованной
поддержке, MessageACK. Topology, logs, events, telephony, command results и error
reports сохраняются в архиве сообщений. В локальном этапе 1 topology также
сохраняется в отдельных таблицах и доступна как карта nodes/edges через REST.
В этапе 2 события `device.online`/`device.offline` от `remote` и `simulated`
также участвуют в оценке доступности; отсутствие сообщений само по себе
не сообщает отказ устройства.
В этапе 3 `LogBatch` и вложенные `AuditRecord` сохраняются также в поисковых
таблицах в той же транзакции. Для ранее архивированных сообщений предусмотрена
отдельная повторяемая команда `backfill-logs`; она не перезапускает Controller.
В этапе 4 TelephonyUpdate сохраняется в отдельных таблицах. Target ID не
равен device ID: SIP failure не объявляет сетевое устройство offline.

### REST API

| Метод и путь | Назначение |
|---|---|
| `POST /api/auth/login`, `POST /api/auth/token` | Вход с username/password: cookie или непрозрачный Bearer-токен |
| `GET /api/auth/me` | Текущий пользователь, права, срок сессии и CSRF token |
| `POST /api/auth/logout`, `POST /api/auth/logout-all` | Отзыв текущей или всех собственных сессий |
| `POST /api/auth/password` | Смена собственного пароля с проверкой текущего |
| `GET`, `POST /api/access/users` | Список/создание пользователей, только admin |
| `PATCH /api/access/users/{id}`, `POST /api/access/users/{id}/password` | Роль, отключение или новый пароль, только admin |
| `GET /api/access/audit` | Журнал действий backend, только admin |
| `GET /api/agents` | Зарегистрированные агенты и состояние связи |
| `GET /api/topology` | Карта соседей: nodes, edges, expiry, resolution и итоги опросов |
| `GET /api/devices/{id}/topology` | Исходящие наблюдения конкретного устройства |
| `GET /api/devices` | Список устройств с `agent_id`, `limit`, `offset` |
| `GET /api/devices/{id}` | Карточка устройства |
| `GET /api/availability/summary` | Число устройств в каждом состоянии, необязательный фильтр `agent_id` |
| `GET /api/devices/{id}/availability` | Текущая доступность, причина, источник и срок актуальности наблюдения |
| `GET /api/devices/{id}/availability/history` | Сообщённые переходы состояния, новые записи первыми |
| `GET /api/logs`, `GET /api/logs/{id}` | Поиск по логам и чтение отдельной строки |
| `GET /api/logs/summary` | Сводка severity, audit failures и скрытых секретов |
| `GET /api/audit` | Строки Linux audit с UID/AUID/PID, executable и результатом |
| `GET /api/audit/events`, `GET /api/audit/events/{key}` | Группы audit events и их строки |
| `GET /api/devices/{id}/logs`, `GET /api/devices/{id}/audit` | Логи и аудит одного устройства |
| `GET /api/telephony/targets`, `GET /api/telephony/targets/{id}` | SIP-точки и карточка последней квалифицированной проверки |
| `GET /api/telephony/targets/{id}/history` | История результатов и задержек SIP OPTIONS |
| `GET /api/telephony/summary` | Сводка пяти состояний SIP-точек |
| `GET`, `PUT /api/telephony/targets/{id}/monitoring` | Ожидаемый SIP интервал 3s..24h; применение в Agent ручное |
| `GET /api/devices/{id}/inventory` | Текущий inventory устройства |
| `GET /api/devices/{id}/telemetry` | Последнее измерение; `/metrics` — alias |
| `GET /api/devices/{id}/metrics/history` | История измерений |
| `GET`, `PUT /api/devices/{id}/monitoring` | Ожидаемые интервалы мониторинга |
| `GET /healthz`, `GET /readyz` | Состояние процесса и доступность БД |
| `GET /openapi.json` | OpenAPI-контракт |
| `POST /api/commands` | Подтверждённая typed interface-команда operator/admin с Idempotency-Key |
| `GET /api/commands`, `GET /api/commands/{id}` | Очередь и результат выполнения |
| `GET /api/commands/{id}/events`, `GET /api/commands/policy` | Журнал переходов и backend policy |
| `POST /api/commands/{id}/rollback` | Подтверждённый откат по исходной команде и её Agent snapshot |
| `GET /api/snapshots`, `GET /api/commands/{id}/snapshot` | Просмотр ссылок и lifecycle; snapshot content/TTL не передаются |

Данные `/api/...` читают все три роли; PUT monitoring разрешён operator/admin.
При cookie-аутентификации мутации требуют `X-CSRF-Token` из `/api/auth/me`.
Cookie и Bearer не передаются одновременно. Public health/readiness/OpenAPI
доступны без сессии; frontend-разработчик получает только контракт API,
пользовательский интерфейс в этом репозитории не создаётся.

В запросах конкретного устройства передавайте `agent_id`, если его ID встречается
у нескольких collectors. `availability.status` различает `online`, `offline`,
`stale` и `unknown`. Поле совместимости `online` равно `true` только для статуса
`online`; последнее принятое сообщение находится в `reported_online`
(`true`, `false` или `null`). `agent_connected` показывает доступность collector.
Потеря связи и истечение срока наблюдения дают `stale`, сохраняя последнее
сообщённое состояние. Подробности — в [описании этапа 2](docs/availability-stage2.md).

## Текущие ограничения

- Поддерживается один экземпляр Backend; автоматическая очистка истории отсутствует.
- Для произвольных events/results пока нет отдельных REST API и поиска.
- SIP OPTIONS не измеряет качество аудио/RTP, звонки или регистрацию абонентов.
  Старые архивные telephony envelopes автоматически не переносятся в поисковую историю.
- Полнота группы Linux audit не доказана; retention и неизменяемый audit trail
  операторов ещё не реализованы. Журнал access_audit уже сохраняет действия
  управления доступом/интервалами, но не все HTTP-запросы. Фильтрация типовых секретов в поисковых строках
  не обезличивает инфраструктуру и не переписывает исходный архив Agent.
- Live ConfigUpdate ещё не реализован. AI workflow доступен и default-disabled;
  live provider acceptance остаётся. Contextual correlation доступна,
  но не выполняет root-cause proof или автоматический ремонт. Incident engine охватывает
  CPU/RAM/trend, oper_down/utilization/errors/drops и SIP availability/latency,
  но не обещает анализ причин сетевых отказов.
- Команды ограничены поддерживаемыми RouterOS interfaces. Нет shell/CLI/service
  executor. Uncertain блокирует новые мутации; автоматического resend/unlock нет.
- REST использует локальные роли на всю установку; SSO, MFA, восстановление
  по email и ограничения по отдельным Agent отсутствуют. HTTPS завершается
  внешним reverse proxy. Без TLS-сертификатов gRPC работает в development-режиме;
  Compose публикует порты только на loopback.
  mTLS проверяет сертификат клиента, но привязка к заявленному Agent ID отсутствует.

## Документация

- [Архитектура](docs/architecture.md) — компоненты и модели хранения.
- [REST API](docs/api.md) — ответы, параметры и ошибки.
- [Этап 1: topology](docs/topology-stage1.md) — карта, сопоставление, TTL, API и границы реализации.
- [Этап 2: availability](docs/availability-stage2.md) — четыре состояния, источники, история, API и локальные проверки.
- [Этап 3: logs/audit](docs/logs-audit-stage3.md) — поиск, группы audit, дедупликация, backfill и проверки.
- [Этап 4: SIP/telephony](docs/telephony-stage4.md) — точки, история, freshness, интервалы и проверка с обычным Agent.
- [Этап 5: REST auth/roles](docs/access-stage5.md) — первый admin, cookie/Bearer, CSRF, права и журнал доступа.
- [Этап 6: commands](docs/commands-stage6.md) — подтверждение, preflight, dispatch, результаты и ограничения.
- [Этап 7: rollback](docs/rollback-stage7.md) — Agent snapshot references, typed rollback, lifecycle и проверка.
- [Этап 8: PKI](docs/stage8-pki.md) — HTTPS enrollment/renewal, bootstrap tokens, отзыв и совместимость Agent.
- [Этап 9A: health/incidents](docs/stage9a-health-incidents.md) — ранний сигнал, evidence, acknowledge и восстановление.
- [Этап 9B: network/SIP incidents](docs/stage9b-network-sip-incidents.md) — port/target resources, rates, OPTIONS и доказательства.
- [Этап 9C: flap/collector incidents](docs/stage9c-flap-collector-incidents.md) — persistent переключения порта, loss/recovery Agent.
- [Этап 9D: reports](docs/stage9d-reports.md) — период, recurrence, quality, JSON export и snapshots.
- [Этап 9E: diagnostic context](docs/stage9e-diagnostic-context.md) — связи симптомов, topology evidence и ограничения.
- [Этап 9F: AI](docs/stage9f-ai-analysis.md) — exact preview, consent, asynchronous provider и verified result storage.
- [Интервалы мониторинга](docs/monitoring-intervals.md) — policies и freshness.
- [Надёжная доставка](docs/reliable-delivery.md) — ACK, replay и integration tests.
- [Генерация protobuf](scripts/README.md) — инструменты и проверка контракта.
