# B5 — Prometheus connector и запросы графиков

Дата: 11 октября 2026. Backend подключается к выбранным Prometheus sources,
получает targets, публикует Assets/SourceBindings и последние значения,
предоставляет instant/range/metadata для frontend. Agent/Collector `215d33a` и
frontend не изменены, commit/push не выполнялись.

Целевая версия команды — Prometheus **3.15.0**. Проверена совместимость формата
на HTTPS API fixtures; рабочий сервер/endpoint команды пока не предоставлен.
Текущий адаптер принимает стабильный API3.x и отвергает другие major/prerelease.
Это не обещание приёмки каждой версии 3.x или реального стенда.

## Что происходит

1. Администратор сохраняет source `PROMETHEUS`, HTTPS base endpoint, настройки
   CA/auth и список конкретных scrape pools. Источник использует прежний
   network allowlist, pinned DNS и проверку TLS. Bearer token/Basic password
   хранится зашифрованным в прежнем credential field.
2. `test` проверяет `/api/v1/status/buildinfo`, версию и доступ. `sync` получает
   `/api/v1/targets?state=active&scrapePool=...` для выбранных pools и фильтров.
3. Target identity вычисляется из source-owned scrape pool и заданных identity
   labels. `job` и `instance` обязательны; `cluster`/namespace добавьте при
   неоднозначности. Source instance сохраняется отдельно. Одинаковый instance
   у разных Prometheus не означает один физический сервер.
4. Target создаёт Asset через общую B3 projection. Health up/down — scrape health,
   не physical online/power. Нет подключения backend к scrapeUrl/exporter.
5. Scoped instant queries получают выбранные метрики. Latest snapshots вместе
   с inventory/cursor/job completion сохраняются атомарно. При ошибке данные
   остаются; source status/quality показывают проблему.
6. График вызывает query_range у этого Prometheus по проверенному SourceBinding.
   Полученный диапазон не копируется в PostgreSQL. Для source metadata используется
   bounded `/api/v1/targets/metadata`, не полный экспорт всех метрик.

Официальный wire contract: [Prometheus HTTP API](https://prometheus.io/docs/prometheus/latest/querying/api/).
Применяется официальный PromQL parser v0.315.0, соответствующий 3.15.0:
[Prometheus source](https://github.com/prometheus/prometheus/tree/v3.15.0/promql/parser).
Backend не строит собственную TSDB и не заменяет сбор существующего Prometheus.

## Настройка

Сетевые переменные и CA mapping — как в [B2.3](backend-v2-b2-network.md).
Endpoint — base URL, например `https://prometheus.example:9090` либо
`https://monitor.example/prometheus`, а не путь query API. Direct HTTP не
поддерживается; лабораторному серверу нужен проверяемый HTTPS frontend.

Пример source (адрес и имя pool заменить своими):

```json
{
  "type": "PROMETHEUS",
  "name": "Prometheus lab",
  "endpoint": "https://prometheus.example:9090",
  "enabled": true,
  "sync_interval_seconds": 3,
  "configuration": {
    "timeout_seconds": 15,
    "tls_ca_ref": "lab",
    "prometheus": {
      "scrape_pools": ["node_exporter"],
      "label_match": {"environment": "lab"},
      "identity_labels": ["job", "instance"],
      "page_size": 5,
      "max_targets": 200,
      "auth_mode": "none",
      "device_type": "server",
      "profile": "node_exporter",
      "verified_identity_labels": false
    }
  }
}
```

Auth modes:

- `none`: credential отсутствует/очищен; username пусто.
- `bearer`: credential — token, username пусто.
- `basic`: username задаётся в settings, credential — password.

Для credential нужен исходный стабильный `DATA_SOURCE_ENCRYPTION_KEY`; секрет
не помещается в endpoint/query/settings. `tls_ca_ref` — ссылка на публичный CA
из deployment, не путь и не insecure TLS flag. Source create/update/sync — admin.
Изменение settings — PUT с последним revision; старый async результат не
публикуется. Configured expression/query responses не выводятся в error logs.

При `profile=custom` укажите mappings:

```json
{
  "id": "api_latency",
  "metric": "latency",
  "unit": "seconds",
  "expression": "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))",
  "multiplier": "1",
  "max_age_seconds": 300,
  "metadata_metric": "http_request_duration_seconds_bucket"
}
```

Выбираются normal metric/unit из B4.2; availability может представлять readiness
приложения, если mapping соответствует его семантике. Максимум 16 mappings.
Multiplier положительный decimal ≤1e9; default1. Max age 3..86400, default300.
Expression ≤4096 bytes, parsed AST ≤200 nodes/depth24. Нужен хотя бы один
реальный vector selector; pure constants, @ timestamp modifiers, negative offsets,
range/offset >30 days, экспериментальные parser features не разрешены.

Backend добавляет exact matchers **к каждому** selector в AST, включая обе
части выражений CPU/RAM/network. Исходные matchers сохраняются и могут сузить
выборку ещё сильнее. Экранирование label values выполняет parser, строковой
подстановки query templates нет. Viewer выбирает только mapping_id, а не
собственный PromQL/URL. Tenant-wide ограничения этой установки по прежней
модели RBAC глобальны; отдельная multi-tenant авторизация здесь не заявляется.

## Профиль node_exporter

При пустом mappings встроенный профиль даёт:

- up → availability;
- CPU utilization из idle rate за 5 минут;
- RAM utilization из available/total;
- суммарный disk utilization с исключением tmpfs/overlay/squashfs/proc/sysfs;
- RX/TX rate всех interfaces кроме lo;
- RX+TX error rate.

Это стартовые выражения, не универсальная enterprise конфигурация. Выберите
нужные filesystems/interfaces и исключите duplicate mountpoints/виртуальные
устройства своим mapping. На холодном exporter rate может ещё не иметь данных.
Для unsupported/нескольких series не выбирается случайное значение.

Профиль дополнительно читает scoped node_uname_info/node_dmi_info для hostname,
OS/kernel/architecture и доступных manufacturer/model/serial/UUID. Нет выдуманных
hardware IDs при отсутствии DMI. Для custom targets такие значения берутся
только из явно указанных hostname/manufacturer/model/serial/uuid label names.
Instance не превращается автоматически в hostname/management IP/hardware UUID.

Typed identities по умолчанию medium, не причина автоматического merge.
`verified_identity_labels=true` — явное подтверждение доверия выбранному exporter/
labels, повышает serial/UUID evidence до high. Automatic resolver всё ещё default
off и требует B3.3 corroboration, свежесть, отсутствие конфликтов и единственного
кандидата. Ручной merge/split доступен как прежде; links следуют за binding.

## Что хранится

Migration025 добавляет **только latest** `prometheus_measurements`. Poll не
увеличивает число строк для одной target/mapping пары. Zabbix snapshot history
не меняется. Полный query_range ответ не сохраняется в эту таблицу.

Target ID: `prom_target_<SHA256(pool+identity_labels)>`, metric ID:
`prom_metric_<SHA256(target_id+mapping_id)>`. Другой source ID — другой namespace.
Добавление/удаление target не меняет IDs остальных. При duplicate identities
нужны дополнительные configured labels; backend не объединяет их по имени/IP.
Исчезнувший target не удаляется автоматически и не считается выключенным.

Safe provider state содержит identity/выбранные labels, source revision/API version,
scrape health/interval/last scrape. DiscoveredLabels, raw lastError, scrapeUrl/
globalUrl и credentials не сохраняются и не вызывают сетевых подключений.
Scrape/revision clocks не создают новую inventory fact version сами по себе.
Source policy edit остаётся в существующем audit.

Latest values — decimal strings/null; NaN/Inf остаются explicit no numeric data,
а не zero. `observed_at` для Prometheus — время **PromQL evaluation**,
`scraped_at` — отдельно reported target scrape time. Свежесть latest проверяется
по обоим и по source status/revision. Повторный query не омолаживает старый scrape.
Даже успешный scrape не доказывает свежесть каждого independently timestamped
application metric; semantics конкретного expression остаются ответственностью
администратора. Backend не объявляет physical online из scrape health/up.

## REST для frontend

Все reads — data.read (viewer/operator/admin). Новые endpoints:

```text
GET /api/assets/{id}/prometheus/instant
GET /api/assets/{id}/prometheus/range
GET /api/assets/{id}/prometheus/metadata
```

Обязательные query params: `source_instance_id`, `target_id`, `mapping_id`.
Target ID берите из Asset source binding `external_id`; mapping_id — из source
settings. Для range: `from`, `to` RFC3339, `step_seconds` default60 (3..86400),
range ≤14 days, максимум3000 evaluation points. Дублированные/неизвестные params,
raw query, endpoint, token отвергаются до обращения к источнику.

```text
GET /api/assets/asset_.../prometheus/range?source_instance_id=src_...&target_id=prom_target_...&mapping_id=cpu&from=2026-10-11T12:00:00Z&to=2026-10-11T13:00:00Z&step_seconds=60
```

Ответ: source/revision/Asset/target/mapping IDs, metric/unit, kind,
`time_semantics=promql_evaluation`, points и metadata. Point quality `valid` /
`non_finite` / `invalid_value`; top quality `complete`, `partial`, `no_data`,
`unsupported`, `ambiguous`. **Complete означает полноту ответа, не здоровье**.
Исторические graph points не помечаются fresh. Native histograms напрямую не
нормализуются; выберите expression, возвращающий float/scalar/vector.

Multiple series → ambiguous с пустым points, без случайного графика. Query API
limit2, response≤4MiB, explicit timeout и максимум4 одновременно выполняемых
операций backend ограничивают нагрузку. Prometheus warnings отвергаются как
partial_response; remote error/info text не возвращается пользователю.

Source binding проверяется до запроса. После ответа повторно проверяются
source enabled/revision и принадлежность binding этому Asset. Изменение settings,
disable или merge во время запроса отбрасывает устаревший ответ с409.
Source credentials не выдаются через REST.

Прежние latest routes `/api/assets/{id}/external-metrics` и
`/api/data-sources/{id}/metrics` теперь возвращают также Prometheus snapshots.
`/api/data-sources/{id}/metrics/history` для Prometheus получает409
`history_on_demand` с указанием range endpoint. Zabbix history продолжает работать.
OpenAPI **0.26.0** описывает schemas, routes и параметры.

## Ограничения и проверка

Selected scrape pools: до8. Max selected targets до200, page1..5, identity labels
до8 с обязательными job/instance, exact label filters до16. Targets endpoint не
является paginated API: сначала приходит bounded response для pool, затем backend
обрабатывает порцию. Очень большой pool/response не считается успешным — сузьте
Prometheus scrape pools/labels или scope. REST body limit8KiB сохраняется.
Интервал3..86400 — между committed pages, не интервал scrape Prometheus.

Unit fixtures проверяют nested selector scoping/escaping, запрещённые expressions,
profiles, opaque identities, range bounds, token handling, no URL/secret leaks,
instant/range/metadata, NaN, multiple-series ambiguity, warning errors и stale
scrape. PostgreSQL + настоящий защищённый HTTPS transport к API fixture проверяют
Asset/latest/cursor commit, отсутствие fake Agent/копирования history, range,
foreign Asset refusal и revision change during request. API tests проверяют
roles/401/bounds/no viewer raw expressions. Общие tests сохраняют Agent/Collector,
Zabbix/PKI/commands/rollback/старые protected REST.

Fixture сервер имитирует Prometheus3.15.0; настоящий Prometheus/node_exporter и
конфигурация предприятия ещё не приняты. Docker/race/Linux-only process suite,
real hardware/BMC и production scaling не подтверждены. Prometheus alert ingestion
и multi-source incident correlation не добавлены: это B12. Все дальнейшие B6–B16
остаются отдельными этапами, backend v2 целиком не объявляется готовым.
