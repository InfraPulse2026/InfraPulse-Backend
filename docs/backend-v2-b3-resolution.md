# B3.3 — resolver, внешний publisher и legacy backfill

Дата: 11 октября 2026. Agent/Collector не изменён; GitHub не публиковался.
Production Zabbix connector ещё не реализован — это следующий B4.

## Что добавлено

- Осторожное автоматическое присоединение source binding к единственному
  совместимому Asset и журнал использованных fact IDs/времён наблюдений.
- Общая внутренняя `assets.Observation` для будущих внешних адаптеров.
- Atomic sync completion: observations, Asset projections, cursor и job state
  сохраняются в одной транзакции. Ошибка projection откатывает всю страницу.
- Команда `backfill-assets` читает архивные AgentHello/InventoryUpdate с
  исходными sessions, без отключения Collector или перезаписи legacy devices.

Миграция 022 добавляет `asset_resolution_events` и `asset_projection_marks`.
Live ingest отмечает projected/skipped сообщения в своей транзакции, поэтому
backfill не воспроизводит уже обработанные сообщения.

## Включение автоматического режима

Режим по умолчанию выключен. После проверки данных стенда задайте окружение:

```dotenv
ASSET_AUTO_RESOLUTION_ENABLED=true
ASSET_RESOLUTION_MAX_AGE=24h
```

Возраст фактов — 1 минута..30 дней, Compose передаёт обе переменные.
Настройка применяется при старте backend и не меняет YAML Agent.

Рассматриваются только реальные пустые scopes и одиночные `source_owned`
bindings без ручных решений. Manual/automatic bindings не переназначаются.
Существующая одиночная карточка проверяется при новом inventory; массовой
автоматической переразметки нет.

## Правила

1. Известный entity kind должен совпадать. BMC/chassis/system/host не смешиваются.
2. Candidate по current high root identity должен быть единственным. При
   нескольких совпадениях сохраняется unresolved и событие ambiguous.
3. Для UUID-based match нужны typed high UUID с обеих сторон и дополнительно
   совпавшие валидный serial и непустой manufacturer.
4. Для network_device вместо UUID допустим high serial в namespace производителя
   с точным совпадением непустой model. Legacy serial без high evidence недостаточен.
5. IP/hostname/model сами по себе не объединяют. Placeholder/нулевые/redacted
   identities исключены. Serial/UUID/manufacturer contradictions блокируют решение.
6. Все current snapshots target группы должны быть свежими; degraded/unsynced
   clocks не участвуют. Unknown допускается с проверкой возраста.
7. Новый binding — от другого source instance; один источник не объединяется
   сам с собой при разных external IDs. Максимум 128 bindings в группе.
8. Canonical source сохраняется, старый одиночный Asset становится alias.
   Manual decisions/separations не отменяются. Новое наблюдение, подходящее
   к двум разделённым карточкам, остаётся неоднозначным.

Правило доверяет корректности typed confidence нормализующего источника,
а не доказывает физическую уникальность UUID криптографически. Cloned IDs,
неверный inventory и vendor/model aliases требуют review. Именно поэтому
режим включается явно и сохраняет консервативные ограничения.

## Журнал решений

`GET /api/assets/{id}/resolution-events?limit=50&offset=0` требует `data.read`.
Viewer/operator/admin читают binding, source/target Asset IDs, source/target fact
IDs, использованные времена inventory и code: matched, ambiguous, separated,
conflict, stale, insufficient_evidence. Max limit 200, offset 0..100000.

Одинаковые отказы по тому же snapshot не размножаются на каждом poll. Fact ID
может обозначать первую версию неизменившихся business facts, поэтому времена
последнего наблюдения решения сохраняются отдельно. Raw history сохраняется
после переноса binding. Ручной журнал B3.2 остаётся отдельно. OpenAPI 0.23.0.

## Внешний publisher

Adapter возвращает `datasources.Result.Observations` и `Cursor`. Это внутренний
Go-контракт, не открытая REST ручка загрузки любых устройств. Source instance
берётся из fenced job, не из observation. External bindings имеют
`legacy_agent_id/device_id=null`: Zabbix host не создаёт выдуманный Agent.

Observation содержит external type/ID, observed_at, provenance/simulation scope
и нормализованный Device snapshot. Общая inventory shape применяется внутри
backend; Agent.proto и правила Agent stream не меняются. Adapter отвечает за
корректные confidence, subjects и provenance. Чтение пользовательского inventory
само по себе не должно считаться подтверждённым hardware evidence.

Публикация разрешена только успешному внешнему sync с текущим lease, enabled
source и неизменённой config revision. Старый lease, cancelled/test/failed result
не сохраняет observations. Collector не использует publisher вместо gRPC.
Невалидная страница получает invalid_response и не продвигает cursor.

Предел: 200 observations, 1 MiB на snapshot, 4 MiB суммарно. Duplicate keys,
nil devices/components/evidence, неверные IDs/provenance/scope и future times
отклоняются. Частичная страница не удаляет отсутствующие объекты; большой
inventory идёт страницами. Job completion и cursor коммитятся вместе с данными.

## Перенос старых данных

Сначала backend применяет migration022. Preview только читает pending count:

```powershell
go run ./cmd/backfill-assets
```

Применение до 50 сообщений за вызов:

```powershell
go run ./cmd/backfill-assets --apply --limit 50 --timeout 5m
```

Повторяйте до remaining=0. Max limit 200, max timeout 1h. Это инструмент
оператора базы с локальным DATABASE_URL, не действие viewer через API.

Команда читает сохранённые envelopes, не угадывает session по current Agent.
Она сохраняет original IDs, scopes и observation times и обрабатывает сообщения
по received_at. Каждый envelope — отдельная транзакция и projection mark.
Live ingest/backfill сериализуются; старый архив не откатывает свежий snapshot.
Если новые projections уже появились, старые сообщения могут только получить
mark: это не восстановление всех старых change versions поверх свежих.

Backfill не вызывает Migrate, не меняет connected/session state, legacy devices
или telemetry. Некорректный архив остаётся без mark, failed увеличивается,
original не удаляется. После отчёта команда возвращает ошибку при failed>0.
Future/empty сообщения получают skipped mark. Telemetry-only карточки без
архивного Hello/Inventory не получают выдуманные аппаратные факты.

## Проверки и приёмка

PostgreSQL fixture: два Collector inventory и native observation источника
типа ZABBIX с одним Eltex serial/manufacturer/model → один Asset, три bindings,
original IDs, audit proof и без выдуманного Agent для external источника.
Проверены stale/contradictory facts, split + повторный inventory, ambiguous third
source, rollback observations/cursor/job, invalid/cancelled results, backfill
двух исходных sessions без отключения Collector и повтор без дубликатов.

**Zabbix в этом тесте — fixture внутреннего publisher**, не host.get/item.get.
Production capabilities Zabbix пока пусты. B4 должен реализовать настоящий
read-only JSON-RPC, TLS policy, selected hosts, inventory/problems/events и
mapping confidence. Live acceptance с реальным Zabbix/железом ещё не проведён.

Базовые B3 части реализованы и проверены на fixtures. HA/performance, retention,
vendor aliases, общая health/topology/incidents агрегация и полноценная внешняя
демонстрация остаются последующими этапами.

Локально прошли `go fmt ./...`, `go vet ./...`, `go build ./...`, полный
`go test -count=1 ./...` с PostgreSQL и неизменёнными executables Agent/Collector:
integration 113.741s, storage 186.500s. Docker/race/настоящий Zabbix и реальное
оборудование требуют отдельной приёмки. Agent checkout чистый.
