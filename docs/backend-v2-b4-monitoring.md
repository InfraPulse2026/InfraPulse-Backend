# B4.2 — метрики, problems/events Zabbix и внешние инциденты

Дата: 11 октября 2026. Изменён только backend. Agent `215d33a` и frontend
не редактировались; GitHub не обновлялся. Рабочий Zabbix endpoint и host/item IDs
нам пока не предоставлены.
Результат проверен на HTTPS JSON-RPC fixtures, PostgreSQL и авторизованном REST.
Целевые версии API по сообщению команды: 7.4.15/7.0.31, также сохранён формат 6.0.

## Что добавлено к B4.1

Раньше Zabbix connector умел проверять доступ и получать выбранный inventory.
Теперь один sync может дополнительно получить выбранные числовые item values,
страницу активных problems, страницу исторических problem events и изменения
ранее открытых событий. Нормализованные сигналы создают обычные InfraPulse
Incidents, доступные через прежние list/detail/acknowledge/evidence/transitions.
Source instance и event ID сохраняются; выдуманный Agent для Zabbix не создаётся.

Подключение и network/TLS policy остаются как в [B4.1](backend-v2-b4-zabbix.md).
Read-only token дополнительно требует `item.get`, `problem.get`, `event.get`,
`trigger.get`. Нет `event.acknowledge`, item/host updates, scripts или remediation
Zabbix. Backend acknowledgment действует только в InfraPulse.

Выбор API основан на официальных документах:
[item.get](https://www.zabbix.com/documentation/7.4/en/manual/api/reference/item/get),
[problem.get](https://www.zabbix.com/documentation/7.4/en/manual/api/reference/problem/get),
[event.get](https://www.zabbix.com/documentation/7.4/en/manual/api/reference/event/get),
[trigger.get](https://www.zabbix.com/documentation/7.4/en/manual/api/reference/trigger/get).
`problem.get` даёт unresolved/недавно resolved problems, а `event.get` — историю.
У событий есть recovery reference `r_eventid`:
[event object](https://www.zabbix.com/documentation/7.4/en/manual/api/reference/event/object).

## Как включить

Старые B4.1 источники продолжают inventory sync: дополнительный monitoring
по умолчанию выключен. Через существующий `PUT /api/data-sources/{id}` добавьте
в `configuration.zabbix` блок `monitoring`, сохранив остальные поля и текущий
`revision`. Токен не нужно повторять при неизменённом endpoint.

```json
{
  "timeout_seconds": 15,
  "tls_ca_ref": "lab",
  "zabbix": {
    "host_ids": ["10601", "10602"],
    "page_size": 20,
    "device_type": "switch",
    "verified_inventory": false,
    "monitoring": {
      "enabled": true,
      "event_page_size": 20,
      "event_lookback_seconds": 86400,
      "item_mappings": [
        {
          "host_id": "10601",
          "item_id": "42001",
          "metric": "cpu.utilization",
          "source_unit": "%",
          "unit": "percent",
          "multiplier": "1",
          "max_age_seconds": 300
        },
        {
          "host_id": "10602",
          "item_id": "42002",
          "metric": "latency",
          "source_unit": "ms",
          "unit": "seconds",
          "multiplier": "0.001",
          "max_age_seconds": 300
        }
      ]
    }
  }
}
```

Все ID — примеры, их нужно заменить настоящими host/item IDs своего Zabbix.
Item mapping должен ссылаться на host из selection. Нельзя передать item другого
host, повторить item ID или угадать unit по display name. Слишком большой input
отклоняется; общий REST body limit остаётся 8 KiB.

Поддерживаемые значения `metric → unit`:

| Metric | Unit |
|---|---|
| cpu.utilization, memory.utilization, disk.utilization | percent |
| memory.used, memory.total, disk.free | bytes |
| network.rx, network.tx | bytes_per_second |
| network.errors | per_second |
| latency | seconds |
| availability | boolean |

`source_unit` должен точно совпадать с `units` item Zabbix; пусто означает,
что ожидается отсутствие units. `multiplier` — положительная decimal string
до 1e9, default `1`. Конверсия явная: например ms → seconds `0.001`, bits/s →
bytes/s `0.125`. Нельзя брать сырой traffic counter и считать его rate только
умножением: выберите уже вычисляемый Zabbix item с правильной семантикой.
CPU idle → utilization также не выполняется multiplier: нужен item utilization.

До 100 mappings; max_age 3..86400 секунд, default 300. Poll interval backend
не меняет частоту сбора самого Zabbix. `item_mappings=[]` допускается: problems/
events работают, числовые values не запрашиваются. Произвольные log/text items,
полные history/trends и все item properties не загружаются.

## Последовательность sync

1. Проверка версии и получение очередной порции inventory.
2. `item.get` только для явно выбранных items этой порции, с коротким output
   allowlist. Отсутствующий/неподдерживаемый item — explicit quality, не ноль.
3. Для каждого host: страница `problem.get(recent=false)` с event ID keyset
   и страница `event.get(value=1)` выбранного history window.
4. До 100 ранее открытых source events повторно проверяются по event IDs;
   отдельный persistent cursor обеспечивает ротацию большого набора.
5. Загружаются bounded trigger names/status и только необходимые recovery events.
6. Inventory, metrics/signals/history, common incident/evidence, cursors и job
   completion сохраняются в одной транзакции. Ошибка SQL откатывает всё.

Monitoring ограничивает host page до `min(page_size,5)`, чтобы число RPC и
payload оставались ограниченными. Для 20 hosts и interval 3s полный inventory/
monitoring обход занимает примерно 12s плюс HTTP/очередь. У большой event history
порции также могут требовать несколько обходов. `succeeded` подтверждает
сохранение этой порции, а не завершение всей истории установки.

Event page 1..100, default 20. Первый history window — последние 300..2592000
секунд, default 86400. Во время обработки полной страницы сохраняется keyset
`eventid_from=last+1`. После завершения окна следующие запросы повторяют последние
5 минут; business dedup исключает повторные записи. Старые active events
проверяются отдельно от history window, даже после его сдвига.

Это bounded polling, а не непрерывный гарантированный event stream. Retention
Zabbix, удаление событий housekeeper, права API и запаздывание больше overlap
могут ограничить историю. Изменение settings сбрасывает cursors и начинает
выборочный backfill заново; сохранённые данные не удаляются.
Используйте новый source instance для другой установки Zabbix: одинаковые IDs
разных установок не должны объявляться одним источником. Перенос endpoint на
другой frontend той же установки требует проверки оператором.

## Как хранится и показывается метрика

`value` — нормализованная decimal string либо `null`. `raw_value` — числовое
значение Zabbix до multiplier. Для uint64 не используется преобразование через
float64, поэтому `18446744073709551615` сохраняется точно. Дробные результаты
конверсии округляются до 12 десятичных знаков; исходное число остаётся отдельно.
Timestamp включает source nanoseconds, которые также сохранены отдельным полем.

Quality: `fresh`, `stale`, `disabled`, `unsupported`, `non_numeric`,
`unit_mismatch`, `no_data`, `clock_skew`, `invalid_value`, `unavailable`.
При чтении latest добавляются `source_unavailable`/`configuration_changed`;
возраст проверяется по времени измерения Zabbix, не по времени очередного poll.
Даже повторно считанный старый lastvalue может быть stale. Известный stale
numeric value сохраняется с quality; неизвестное значение остаётся null.

Хранятся выбранные полученные snapshots, а не полная история Zabbix. Повторный
poll одинаковых value/time/quality не добавляет sample. Изменение качества или
других business fields может создать новую версию той же source measurement.

## Problems и Incidents

External signal key: `(source_instance_id,event_id)`. Один event может относиться
к нескольким выбранным hosts; membership сохраняется по authoritative host
filter API. Asset IDs вычисляются через текущие source bindings. Merge/split
Assets не ломает связи и не требует переписывать всю event history.

Сохраняются trigger/event IDs, names/status, severity 0..5, source acknowledgment,
suppression, problem/recovery timestamps, recovery event ID и history versions.
Оригинальная severity сохраняется целиком. Существующий Incident engine имеет
две coarse severity: Zabbix 0..3 → warning, 4..5 → critical. Поэтому UI должен
показывать исходную severity из signal, если важна точная категория Zabbix.

Новый active event открывает один common Incident с `resource_kind=external`,
`source=zabbix`, `source_instance_id`, `external_event_id`; agent_id/device_id
в БД null. Повторный poll обновляет evidence freshness, не открывает дубликат.
Local acknowledgment не является source acknowledgment и не вызывает Zabbix API
mutation. Явный recovery `r_eventid` с проверенным recovery event закрывает
тот же incident. Уже resolved event не открывается заново от stale active reply.
Первый импорт уже закрытого события создаёт historical resolved incident.

Отсутствие problem в новой порции, пустой ответ, API timeout/ошибка либо удаление
event **никогда не означают восстановление**. Прежние данные остаются, а
freshness становится stale/source_unavailable. Missing recovery reference
не закрывает событие: невалидная порция не коммитится. Проверяются обычные trigger
problem/recovery events `source=0,object=0`; сложная корреляция/symptoms,
`c_eventid` и глобальное связывание разных sources относятся к B12.

Existing reports читают внешние Incidents; source creation/recovery times могут
предшествовать импорту backend, и их длительность не равна измеренному downtime.
Старый локальный IncidentContext пока не делает global external correlation.
AI preview для external incident возвращает 409 `external_analysis_unavailable`:
осмысленный source-aware export реализуется в B12/B13; пустой legacy export не
отправляется модели. Прежний AI flow для Collector incidents сохранён.

## REST для frontend

Все новые маршруты требуют `data.read`; viewer/operator/admin могут читать.
Limit 1..200, default 50; offset 0..100000. Ответ: `{items,limit,offset}`.

| GET | Назначение |
|---|---|
| /api/assets/{id}/external-metrics | latest external metrics текущей карточки |
| /api/assets/{id}/signals | связанные external signals; optional state |
| /api/data-sources/{id}/metrics | latest выбранных items; optional item_id |
| /api/data-sources/{id}/metrics/history | полученные snapshots; optional item_id |
| /api/signals | optional source_instance_id, asset_id, state=active/resolved |
| /api/data-sources/{id}/signals/{event_id}/history | business-state versions |

Источник записи всегда виден. Для merged alias переходите к его `merged_into_id`.
В Asset metrics пока только external данные этой части B4; это не объединённая
история всех Collector metrics. Normalized SourceBinding остаётся основой.
Новые поля/маршруты опубликованы в локальном OpenAPI **0.25.0**.

Просмотр common incidents:

```text
GET /api/incidents?source=zabbix&resource_kind=external
GET /api/incidents/{incident_id}
GET /api/incidents/{incident_id}/evidence
GET /api/incidents/{incident_id}/transitions
POST /api/incidents/{incident_id}/acknowledge  {"confirmed":true}
```

Acknowledge требует operator/admin и CSRF при cookie authentication; Bearer
используется по прежним правилам. Создание/изменение source и запуск sync — admin.
При source error последние samples/signals остаются доступны с честным quality.

## Проверка и дальнейшие шаги

Миграция 024 добавляет metric snapshots/latest, external signals/host memberships/
versions и безопасно расширяет существующие incidents, сохраняя legacy identity
constraints. Прежние Collector records и foreign keys не отменены.

Unit fixtures: API6.0/7.0.31/7.4.15, item units/precision/null, active/history
cursor, explicit recovery lookup, error redaction и cross-host rejection.
PostgreSQL: dedup/history, common acknowledgment, отсутствие fake recovery,
source error freshness, dynamic Asset merge links и полный rollback при отказе
записи incident evidence. TLS/REST E2E: encrypted token → read-only HTTPS API →
настоящая очередь workers → PostgreSQL → authorized Asset metrics/signals/common
Incident/recovery. Unauthorized reads и limits проверены отдельно.

Это тестовый JSON-RPC сервер, не настоящая установка Zabbix. Реальный endpoint,
права и реальные host/item IDs пока отсутствуют; этот live acceptance остаётся
открытым. Также не подтверждены actual device/BMC outages, Docker/race/Linux-only
process suite. Сам Agent не менялся; его regression выполняется общим набором.

Для следующего B5 команда указала Prometheus 3.15.0. Внешние модели и source
bindings уже подходят для его snapshots/signals; Prometheus connector пока
не реализован. Финальный backend v2 не объявляется полностью готовым.
