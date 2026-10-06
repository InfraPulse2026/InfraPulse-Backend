# Контракт для frontend

Базовый URL: `http://127.0.0.1:8080`. OpenAPI: `/openapi.json`.
Имена полей — `snake_case`. Все времена в REST-карточках — UTC RFC3339.

| Метод и путь | Ответ |
|---|---|
| `GET /healthz` | `{"status":"ok"}` — процесс работает |
| `GET /readyz` | `{"status":"ready"}` или 503 — проверка PostgreSQL |
| `GET /api/agents` | `{"items":[...]}`; максимум 1000 записей |
| `GET /api/devices?limit=100&offset=0&agent_id=...` | `{"items":[...],"limit":100,"offset":0}` |
| `GET /api/devices/{id}` | карточка одного устройства |
| `GET /api/devices/{id}/inventory` | последняя полная Device-модель из protobuf |
| `GET /api/devices/{id}/monitoring` | ожидаемые интервалы и YAML-фрагменты для ручной настройки Agent |
| `PUT /api/devices/{id}/monitoring` | сохранить три ожидаемых интервала в диапазоне 3..86400 секунд |
| `GET /api/devices/{id}/telemetry` | последнее измерение |
| `GET /api/devices/{id}/metrics` | тот же ответ, что `/telemetry` |
| `GET /api/devices/{id}/metrics/history?limit=100` | `{"items":[...],"limit":100}`, новые измерения первыми |

`limit` от 1 до 1000, `offset` от 0 до 1 000 000. Пустые списки всегда `[]`.
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
  "agent_connected": true,
  "last_seen": "2026-10-05T16:00:00Z",
  "received_at": "2026-10-05T16:00:01Z",
  "inventory": {"id": "local:collector-uuid", "os": "linux"}
}
```

`online` — последнее сообщённое состояние устройства. `agent_connected` — связь
с Controller и свежесть входящих сообщений. Если связь потеряна, frontend
показывает недоступность коллектора/устаревшие данные, а не придумывает отказ
каждого наблюдаемого устройства. `received_at` — время приёма backend;
`last_seen` — последнее наблюдение устройства. Эти времена особенно различаются
у Simulator с фиксированной временной шкалой.

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
