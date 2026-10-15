# Контракт для frontend

Базовый URL: `http://127.0.0.1:8080`. OpenAPI: `/openapi.json` (текущая версия 0.44.0, исходник internal/api/openapi.json).

Prometheus instant/range/metadata, latest snapshots и scoping описаны в
[B5](backend-v2-b5-prometheus.md). Frontend получает графики через backend;
token внешнего источника ему не передаётся.

Внешние метрики/сигналы и поля source-instance Incident описаны в
[B4.2 Zabbix monitoring](backend-v2-b4-monitoring.md). Это дополнение к прежним
маршрутам Collector, не их замена. Frontend в этом backend не создаётся.
Имена полей — `snake_case`. Все времена в REST-карточках — UTC RFC3339.

REST-аутентификация включена по умолчанию (`AUTH_MODE=required`). Данные
требуют cookie-сессию или `Authorization: Bearer <access_token>`.
Cookie-мутации дополнительно требуют `X-CSRF-Token` из `GET /api/auth/me`.
Не передавайте cookie и Bearer одновременно. Роли глобальны для установки:
viewer читает данные, operator также меняет monitoring policy и создаёт разрешённые команды, admin управляет
пользователями. Public `/healthz`, `/readyz`, `/openapi.json` доступны без входа.
Первого admin создаёт CLI после старта сервера; открытой регистрации нет.
Подробности и пример cookie fetch — [этап 5](access-stage5.md).

| Метод и путь | Ответ |
|---|---|
| `POST /api/auth/login` | user/expires_at/csrf_token и HttpOnly cookie; тело username/password |
| `POST /api/auth/token` | access_token/token_type/expires_at/user; тело username/password |
| `GET /api/auth/me` | user/permissions/expires_at/csrf_token |
| `POST /api/auth/logout`, `POST /api/auth/logout-all` | 204, текущая или все собственные сессии отозваны |
| `POST /api/auth/password` | current_password/password → 204, все собственные сессии отозваны |
| `GET /api/access/users` | admin: items/limit/offset |
| `POST /api/access/users` | admin: username/password/role → 201, user |
| `PATCH /api/access/users/{id}` | admin: role и/или disabled → user, сессии отозваны при изменении |
| `POST /api/access/users/{id}/password` | admin: password → 204, сессии пользователя отозваны |
| `GET /api/access/audit` | admin: журнал управления доступом/интервалами, items/limit/offset |
| `POST /api/commands` | operator/admin: 202 queued либо 200 replay, required Idempotency-Key |
| `GET /api/commands` | items/limit/offset, filters agent_id/device_id/state |
| `GET /api/commands/{id}`, `GET /api/commands/{id}/events` | состояние/result и журнал переходов |
| `GET /api/commands/policy` | backend switches/protected resources |
| `POST /api/commands/{id}/rollback` | Подтверждённый rollback исходной команды, Idempotency-Key, 202/200 |
| `GET /api/commands/{id}/snapshot`, `GET /api/snapshots` | Наблюдённые Agent references; expires_at=null означает неизвестный срок |
| `GET /healthz` | `{"status":"ok"}` — процесс работает |
| `GET /readyz` | `{"status":"ready"}` или 503 — проверка PostgreSQL |
| `GET /api/agents` | `{"items":[...]}`; максимум 1000 записей |
| `GET /api/topology` | `nodes/edges/collections` с TTL и confidence |
| `GET /api/devices/{id}/topology` | Исходящие наблюдения одного устройства |
| `GET /api/devices?limit=100&offset=0&agent_id=...` | `{"items":[...],"limit":100,"offset":0}` |
| `GET /api/devices/{id}` | карточка одного устройства |
| `GET /api/availability/summary?agent_id=...` | количество устройств `online/offline/stale/unknown`; без фильтра — все агенты |
| `GET /api/devices/{id}/availability?agent_id=...` | доступность и сведения о выбранном наблюдении |
| `GET /api/devices/{id}/availability/history?agent_id=...&limit=100&offset=0` | `{"items":[...],"limit":100,"offset":0}`, новые сообщённые переходы первыми |
| `GET /api/logs?q=denied&limit=100` | `{"items":[...],"limit":100,"next_before_id":42}` — поиск, строки с большими ID первыми |
| `GET /api/logs/{id}` | отдельная строка и вложенный audit либо `null` |
| `GET /api/logs/summary` | счётчики всех строк, соответствующих фильтрам |
| `GET /api/audit?success=false&uid=1000` | строки audit с фильтрами; тот же формат страницы |
| `GET /api/audit/events` | группы найденных строк; `completeness=unknown` |
| `GET /api/audit/events/{key}` | строки одной группы, ключ кодируется через `encodeURIComponent` |
| `GET /api/devices/{id}/logs`, `GET /api/devices/{id}/audit` | страницы логов/аудита одного устройства |
| `GET /api/telephony/targets?agent_id=...&transport=tls` | `items/limit/offset`, SIP-точки отдельно от устройств |
| `GET /api/telephony/targets/{id}` | latest sample, clock issue, health и monitoring |
| `GET /api/telephony/targets/{id}/history?limit=100` | `items/next_before_id/limit`, новые вставленные строки первыми |
| `GET /api/telephony/summary?agent_id=...` | total, up/degraded/down/stale/unknown и диагностические счётчики |
| `GET`, `PUT /api/telephony/targets/{id}/monitoring` | expected_interval_seconds 3..86400; Agent настраивается вручную |
| `GET /api/devices/{id}/inventory` | последняя полная Device-модель из protobuf |
| `GET /api/devices/{id}/monitoring` | ожидаемые интервалы и YAML-фрагменты для ручной настройки Agent |
| `PUT /api/devices/{id}/monitoring` | сохранить три ожидаемых интервала в диапазоне 3..86400 секунд |
| `GET /api/devices/{id}/telemetry` | последнее измерение |
| `GET /api/devices/{id}/metrics` | тот же ответ, что `/telemetry` |
| `GET /api/devices/{id}/metrics/history?limit=100` | `{"items":[...],"limit":100}`, новые измерения первыми |

Для списка устройств и availability/history: `limit` от 1 до 1000
(по умолчанию 100), `offset` от 0 до 1 000 000 (по умолчанию 0).
Metrics/history принимает `limit` от 1 до 1000. Пустые списки всегда `[]`.
Для logs/audit `limit` 1..500 (default 100), следующая страница — `before_id`
из `next_before_id`, без `offset`. Фильтры на следующих страницах сохраняйте.
Полнота, время событий, поиск по словам/подстроке, `null` и audit correlation
описаны в [этапе 3](logs-audit-stage3.md).
Подтверждение, command body и uncertainty — [этап 6](commands-stage6.md).
Typed rollback, ownership и lifecycle ссылок — [этап 7](rollback-stage7.md).
SIP endpoints и правила отображения — в [этапе 4](telephony-stage4.md).
Для неоднозначного target ID возвращается 409 `ambiguous_target`;
передайте `agent_id`. Список SIP-точек: limit 1..500, offset 0..1000000.
SIP history: limit 1..500, before_id, from/to по observed_at; offset не используется.
История пустого устройства возвращает 200 с пустым списком; запрос последнего
измерения — 404, если измерений ещё нет.

Идентичность устройства в БД — пара `(agent_id, device_id)`. Если одинаковый
`device_id` встретился у нескольких агентов, detail-запрос без `agent_id`
вернёт 409. Передавай `?agent_id=...` из карточки устройства. Поле `id` в пути
нужно кодировать через `encodeURIComponent`.

Пример карточки (значения иллюстративные):

```json
{
  "agent_id": "collector-uuid",
  "device_id": "local:collector-uuid",
  "name": "linux-demo-agent",
  "hostname": "ubuntu-demo",
  "vendor": "",
  "model": "",
  "type": "DEVICE_TYPE_SERVER",
  "management_address": "",
  "source": "",
  "online": true,
  "reported_online": true,
  "availability": {
    "status": "online",
    "reason": "device_reported_online",
    "last_reported_online": true,
    "agent_connected": true,
    "evidence_kind": "telemetry",
    "source": "linux",
    "message_id": "message-uuid",
    "time_basis": "agent_observed",
    "observed_at": "2026-10-05T16:00:00Z",
    "received_at": "2026-10-05T16:00:01Z",
    "expires_at": "2026-10-05T16:00:11Z",
    "age_seconds": 1,
    "expected_interval_seconds": 3,
    "stale_after_seconds": 11,
    "clock_skew": false
  },
  "agent_connected": true,
  "last_seen": "2026-10-05T16:00:00Z",
  "received_at": "2026-10-05T16:00:01Z",
  "inventory": {"id": "local:collector-uuid", "os": "linux"}
}
```

`availability.status` — состояние для показа на экране: `online`, `offline`,
`stale` или `unknown`. `online` — поле совместимости: оно равно `true` только
для статуса `online`. `reported_online` и `availability.last_reported_online`
содержат последнее принятое сообщение (`true`/`false`); `null` означает,
что достоверного сообщения ещё нет. Поэтому `online:false` само по себе
недостаточно для вывода об отказе: проверяй `availability.status`.

`agent_connected` — связь с Controller и свежесть входящих сообщений.
Потеря связи и истечение срока наблюдения переводят состояние в `stale`,
сохраняя последнее сообщённое значение и его источник. `received_at` — время
приёма backend; `last_seen` — время, сохранённое в карточке устройства,
которое само по себе не определяет текущую доступность. У Simulator бизнес-время
может следовать фиксированной временной шкале, поэтому отличается от времени
приёма. Отмечай `source:simulated` на экране.

## Доступность и история сообщённых состояний

`GET /api/devices/{id}/availability` возвращает ровно объект `availability`
из карточки. Известный объект без достоверного наблюдения возвращает 200
со статусом `unknown`; отсутствующее устройство — 404. Неоднозначный ID без
`agent_id` возвращает 409. Для этих маршрутов ID устройства и агента ограничены
256 байтами UTF-8.

| `status` | Значение |
|---|---|
| `online` | Есть актуальное положительное наблюдение, Agent доступен |
| `offline` | Есть актуальное явное отрицательное сообщение, Agent доступен |
| `stale` | Последнее сообщение известно, но истекло или связь с Agent потеряна |
| `unknown` | Достоверного сообщения ещё нет, в том числе при наличии только данных с ошибкой часов |

`reason` уточняет причину: `device_reported_online`, `device_reported_offline`,
`agent_heartbeat`, `observation_expired`, `agent_disconnected`, `no_observation`
или `clock_skew`. Срок актуальности выбирается по виду сообщения и policy
устройства; heartbeat действует только на локальное устройство своего Agent
и использует время получения сервером. Изменение policy пересчитывает срок,
не меняя `observed_at` и не создавая новый сообщённый переход.

Время наблюдения более чем на 30 секунд впереди сервера не заменяет последнее
достоверное состояние. При этом `clock_skew:true` показывает проблему часов.
Если достоверных сообщений нет, возвращаются `unknown`, `reason:clock_skew`
и `last_reported_online:null`. Флаг ошибки часов может также сопровождать
`online`, `offline` или `stale`, если прежнее достоверное наблюдение сохранено.

Обязательные nullable-поля `last_reported_online`, `observed_at`, `received_at`,
`expires_at`, `age_seconds` возвращаются явно с `null`, когда значения нет.
Пустые поля происхождения сообщения — `evidence_kind`, `source`, `message_id`,
`event_id`, `time_basis` — могут отсутствовать.

История содержит первое принятое сообщение с `from_online:null` и дальнейшие
изменения `true → false`/`false → true`. Повтор того же состояния, истечение
срока и отключение Agent не создают записей. Это история сообщённых переходов,
а не журнал всех измерений и не журнал отказов. Каждая запись содержит
`id`, `agent_id`, `device_id`, `kind`, `source`, `message_id`, `event_id`,
`time_basis`, `from_online`, `to_online`, `observed_at`, `received_at`.
У начальных записей, перенесённых миграцией, `kind:legacy`, а `message_id`
может быть пустой строкой. Пустая история существующего устройства — 200
с `items:[]`; сортировка идёт по `id` от новых к старым.

Пример сводки:

```json
{
  "generated_at": "2026-10-07T12:00:00Z",
  "total": 4,
  "online": 1,
  "offline": 1,
  "stale": 1,
  "unknown": 1,
  "clock_skew": 1,
  "agent_disconnected": 2
}
```

`total = online + offline + stale + unknown`. Диагностические счётчики
`clock_skew` и `agent_disconnected` пересекаются с этими состояниями,
поэтому прибавлять их к total нельзя. Сводка, карточки и известные узлы карты
используют одну модель оценки. В `TopologyNode` поле `availability` есть
у известных устройств; у несопоставленных соседей оно может отсутствовать.
Полный разбор источников и проверок — [этап 2](availability-stage2.md).

## Телеметрия

Карточка также содержит `monitoring` и `telemetry_freshness`; latest metric —
`monitoring` и `freshness`. Статусы свежести: fresh/stale/no_data/clock_skew.
Оценка использует период конкретного устройства. Новый heartbeat не делает
старую telemetry свежей. Настройки сохраняются в backend; применение в Agent
ручное, статус unverified. Полные примеры: [monitoring-intervals.md](monitoring-intervals.md).

Пример последнего измерения:

```json
{
  "id": 42,
  "agent_id": "collector-uuid",
  "device_id": "local:collector-uuid",
  "observed_at": "2026-10-05T16:00:00Z",
  "collected_at": "2026-10-05T16:00:00Z",
  "received_at": "2026-10-05T16:00:01Z",
  "source": "linux",
  "cpu_usage_percent": 23.5,
  "memory_total_bytes": "8589934592",
  "memory_used_bytes": "4294967296",
  "sample": {
    "device_id": "local:collector-uuid",
    "source": "linux",
    "uptime_seconds": 1200,
    "interfaces": [{"id":"eth0", "name":"eth0", "counters":{"rx_bytes":"123456"}}]
  }
}
```

`cpu_usage_percent: null` — значение отсутствует; `0` — измеренный ноль.
Поля RAM — десятичные строки, чтобы не потерять точность uint64 в JavaScript.
Для расчёта процента RAM можно перевести обычные объёмы в `Number`; для точных
счётчиков используй `BigInt`. RAM null означает отсутствие блока MemoryStats.
Не заменяй отсутствующие измерения нулём.

`inventory`, `sample`, `heartbeat` используют protobuf JSON из `agent.proto`:
enum остаются строками `DEVICE_TYPE_...`, int64/uint64 — строками, необязательные
или default-поля могут отсутствовать. Interfaces/rates/metrics полностью
сохраняются внутри `sample`. Общая карточка telemetry содержит краткие CPU/RAM
поля для удобства frontend. Latest выбирается по `observed_at`, не по порядку
прибытия сообщений. `source=simulated` нужно явно обозначать на экране.

Ошибки для объявленных маршрутов:

```json
{"error":{"code":"not_found","message":"device or telemetry not found"}}
```

| HTTP | code | Действие frontend |
|---|---|---|
| 401 | `unauthorized` | предложить новый вход; сессия отсутствует, истекла или отозвана |
| 403 | `forbidden` | показать нехватку прав; не повторять изменение автоматически |
| 403 | `csrf_failed`, `invalid_origin` | проверить CSRF-заголовок и origin в конфигурации/proxy |
| 409 | `username_conflict`, `last_administrator` | исправить логин или сохранить активного admin |
| 429 | `rate_limited` | учесть Retry-After, приостановить попытки входа |
| 400 | `invalid_query` | исправить параметры |
| 400 | `invalid_policy` | передать три целых интервала 3..86400 секунд |
| 404 | `not_found` | показать отсутствие устройства/метрики |
| 409 | `ambiguous_device` | передать `agent_id` |
| 503 | `storage_unavailable` | показать временную недоступность, повторить позже |

Неизвестные пути и неподдерживаемые методы возвращают стандартные HTTP 404/405.
В MVP нет WebSocket/SSE. Для демо достаточно опрашивать список и метрики раз в
3 секунды, прекращая запросы при закрытии страницы.

Пример подключения:

```js
const path = `/api/devices/${encodeURIComponent(device.device_id)}/telemetry`;
const response = await fetch(`${path}?agent_id=${encodeURIComponent(device.agent_id)}`);
if (!response.ok) throw new Error(`Backend returned ${response.status}`);
const telemetry = await response.json();
```

Для frontend на другом локальном порту настрой development proxy `/api`,
`/healthz`, `/readyz` на `http://127.0.0.1:8080`. Так браузер обращается к одному
origin. Backend пока не выдаёт CORS-заголовки; прямой cross-origin fetch без proxy
будет заблокирован браузером. Production frontend можно обслуживать через общий
reverse proxy вместе с API.

## PKI management

OpenAPI0.10.0 включает admin-only /api/pki/bootstrap-tokens,
/api/pki/certificates, /api/pki/audit и revoke routes. Cookie mutations требуют
CSRF. Secret token возвращается только при создании; lists содержат metadata.
Enrollment /enroll и /renew находятся на отдельном HTTPS listener, не основном
REST port. Их формат и запуск — [stage8-pki.md](stage8-pki.md).

## Health и incidents

OpenAPI0.11.0 добавляет /api/health/rules, /api/devices/{id}/health и /api/incidents
с detail/evidence/transitions/acknowledge. Чтение требует обычной роли viewer,
acknowledge — operator/admin и cookie CSRF, body {"confirmed":true}.
Signal — последняя сохранённая оценка; evidence_status сообщает её актуальность.
Аcknowledge не означает recovery и не исполняет команду Agent.
Scope/current отличают предыдущие simulated sessions от текущего запуска.
Подробные DTO/правила/границы — [stage9a-health-incidents.md](stage9a-health-incidents.md).

OpenAPI0.12.0 расширяет эти DTO: resource_kind/id/context в Incident и
HealthObservation, value/unit/resource_evidence/resource_policy в Assessment.
GET interface/SIP health routes и incident resource filters описаны в
[stage9b-network-sip-incidents.md](stage9b-network-sip-incidents.md).
Device health теперь включает портовые компоненты; SIP health остаётся отдельным.
Coverage относится к наблюдённым компонентам, не ко всем возможным проблемам сети.

OpenAPI0.13.0 добавляет agent resource kind и GET /api/agents/{id}/health.
Interface assessment включает persistent flap, а collector evidence описывает
состояние транспорта Backend, не физическую доступность устройств за Agent.
Settings и детали — [stage9c-flap-collector-incidents.md](stage9c-flap-collector-incidents.md).

OpenAPI0.14.0 добавляет reports summary/snapshots. Чтение viewer/operator/admin,
snapshot creation operator/admin, Idempotency-Key и cookie CSRF. Download=true
возвращает JSON attachment. Период [from,to),1сек..31дней, без future end.
Summed incident seconds не равны downtime; availability_percent сейчасnull.
Методика и DTO — [stage9d-reports.md](stage9d-reports.md).

OpenAPI0.15.0 добавляет GET /api/incidents/{id}/context для viewer/operator/admin.
Связи содержат relation/strength/basis/topology links/limitations и causal_claim=false.
Window60..3600с, limit1..50; current topology не выдаётся за историческую карту.
Детали — [stage9e-diagnostic-context.md](stage9e-diagnostic-context.md).

OpenAPI0.16.0 добавляет POST incident/analysis/preview, POST incident/analysis,
GET incident/analyses и GET analyses/{analysis_id}. Preview local-only; внешний
send operator/admin требует frozen request hash, подтверждение, Idempotency-Key
и cookie CSRF. Polling получает queued/running/succeeded/failed/uncertain.
Exact body, data allowlist и ограничения — [stage9f-ai-analysis.md](stage9f-ai-analysis.md).


## B9 — управляемые настройки Collector

Четыре admin-маршрута `/api/collectors/{id}/config`, `/config/history`, `/config/events`; PUT `/config` сохраняет полный desired snapshot и возвращает 202, а применение происходит асинхронно. Подробный формат, пример PowerShell, различие desired/applied и условия mTLS: [B9](backend-v2-b9-collector-configuration.md). Legacy PUT monitoring сохраняет только ожидаемый период backend; управление scheduler выполняется новым API.

## B10 — разрешённые Ansible jobs и snapshots

Контракт OpenAPI `0.31.0` добавляет `/api/automation/templates`, admin `/api/automation/workers`, admin GET/PUT `/api/assets/{id}/automation-target`, GET/POST `/api/assets/{id}/automation/jobs`, GET `/api/automation/jobs/{job_id}`, GET `/events`, POST `/cancel`, GET `/api/assets/{id}/automation/snapshots` и admin GET `/api/automation/snapshots/{snapshot_id}/content`.

Создание read-only job требует operator/admin, `Idempotency-Key`, текущую `target_revision`, `timeout_seconds` 30–600 и `reason`. Возвращается 202; наличие `snapshot_id` в `succeeded` подтверждает сохранённый результат. Одна активная задача на Asset, максимум 100 в очереди/исполнении. Cancel не принимает body: operator отменяет свою задачу, admin — любую. История/события/метаданные доступны viewer; полное configuration/facts только live admin с аудитом. Все новые маршруты возвращают `Cache-Control: no-store`. Неизвестные параметры и поля запрещены; `command`, `playbook`, raw credentials отсутствуют в контракте.

Привязка проверяется по Asset revision и lifecycle/scope; SSH IP, reviewed template, credential_ref и публичный host key задаёт администратор. Snapshot observation time — backend-время получения. Подробно и с примерами: [B10 Ansible](backend-v2-b10-ansible.md). Настоящая Linux/device приёмка отделена от fixtures: [валидация](backend-v2-b10-validation.md).

## B11.1 — approved baseline и configuration drift

OpenAPI `0.32.0`: admin GET/PUT `/api/assets/{id}/configuration/baseline`, admin GET `/baseline/history`; data.read GET `/configuration/drift` и `/drift/history`; monitoring.write POST `/configuration/compare`. Baseline approval требует `Idempotency-Key`, текущие `expected_revision`/`asset_revision`, успешный same-Asset `snapshot_id`, `confirmed=true` и reason. Снимок остаётся encrypted; arbitrary configuration/playbook не принимаются.

Новые B10 snapshots автоматически сравниваются с approved baseline. Общий API возвращает redacted contexts и явное изменение VLAN; full snapshots остаются admin-only с аудитом. `state.status` текущий и учитывает stale/source/target/Asset guards; `state.latest.status` относится к сохранённому snapshot. Предложение восстановления помечается как поддерживаемое backend, но требует включённой B11.2 policy и отдельного admin approval. Reviewed VoIP association — контекст без доказанного port impact. Полные примеры: [B11.1](backend-v2-b11-1-configuration-drift.md).

## B11.2 — remediation policy, approval и verification

OpenAPI `0.36.0` добавляет admin GET/PUT `/api/assets/{id}/configuration/remediation-policy`, data.read GET/command.write POST `/api/assets/{id}/configuration/remediations`, data.read GET `/api/configuration/remediations/{remediation_id}`, admin POST `/approve` и requester/admin POST `/cancel`.

Клиент передаёт только `drift_result_id`, `policy_revision` и reason. Action, интерфейс и VLAN backend извлекает из свежего текущего redacted drift. Одобрение требует отдельный `Idempotency-Key`, `confirmed=true`, точную revision и timeout 30–600. Перед запуском и каждую секунду исполнения backend повторно проверяет текущий Asset, baseline, target, evidence, policy, live session и роль. Разрешён только `restore_access_vlan` на физическом access-порту Cisco IOS/Arista EOS; worker после изменения сразу читает running config, а состояние становится `verified` только после нового compliant drift result. Подробно: [B11.2](backend-v2-b11-2-remediation.md).

## Корреляция B12.1

Добавлены `/api/correlation/summary`, `/incidents`, `/incidents/{id}`, `/services`, `/services/{id}` и admin `/rebuild`. Префикс всех маршрутов — `/api/correlation`. Полный контракт — OpenAPI 0.36.0, объяснение lifecycle и ограничений — [B12.1](backend-v2-b12-1-correlation.md). Старый `/api/incidents` продолжает возвращать исходные инциденты.

Prometheus Settings теперь принимает opt-in `alerts_enabled`. Firing alerts доступны через существующие `/api/signals`, `/api/assets/{id}/signals`, source signal history и `/api/correlation/incidents`. [Границы lifecycle](backend-v2-b12-2a-prometheus-alerts.md).

B12.2b добавляет admin GET/PUT `/api/data-sources/{id}/alertmanager` и source-scoped POST `/api/data-sources/{id}/alertmanager/webhook`. [Настройка и точные ограничения matching](backend-v2-b12-2b-alertmanager.md).

## Завершение B12–B16

Hardware, reported log/audit, approved configuration drift и typed topology peer-change producers подключены к общей корреляции. Подробности и границы восстановления — [итог B12](backend-v2-b12-final.md). Старые leaf incidents сохранены, correlated case объединяет их с evidence/timeline и не объявляет доказанную первопричину.

Multi-source AI: POST `/api/correlation/incidents/{id}/analysis/preview`, POST `/api/correlation/incidents/{id}/analysis`, GET `/api/correlation/incidents/{id}/analyses`. Отдельное подтверждение, hash и Idempotency-Key обязательны для job; refs связывают ответ с локальными фактами. [B13](backend-v2-b13-ai.md).

Reports: GET `/api/reports/v2/summary`, GET/POST `/api/reports/v2/snapshots`, GET `/api/reports/v2/snapshots/{id}`. GET/POST `/api/reports/schedules` и PUT `/api/reports/schedules/{id}` требуют admin; daily/weekly используют завершённые UTC периоды. Episode counts/durations не являются измеренной availability. [B14](backend-v2-b14-reports.md).

B15 добавляет внутренний MetricStore, сохраняя прежний REST JSON метрик. [Передача frontend и итоговые ограничения](backend-v2-final-handoff.md), [проверки безопасности/восстановления](backend-v2-b16-security-recovery.md).
