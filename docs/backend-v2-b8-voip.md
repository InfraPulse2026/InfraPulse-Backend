# B8.3 — SIP/VoIP по Asset

## Привязка сервиса

SIP target принадлежит Collector, а не автоматически физическому устройству. Backend требует явное решение администратора: какой существующий inventory binding соответствует наблюдённой SIP-точке. Hostname/IP не используются для угадывания оборудования.

Для реальных данных inventory может быть от другого источника, например Zabbix. `source_instance_id` — Collector, выполняющий SIP OPTIONS; `inventory_source_instance_id` — источник выбранной инвентаризации. Симуляция связывается только с binding того же Collector и той же сессии.

После входа администратора и получения актуальных Asset revision/bindings:

```text
POST /api/assets/{asset_id}/voip
Authorization: Bearer <session token>
Idempotency-Key: review-pbx-001
Content-Type: application/json
```

```json
{
  "binding_id": "binding_...",
  "agent_id": "<existing Collector ID>",
  "target_id": "<observed SIP target ID>",
  "address": "pbx.example:5060",
  "transport": "udp",
  "source": "<exact observed source>",
  "scope": "",
  "service_kind": "pbx",
  "name": "АТС офиса",
  "expected_asset_revision": 1,
  "expected_revision": 0,
  "enabled": true,
  "confirmed": true,
  "reason": "Проверили адрес и inventory привязку с владельцем АТС"
}
```

Значения в примере — placeholders. Использовать ID, address, transport и source из своей уже принятой inventory/SIP-истории. Для update: PUT /api/assets/{id}/voip/{association_id}, expected_revision — актуальная revision связи; для новой записи — 0.

Внутри транзакции повторно проверяются live admin/session, lifecycle/revision Asset, существующий binding, scope, источник Collector и точное последнее квалифицированное наблюдение. SQL trigger также проверяет владельца и reviewed sample. Ошибка audit отменяет запись. Повтор того же idempotency key/body возвращает сохранённый результат; другой body с тем же key — 409. Изменение роли или logout запрещает запись даже при устаревшем HTTP principal.

## Карточка и история

```text
GET /api/assets/{id}/voip?limit=50&offset=0
GET /api/assets/{id}/voip/{association_id}/history?limit=50
GET /api/assets/{id}/voip/{association_id}/history?before_id=123
GET /api/assets/{id}/voip/{association_id}/events?limit=50
```

data.read разрешает viewer/operator/admin чтение. Карточка и events: limit 1–100, offset до 100000. Sample history: limit 1–200, before_id, RFC3339 from/to; если заданы обе границы, диапазон не больше 30 дней. ID курсор сортирует историю по порядку сохранения, времена наблюдения остаются отдельными.

Карточка возвращает существующее reviewed physical placement B6, topology_node_id=Asset ID и items сервисов. В сервисе есть latest/clock_issue, health, monitoring expected interval, observed_endpoint, mapping_status/reason и не больше 20 существующих SIP-инцидентов с incidents_truncated. Неизвестные optional response_code/latency остаются null, измеренный 0 сохраняется.

Mapping statuses: active, disabled, review_required, session_expired, source_disabled, no_observation. Смена address/transport/source не переносит автоматически старое оборудование на новую SIP-точку. Новый endpoint показывается как observed_endpoint, а при review_required latest этой карточки пустой до пересмотра. Отключение связи сохраняет прежнюю проверенную endpoint identity.

История association относится к текущему проверенному endpoint; предыдущий endpoint сохраняется в прежней SIP-истории и immutable mapping events. Source/provenance/scope вычисляются по исходному сохранённому envelope, поэтому новый запуск симулятора не обновляет реальный Asset. Старый global latest SIP pointer не используется для выбора latest в новой карточке.

## Merge, split и инциденты

Association закреплена за binding. При объединении/разделении Asset связь следует за привязкой. Физическое размещение возвращается из текущего Asset, а не копируется в отдельную таблицу SIP. При необходимости администратор может исправить physical binding с подтверждением и актуальными revisions.

Показанные инциденты — существующие правила конкретного Collector/target/source/scope/endpoint. Это контекст, а не ещё реализованная глобальная корреляция B12. Просроченное соединение или данные отражаются в evidence_status, без выдуманного recovery.

## Проверки и границы

Миграция 030 создаёт associations, events, idempotency operations и immutable функцию определения SIP scope по сохранённому envelope. Тесты проверяют null/zero, revisions/concurrency/idempotency, audit rollback/live admin, endpoint change, merge/split, session expiry и explicit cross-source связь с inventory Zabbix. SIP incidents также разделяются по typed simulation provenance.

Новый SIP stack и сборщик не добавлены: OPTIONS уже делает неизменённый Collector. Проверка на настоящей АТС остаётся практической приёмкой команды. Управление конфигурацией Collector, Ansible и общая корреляция реализуются на следующих этапах.
