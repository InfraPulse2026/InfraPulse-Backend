# B3.1 — общий реестр, привязки источников и история фактов

Дата: 11 октября 2026. Первая часть B3. Agent/Collector не изменён,
публикации на GitHub нет. Это основа Entity Resolution, а не завершённое
автоматическое объединение источников.
Следующий этап уже добавил [B3.2: предложения и ручные merge/split](backend-v2-b3-decisions.md).
Перечисленные ниже ограничения отражают первоначальную B3.1; актуальное состояние
manual операций и revisions описано в B3.2, OpenAPI 0.22.0.

## Что реализовано

Миграция 020 добавляет четыре таблицы:

| Таблица | Назначение |
|---|---|
| `assets` | Карточка общего объекта оборудования |
| `asset_source_bindings` | Источник, его original ID и ссылка на старую карточку устройства |
| `asset_fact_versions` | Версии изменившегося inventory |
| `asset_identity_evidence` | Идентификаторы, subject, confidence и исходные evidence каждой версии |

Старые `devices`, `telemetry`, raw envelopes, ключи `agent_id/device_id` и
все прежние маршруты не заменяются. Общий реестр — дополнительная проекция.

`AgentHello` и `InventoryUpdate` теперь создают/обновляют Asset в транзакции
`Store.Ingest`. Архив сообщения, legacy projections, Asset, binding, история
и identities сохраняются вместе. Ошибка любого шага откатывает весь ingest;
подтверждение durable ACK по-прежнему возможно только после COMMIT.

Повторная доставка того же message ID не создаёт второй объект или запись
истории. Новое сообщение с прежним inventory обновляет текущий snapshot и
время receipt, но не плодит версию только из-за uptime/observation clocks.
Изменение фактов создаёт новую версию; последовательность A → B → A остаётся
тремя версиями, а не сворачивается обратно в одну.

## Источник и собственность данных

Binding имеет ключ `(source_instance_id, external_type, external_id, scope)`.
На первом этапе `external_type=device`, original ID — Device.id от Collector.
`legacy_agent_id` и `legacy_device_id` позволяют связать новые маршруты со
старой телеметрией, health и командами. ID не становится глобальным по совпадению.

Если Collector ещё не заведён в Data Sources, backend автоматически создаёт
один источник с его `agent_id` и понятным именем `Collector <Agent name>`.
Создание отражается в source audit с пустым actor: это системное действие,
а не выдуманный пользователь. Если источник уже создан администратором,
используется его ID; имя, enabled, интервал и configuration не меняются.

Настройки Collector `connection_ref` теперь неизменяемы после создания.
Чтобы зарегистрировать другой Agent, создаётся отдельный источник. Это
предотвращает переназначение уже существующих bindings другому владельцу.
Обычные изменения имени/настроек через PUT и revision check остаются.

`enabled=false` источника не является остановкой Agent или запретом старого
gRPC ingress, как и на B2. Incoming inventory по-прежнему сохраняется.
Автоматическая регистрация источника не меняет Collector YAML или scheduler.

## Какие объекты различаем

Типы Asset: `host`, `network_device`, `system`, `bmc`, `chassis`, `component`,
`unknown`. Redfish `ComputerSystem`/`computer_system`/`system` — system;
Manager — bmc; Chassis — chassis. Они не объединяются между собой.
Unknown hardware resource type остаётся unknown, без догадки по похожему имени.

Для обычного Linux server/workstation/hypervisor используется host: сам тип
ОС не доказывает, что это физический сервер, а не виртуальная машина.
Вложенные компоненты inventory остаются в исходном snapshot. Их identity
evidence сохраняются с component subject ID и не превращаются в root UUID
или серийный номер всего устройства. Отдельные component Assets из вложенного
списка на этой стадии не создаются.

Legacy serial/UUID сохраняются с confidence `unresolved`, если Agent не прислал
отдельное typed evidence. Backend не придумывает high confidence сам.
Typed confidence и SourceRef сохраняются; `usable` означает синтаксически
нормализованное значение, а не доказанную глобальную уникальность.
Нулевой/placeholder UUID не становится usable identity.

## Симуляция и время

Simulated Device определяется по legacy source или observation provenance.
Binding scope содержит session ID; Asset scope включает источник и session.
Разные запуски симулятора и разные Collector не сливаются. Реальные данные
имеют пустой scope. Старые simulated Assets сохраняются как история, а не
объявляются действующим физическим оборудованием.

`first_seen/last_seen` binding — времена получения inventory backend.
`inventory_at/observed_at` — время наблюдения Collector. Поздний inventory
не заменяет текущий snapshot и identities; исходное сообщение остаётся в
архиве. Время больше чем на 30 секунд в будущем не попадает в эту проекцию,
но envelope и прежние механизмы фиксации clock anomaly сохраняются.

`lifecycle_status=observed` означает «есть наблюдение», а не online.
`resolution_status=unresolved` означает, что объединение разных источников
ещё не подтверждено. Текущая canonical карточка берёт поля из binding snapshot,
а не делает multi-source health или AI вывод. Это следующие этапы.

## REST для разработчика фронтенда

Все маршруты требуют `data.read` — viewer/operator/admin:

| Запрос | Результат |
|---|---|
| `GET /api/assets` | Страница объектов |
| `GET /api/assets/{id}` | Canonical карточка и ID её binding |
| `GET /api/assets/{id}/sources` | Original source IDs без большого inventory payload |
| `GET /api/assets/{id}/inventory` | Текущие snapshots с provenance по bindings |
| `GET /api/assets/{id}/facts` | Версии изменившихся фактов |
| `GET /api/assets/{id}/identities` | История identities и принадлежность компонентам |

Страницы используют `limit=1..200`, `offset=0..100000`, по умолчанию 50/0.
`/api/assets` дополнительно принимает `kind` и `scope=all|real|simulated`.
По умолчанию all, поэтому demo simulator виден без отдельной настройки.
Следует явно отображать scope/provenance, а не считать demo Assets реальными.

`facts` и `identities` возвращают `current`: запись принадлежит текущему
snapshot binding или является исторической. История — версии принятого
inventory, не обещание, что значение непрерывно действовало всё время между
двумя измерениями. Snapshot сохраняется полностью в normalized protobuf JSON.
Для прежних метрик frontend пока использует legacy agent/device ID из binding.

OpenAPI 0.21.0 описывает новые read-only маршруты. Merge/split endpoints пока
не зарегистрированы, чтобы не выдавать незавершённую операцию за рабочую.

## Обновление существующей базы

020 применяется автоматически после 019. Повторный запуск сохраняет реестр.
Обновление не удаляет старые устройства и не перегенерирует original IDs.
Оно также **не выполняет массовый backfill**: старые устройства попадают в
Assets при следующем новом Hello/Inventory сообщении. Уже принятый старый
message ID при replay не становится новым наблюдением.

Если Collector настроен на редкий inventory, новая карточка может появиться
позднее. Отдельный контролируемый backfill существующего inventory ещё нужен;
пока старые данные доступны через прежний `/api/devices`. Telemetry-only
устройства без нового inventory также пока не создают Asset самостоятельно.
Пустой/partial inventory batch не удаляет прежние Assets.

## Что осталось в B3

1. Обобщённая проекция наблюдений внешних источников в транзакции завершения sync.
2. Детерминированный resolver: подтверждённые аппаратные признаки, проверка
   противоречий, неоднозначности и независимости evidence.
3. Ручные merge/split с правами администратора, revision/idempotency, журналом,
   выбором canonical источника и блокированием конфликтных операций.
4. Контролируемый legacy backfill без догадки о потерянной simulation session.
5. Приёмочный пример: один Eltex, Zabbix + два Collector → один Asset с тремя
   bindings; похожие устройства с противоречивыми serial → разные Assets.

Сейчас даже совпавшие high identities разных источников хранятся раздельно:
это первый безопасный шаг, а не выполненный критерий B3 целиком.
Retention фактов/identities, aggregate hardware health, глобальная topology,
source outages и multi-source incidents добавляются последующими этапами.

## Проверки

Проверены нормализация/subjects/fingerprint без изменения protobuf Agent;
PostgreSQL — отсутствие cross-source guesses, replay, неизменившиеся clocks,
история A-B-A, late inventory, simulator sessions, атомарный rollback при
ошибке projection, upgrade заполненной B2.3 базы, сохранение legacy карточки
и пользовательских настроек Collector. В базе compound FK не позволяет
привязать snapshot/identity/canonical binding к чужому объекту.
HTTP тесты проверяют authentication, роли чтения, фильтры и отделение источников
от тяжёлого inventory. Старые API/protobuf contract tests остаются.

Локально прошли `go fmt ./...`, `go vet ./...`, `go build ./...` и полный
`go test -count=1 ./...` с PostgreSQL и неизменёнными executables Agent/Collector:
integration 111.224s, storage 165.226s. Docker/race/реальное оборудование
в этой среде по-прежнему требуют отдельной проверки. Agent checkout чистый.
