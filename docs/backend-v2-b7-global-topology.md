# B7 — глобальная топология поверх Asset Registry

Продолжение B6. Изменён только Backend. Collector/Agent, protobuf-контракт и
frontend не редактировались. GitHub-публикации в этом этапе нет.

## 1. Что поменялось

Прежняя карта показывала устройства внутри каждого Collector. Даже после
объединения их inventory в Asset сетевые наблюдения оставались отдельными.
Теперь добавлено глобальное представление: узел известного устройства имеет
его существующий Asset ID. Несколько SourceBindings одного оборудования
показываются одним узлом; каждое наблюдение связи сохраняет свой источник.

Использован прежний topology engine: Prepare, идентификаторы наблюдений,
протокольные chassis subtype/raw bytes, проверка времени и TTL. Старое
представление не удалено. Новый слой разрешает существующие SourceBindings
и группирует направленные доказательства, а не создаёт новую систему сбора.

Важно: одинаковое имя устройства не означает одинаковое оборудование.
Объединение Asset выполняется механизмом B3: ручным подтверждением или включённым
консервативным resolver при достаточных аппаратных признаках. B7 не объединяет
инвентарные карточки только потому, что сосед назвался знакомым hostname.

## 2. Новые API

```text
GET /api/topology/global?scope=real
GET /api/topology?view=global&scope=real
GET /api/assets/{asset_id}/neighbors
GET /api/topology/changes?after_id=0&limit=50
```

Для нового frontend предпочтителен явный `/api/topology/global`. GET
`/api/topology` без view и `/api/devices/{id}/topology` сохраняют прежний формат.

Параметры графа: `source_instance_id`, `asset_id`, `scope`, `include_expired`,
`limit`, `offset`. Scope — all/real/simulated, по умолчанию all. Limit по
умолчанию 500, максимум 2000. Source filter выбирает наблюдения/management
evidence этого источника; inventory остальных источников остаётся доступным
для разрешения соседа. Нельзя смешивать legacy agent_id/device_id с view=global.
Неизвестные или повторяющиеся параметры возвращают 400.

Все API требуют data.read; viewer/operator/admin могут читать. Новых write API
для топологии нет. Просмотр графа не меняет Asset Registry и не отправляет команды.
Неактивный/объединённый Asset для neighbors возвращает 409. Карточка alias
содержит merged_into_id для перехода к действующему Asset.

## 3. Что возвращается

В графе есть:

- `nodes`: Asset или явно обозначенный placeholder;
- `edges`: группы L2-наблюдений с портами, статусом, confidence и evidence;
- `management_relations`: связи управления BMC, отдельно от L2;
- generated_at, флаги ограничений и параметры страницы.

У известного узла id=asset_id. В binding_ids перечислены источники карточки.
У неизвестного asset_id=null, kind=unresolved_neighbor/source_device/
unresolved_resource. Неизвестный сосед не создаёт новый Asset и не становится
online. Граф не выводит состояние питания или здоровье из наличия LLDP.

У каждого evidence есть source instance, Agent/device ID, simulation scope,
observation/version ID, протокол, направление from/to, исходные local/remote
ports, observed/received/expires, confidence, результат разрешения и canonical
Neighbor JSON. Raw bytes сохраняются как base64; subtype не теряется.
Это позволяет разработчику frontend показать, кто и чем подтверждает связь.

## 4. Как разрешается сосед

Локальное устройство ищется по точному source instance + scope + source-owned
device ID. Данные topology, пришедшие раньше inventory, остаются source_device
placeholder; регистрация оборудования не выдумывается из сетевой рекламы.

Удалённый chassis сопоставляется с текущим inventory существующих Asset.
MAC нормализуется с проверкой пригодности; typed chassis evidence сохраняет
subtype и raw bytes. MNDP subtype 0 интерпретируется как native MAC только при
явном MNDP и подходящих байтах. Он не становится LLDP subtype 4 автоматически.
Rejected/invalid root evidence не используется.

Совпадения от нескольких bindings одного Asset дают одного кандидата. Если
получилось несколько разных Asset — ambiguous, кандидатами можно заняться
через B3 review. Asset с конфликтующими аппаратными идентификаторами не
выбирается уверенно. Hostname и адрес дают только tentative candidate:
неизвестный узел остаётся неизвестным. Известное противоречие hostname/chassis
показывается отдельно. Текст имени, описание порта и management IP не запускают
подключение, сканирование или добавление нового target.

Real и simulated пространства разделены. Симуляция дополнительно привязана
к source instance и session. Одинаковый MAC в симуляторе и реальной сети не
связывает эти устройства. При усечённом inventory разрешение не объявляется
уникальным: среди пропущенных устройств может быть ещё один кандидат.

## 5. Группировка связей и двусторонность

У L2 evidence сохраняется направление, даже когда группа edge отображается
как одна связь. Для группировки должны совпасть существующие Asset и typed
advertised port IDs. Только имя/ifIndex из одного Collector не доказывает
совпадение с портом другого Collector. Для таких случаев сохраняется отдельная
source-owned identity, а неопределённость не скрывается.

Confirmation:

- one_sided — текущее наблюдение есть, обратного подтверждения портов нет;
- bidirectional — активные наблюдения найдены в обоих направлениях, устройства
  разрешены и противоположные typed ports совпали;
- unresolved — удалённое устройство уверенно не разрешено.

Bidirectional означает подтверждённое двустороннее L2-соседство. Это не
доказательство одного физического кабеля: промежуточные bridge/виртуальные
элементы нельзя исключить только этой рекламой. Confidence учитывает заявленную
уверенность активных evidence и качество разрешения. Два направления сами по
себе не превращают unresolved confidence Collector в high.

В тестовом примере два Collector наблюдают A→B, один также B→A. После B3 mapping
получаются два узла, одна группа и три evidence. Если обратное наблюдение
истекло, оставшаяся активная связь снова one_sided.

Протоколы и raw subtype сохраняются. Backend не угадывает, что CDP/MNDP port
строка и LLDP typed port обозначают один порт. При недостаточных признаках
могут остаться параллельные группы; это честнее потери наблюдения.

Если изменился идентификатор порта, создаётся новая identity связи. Старое
наблюдение сохраняется до своего TTL и остаётся в истории. Empty complete,
partial или failed snapshot не удаляет действующую связь раньше advertised/
Collector expiry — это прежнее правило engine, сохранённое в B7.

## 6. История, TTL и повторная доставка

Migration027 добавляет:

- global_topology_latest — текущие source/session-scoped наблюдения;
- global_topology_versions — версии содержательных данных;
- global_topology_events — observed/changed/revived/expired/imported.

Запись выполняется в той же транзакции, что inbox и прежняя topology projection.
ACK выдаётся после commit. Ошибка новой projection откатывает и остальные
изменения этого сообщения. Запись выполняется bounded batches по 256 наблюдений,
чтобы не делать сетевой round trip на каждое отдельное поле.

Повтор message ID не дублирует данные. Новый пакет с теми же фактами обновляет
latest timestamps/expiry, сохраняя прежнюю версию. В fingerprint не входят
обservation clocks и age_seconds. Изменение confidence/description и прочих
содержательных данных создаёт changed; изменение порта создаёт другое
наблюдение. Устаревший пакет и далёкое будущее не заменяют уже принятые корректные
данные. Первоначальное future наблюдение явно clock_skew и скрыто из active view.

TTL в графе вычисляется по now при каждом чтении. Поэтому просроченное
наблюдение перестаёт быть active даже при отставании фоновой обработки.
Фоновый worker сохраняет expired раз в секунду, порциями до 500. После
перезапуска он обрабатывает backlog. occurred_at для expired — реальное
expires_at; recorded_at — момент записи события. Возвращение после истечения
срока регистрируется revived; повторная обработка не дублирует expired.
Если новый пакет пришёл до фоновой обработки пропущенного expiry, эта же
транзакция сначала сохраняет expired, затем revived. История не теряет разрыв
только из-за задержки worker.

History использует keyset cursor: берите next_after_id предыдущего ответа.
Has_more показывает, есть ли продолжение. Limit 1..200; payload страницы
ограничен 8 MiB. asset_id в changes выбирает текущий локальный reporter Asset,
а не все исторические удалённые кандидаты.

current_local_asset_id — сегодняшняя привязка исходного source-owned device.
Она явно не выдаётся за Asset ID на дату события. История содержит immutable
Neighbor версии, version_observed_at/version_expires_at и отдельные времена
события. Полный граф на произвольную прошлую дату здесь не реконструируется.
B3 merge/split имеют собственный audit; BMC отношения меняются вместе с inventory
и имеют историю `/api/assets/{id}/facts`.

## 7. BMC остаётся отдельной сущностью

BMC_MANAGES_SYSTEM и BMC_MANAGES_CHASSIS возвращаются в management_relations.
MANAGED_BY_BMC нормализуется в соответствующее прямое управление. Эти связи
не попадают в edges и не считаются физическими сетевыми кабелями.

Resource URI недостаточно: `/redfish/v1/Systems/1` может существовать у многих
BMC. Поиск включает source instance, scope, BMC target/type/protocol/instance
из SourceRef. Неподходящий тип ресурса, неоднозначность или неполный inventory
не приводят к уверенной привязке. Evidence содержит source/binding/fact IDs и
время inventory. Одинаковая подтверждённая связь от нескольких bindings
группируется с сохранением всех evidence.

## 8. Обновление, ограничения и проверки

Обычный запуск применяет migration027. Уже принятые текущие topology observations
переносятся в новый слой порциями по 256 с событием imported и сохранением
simulation scope. Прежние snapshots и Agent inbox остаются. Старые transitions
не выдумываются; полные прежние snapshots доступны в старом архиве. Повторная
миграция не создаёт новые imported events.

Граф читает inventory/evidence в одном repeatable-read snapshot. Ограничения:
4000 bindings и 16 MiB inventory; 10000 observations и 16 MiB; 2000 management
groups. Флаги truncated/inventory_truncated/observations_truncated говорят
об ограничениях. Edge pagination выполняется после группировки. total_edges
считает только загруженные данные и не обещает размер неограниченного enterprise
графа. Offset-page меняющейся карты не является неподвижным историческим snapshot.

Тесты проверяют multi-Collector объединение, reciprocal ports, ambiguous
identities, simulation isolation, port changes, TTL, replay/clock dedup,
changed/revived history, поздние/future пакеты, keyset pagination, upgrade с B6,
атомарный rollback, role/query validation и сохранение legacy API.
Process test неизменённого Collector215d33a проверяет Redfish management relation
через gRPC → PostgreSQL → authenticated REST, отдельно от network edges.

Конкретные результаты — в [validation.md](validation.md). Физическое железо,
Docker, Linux-only process/race suite и большие enterprise нагрузки пока
не приняты. Прежнее ограничение одного Controller сохраняется до B16.
Frontend в этом этапе не создавался. Следующий этап B8 связывает hardware,
remote logs/audit и SIP с глобальными Asset и общим контекстом.
