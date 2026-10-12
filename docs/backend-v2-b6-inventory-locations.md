# B6 — единая инвентаризация и физическое расположение

Добавлен следующий этап Backend v2 после B5. Код Collector/Agent и frontend
не менялись. Здесь описан реализованный объём; весь план B0–B16 ещё не завершён.

## 1. Что изменилось для пользователя

Раньше у Asset были общие поля и отдельные inventory-снимки каждого источника.
Теперь отдельный ответ API собирает характеристики в одну карточку и показывает,
откуда взялось каждое значение. В той же карточке доступно проверенное физическое
размещение: площадка → здание → помещение → стойка → диапазон U.

Пример: Collector сообщает serial `ABC-123` с высокой уверенностью, а Zabbix
присылает `OTHER-123` без подтверждения. Карточка сохраняет оба утверждения,
показывает конфликт и выбирает более уверенное значение. Если два утверждения
с одинаковой максимальной уверенностью противоречат друг другу, выбранного
значения нет: `selected=null`, `value=null`. Даже канонический источник не
скрывает такой конфликт. Hostname никогда не подставляется вместо serial.

## 2. Сводная инвентаризация

Новый endpoint:

```text
GET /api/assets/{asset_id}/resolved-inventory
```

В ответ входят `asset_id`, `asset_revision`, `source_count`, `status`, `missing`,
`attributes` и `placement`. Поля инвентаризации:

- name, hostname, vendor, model, serial, hardware_uuid и hardware_revision;
- OS/network OS, distribution, kernel, architecture, software/firmware version;
- management IP, выбранные management interfaces Zabbix и network-сведения
  Collector, если они были присланы;
- hardware inventory и компоненты;
- исходные location/system_location как утверждения источников.

У каждого атрибута есть `status`, `value`, `selected`, `conflict`, `claims`.
Каждый claim содержит значение, source instance, source type/name,
Collector protocol, SourceBinding, fact ID, provenance, confidence, method,
usable, состояние enabled источника и временные отметки. Исходное типизированное
identity evidence сохраняется в claim, когда оно есть.

`observed_at` — явное время устройства/evidence, если оно прислано; иначе время
принятого inventory. `snapshot_observed_at` — время последнего принятого снимка.
`fact_received_at` — получение версии фактов. Неизменившийся снимок может обновить
snapshot time, сохранив прежний fact ID и время получения версии. Heartbeat не
становится новым аппаратным фактом. Clock quality передаётся отдельно.

Алгоритм выбора скалярных значений:

1. Непригодные и rejected/invalid идентификаторы остаются видимыми, но не выбираются.
   Диагностика rejected не обходится повторным чтением того же legacy-поля.
2. Уверенность: high → medium → low → unresolved. Backend не повышает уверенность
   просто потому, что значение пришло из определённого протокола.
3. Противоречие среди лучших claims оставляет значение невыбранным.
4. Для совпадающих лучших значений канонический SourceBinding служит приоритетом.
5. Любые разные пригодные скалярные значения сохраняют признак conflict.

IP — отсортированное объединение адресов с сохранением source claims. Interfaces,
components, networks и hardware имеют статус `source_scoped`: они остаются
разделёнными по источникам. Компоненты с одинаковым именем не объединяются
без аппаратного доказательства. Серийный номер диска не становится serial сервера.
У management interfaces Zabbix выводятся только разрешённые нормализованные
поля; произвольные provider fields и connection credentials не копируются.

Статус `complete` означает наличие выбранных vendor/model/serial, `partial` —
часть данных, `unknown` — отсутствие пригодных фактов, `conflict` — противоречие.
Это статус полноты инвентаризации, а не online, свежесть метрик или здоровье.
Отключённый источник не теряет прежние факты: `source_enabled=false` видно явно.

Сводная карточка читается в одном repeatable-read snapshot PostgreSQL. Ограничение:
128 bindings и 8 MiB входных inventory/provider snapshots; превышение возвращает
409, а не молча усечённую карточку. Текущие факты берутся из последних снимков;
предыдущие значения доступны через существующий `/facts`. Неприсланное поле не
выдумывается и не восстанавливается автоматически из старой версии.

Источники Collector/SNMP, Redfish/IPMI через Collector, Zabbix и Prometheus
используют уже работающие SourceBindings. Для будущего Ansible действует та же
модель; сам Ansible Connector будет в B10 и пока не реализован.

## 3. Справочник расположения

```text
GET  /api/locations?kind=rack&parent_id={room_id}&limit=50&offset=0
POST /api/locations
GET  /api/locations/{id}
PUT  /api/locations/{id}
GET  /api/locations/{rack_id}/rack-layout
```

Допустимые kind: site, building, room, rack. Site не имеет родителя; building
принадлежит site, room — building, rack — room. Каждый объект получает отдельный
`loc_...` ID. Имена уникальны среди соседей без учёта регистра. Стойка имеет
ёмкость 1..100 U; для остальных объектов rack_units=0.

Можно переименовать объект или изменить ёмкость стойки. Kind и parent_id после
создания неизменяемы: случайное изменение одной ссылки не переносит всё здание.
Уменьшение стойки запрещено, если оно отбрасывает занятые верхние U. Защита
иерархии и ёмкости есть и в сервисе, и в проверках PostgreSQL.

Все изменения требуют admin, `Idempotency-Key`, reason и текущую revision.
Для создания `expected_revision=0`; для изменения — revision из последнего GET.
Точное повторение с тем же ключом возвращает прежний ответ и
`Idempotent-Replay: true`. Другой payload с тем же ключом возвращает 409.

## 4. Размещение устройств

```text
GET /api/assets/{asset_id}/placement
PUT /api/assets/{asset_id}/placement
GET /api/assets/{asset_id}/placement/history?limit=50&offset=0
POST /api/placements/import
```

Три режима:

- `rack`: location_id стойки, start_unit и height_units;
- `outside_rack`: location_id помещения, свободное текстовое position;
- `unplaced`: расположение неизвестно/явно снято, location_id=null и U=0.

Диапазон 2U с start_unit=10 занимает U10 и U11 включительно. Нумерация U идёт
от 1; фронт/тыл не позволяют разместить два устройства в одних U. Для хранения
занятости создаётся строка на каждую U с уникальным ключом rack_id+unit.
Приложение не полагается только на предварительный поиск пересечений.

Placement имеет собственную revision: частые inventory обновления не делают
форму расположения устаревшей каждые три секунды. До первого размещения
revision=0, mode=unplaced. Последующие перемещения/снятие требуют текущую
placement revision. Просмотр возвращает полный путь из справочника.

При ошибке или конфликте транзакция откатывается вместе с U, аудитом и
idempotency-result. Импорт — JSON до 64 KiB, 1..50 различных Asset за раз,
существующие location IDs. Все устройства проверяются до записи; прежние U
освобождаются внутри той же транзакции. Это позволяет атомарно обменять места
двух устройств. Повтор операции не создаёт новые события истории.

Read доступен viewer/operator/admin. Изменения — только admin; текущая роль
и сессия повторно проверяются в транзакции. Cookie-запросы требуют прежний
CSRF-token. В development auth bypass Store всё равно требует административный
principal для записи. Одновременное демотирование пользователя не оставляет
права на изменение расположения по старому principal.

`placement_events` хранит before/after, автора, reason и время. Справочник
имеет аналогичный внутренний `physical_location_events`; общий access audit
регистрирует операции. Полные события размещения доступны через history API.

## 5. Как внести первое устройство

Backend запускается обычным способом из README; migration026 применяется
автоматически вместе с прежними миграциями. Используйте прежнюю admin-сессию.
Для каждой новой операции задавайте отдельный Idempotency-Key; при сетевом
повторе той же операции оставляйте ключ прежним.

Сначала четыре POST `/api/locations`, запоминая ID каждого ответа:

```json
{"kind":"site","name":"Демо-площадка","expected_revision":0,"reason":"Сверено с планом площадки"}
```

```json
{"kind":"building","parent_id":"ID_ПЛОЩАДКИ","name":"Корпус А","reason":"Сверено с планом площадки"}
```

```json
{"kind":"room","parent_id":"ID_ЗДАНИЯ","name":"Серверная 101","reason":"Сверено с планом площадки"}
```

```json
{"kind":"rack","parent_id":"ID_ПОМЕЩЕНИЯ","name":"R01","rack_units":42,"reason":"Проверена маркировка стойки"}
```

Затем получите Asset ID через GET `/api/assets`. PUT его `/placement`:

```json
{"mode":"rack","location_id":"ID_СТОЙКИ","start_unit":10,"height_units":2,"expected_revision":0,"reason":"Проверено по маркировке оборудования"}
```

GET `/api/assets/{id}/resolved-inventory` покажет характеристики с claims и
placement U10–U11; GET стойки `/rack-layout` — occupied_units=2, free_units=40.
Попытка разместить другой Asset в U11 вернёт 409. Снятие требует следующий PUT
с mode=unplaced, expected_revision из последнего ответа и reason.

Для группового импорта применяется [пример](examples/b6-placement-import.json).
Замените демонстрационные IDs и revisions действительными. Импорт не создаёт
физические устройства и не регистрирует вымышленные Agent: он размещает уже
существующие Asset. Новые характеристики по-прежнему поступают через источники.

## 6. Что сохраняет совместимость

Старые `/api/assets/{id}/inventory`, `/api/devices`, `/facts`, telemetry и source
API сохраняют прежние форматы. Сводный ответ добавлен отдельным endpoint.
Протокол gRPC и Agent не изменены; новый backend код не посылает location-команды.
Source location сохраняется как evidence, но не переносит устройство без
решения пользователя. LLDP/CDP/MNDP не определяют его U.

При ручном merge размещение целевого Asset сохраняется. Если у присоединяемого
Asset есть размещение, merge отклоняется: сначала явно освободите его место,
проверьте, что это дубликат, затем объедините. Автоматический resolver не
переносит уже размещённую входящую карточку. При split старое место остаётся
у старого Asset, новая карточка unplaced: копирование дало бы двойную занятость.

Migration026 добавляет таблицы, индексы и функции; существующие assets,
source bindings, agent_messages, telemetry и data sources не удаляются.
Повторное применение миграций сохраняет расположение. Прежнее ограничение
одного Controller остаётся до B16: B6 не реализует multi-instance HA.

## 7. Проверки и границы приёмки

Есть тесты выбора root evidence, конфликтов, rejected идентификаторов,
источников компонентов, всех management interfaces Zabbix, времён и полноты.
PostgreSQL-тесты проверяют hierarchy, capacity, multi-U, overlap/concurrency,
атомарный swap/rollback, revision, idempotency, историю, revoked admin,
upgrade с B5, merge/split и сохранение расположения после inventory.
Process test неизменённого Collector215d33a дополнен чтением новой карточки
через настоящий authenticated REST после gRPC/Redfish ingestion.

Результаты конкретного запуска приведены в [validation.md](validation.md).
Docker, frontend, физический стенд и Linux-only process/race suite здесь
не проверены. Ни XML/CSV import, ни автоматическое сопоставление текстовых
площадок со справочником, ни generic manual inventory editor не добавлены.
JSON placement import и ручное расположение реализованы; B7–B16 впереди.

## 8. Следующий шаг

B7 связывает существующую LLDP/CDP/MNDP topology с глобальными Asset IDs,
сохраняя неизвестных соседей, evidence и expiry. Это отдельный этап.
