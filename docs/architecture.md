# Решения и связь с исходными материалами

Изучены Agent ZIP, Backend ZIP, кейс NetOps из DOCX, сводный план backend,
HTML с архитектурой Agent и сопутствующие рекомендации по Git. Backend ZIP
содержал только LICENSE. Код в этом каталоге создан как новый backend.

Требования кейса шире MVP: шесть вендоров, health, reports, Linux logs,
телефония, AI, inventory, расположение и граф сети. Первый этап реализует основу
для этих функций: Controller → PostgreSQL → REST. Опрос Linux/SNMP/RouterOS
остаётся в Agent. Backend работает с нормализованными protobuf-моделями.

## Протокол

Сервис `/infrapulse.v1.AgentService/Connect`, постоянный двунаправленный stream.
Первое сообщение — matching `AgentHello` с `protocol_version=1`.
Проверяются обязательные ID/timestamps, совпадение agent/session identity,
рост sequence, валидность дат и конечность чисел. Sequence относится к попытке
отправки в stream; пропуски допустимы. Logical message ID остаётся стабильным
при replay, включая восстановление durable outbox после restart Agent.

Backend сначала фиксирует регистрацию и устройство в транзакции, затем
отправляет `HelloAck{accepted:true, protocol_version:1, application_ack:true}`,
если Agent запросил application ACK. Старые клиенты без запроса сохраняют прежний
HelloAck без дополнительных MessageACK. В существующем контракте
нет server time, heartbeat interval или config revision в HelloAck. Эти поля
потребуют согласованного изменения proto в Agent и Backend.

На один Agent разрешён один активный stream; одновременный второй получает
AlreadyExists и должен повторить подключение после закрытия первого. Всего
не более 128 stream, сообщение до 4 MiB. Disconnect обновляет только matching
session. При старте Controller сбрасывает сохранённые connected-флаги, потому
что старые TCP-stream не переживают перезапуск. MVP рассчитан на один Controller.

## Хранение

- `agents` — текущая регистрация, локальное устройство, heartbeat, last_seen;
- `agent_sessions` — сессии процесса Agent, времена подключения/отключения;
- `devices` — последнее inventory и последнее наблюдаемое состояние;
- `telemetry` — отдельные измерения с CPU/RAM и полным TelemetrySample JSON;
- `agent_messages` — полные envelopes всех типов, включая события и логи;
- `topology_snapshots` — история complete/partial/failed опросов;
- `topology_observations` — последние directed neighbor observations с expiry;
- `availability_sources`, `device_availability_current` — наблюдения доступности и выбранное сообщение;
- `device_availability_changes`, `availability_event_ids` — переходы и дедупликация событий;
- `log_batches`, `log_records`, `audit_records` — поисковые проекции логов и Linux audit;
- `telephony_batches`, `telephony_targets`, `telephony_samples` — SIP observations и последнее квалифицированное измерение;
- `telephony_policies` — ожидаемый интервал проверки конкретной SIP-точки;
- `access_users` — локальные пользователи и Argon2id-хеши паролей;
- `access_sessions` — SHA-256 сессионных токенов и абсолютный срок действия;
- `access_audit` — действия управления пользователями, сессиями и monitoring policy;
- `monitoring_policies` — ожидаемые интервалы устройства от 3 секунд до 24 часов;
- `schema_migrations` — применённые версии схемы.

Одна транзакция на envelope: сообщение, batch samples и изменения устройств
фиксируются вместе. Уникальность `(agent_id,message_id)` убирает повторную
обработку logical message. Duplicate сравнивается по kind и JSONB без внешних
`sequence`, `sent_at`, `session_id`; вложенные business поля остаются значимыми.
Такое сравнение работает и для прежних строк без migration/backfill. Повтор ID
с другим business payload отклоняется и откатывает транзакцию.
Hello при reconnect восстанавливает регистрацию, сохраняя историю измерений.

После успешного `Store.Ingest()` и COMMIT Controller отправляет `MessageACK`.
Accepted duplicate тоже получает ACK, не повторяя domain effect. Ошибка validation,
SQL или COMMIT означает отсутствие ACK. Если ACK потерян после commit, Agent
повторяет сообщение. Session tracking обновляется независимо от dedup, внутри
той же транзакции. Все исходящие сообщения проходят сериализованный send path.
Unknown protobuf fields отклоняются до JSON archive, чтобы ACK не подтверждал
потерю содержимого, которое эта версия Backend не умеет сохранять.

Пара `(agent_id,device_id)` защищает устройства одинакового ID у разных
collectors от перезаписи. Глобальная дедупликация физических устройств будет
отдельным этапом reconciliation.

Telemetry может прийти раньше inventory: создаётся минимальная карточка
с ID, а последующий inventory дополняет её. Telemetry не стирает inventory.
Старая inventory/telemetry не подменяет более новое состояние. Полный snapshot
inventory сохраняется JSONB: это позволяет принять серийные номера, location,
capabilities, компоненты и сведения OpenWrt без десятков таблиц в первом MVP.

Heartbeat обновляет last_seen самого Agent и online его локального устройства.
Он не доказывает доступность удалённых устройств. REST вычисляет `online`
из общего availability evaluator и отдельно возвращает `reported_online`
и `agent_connected`. Потеря collector или истёкшее наблюдение дают `stale`,
а не придуманный отказ. Устройства не удаляются при завершении stream.

Логи не подтверждают текущую доступность устройства: старая строка из файла
может прийти спустя часы. LogBatch создаёт placeholder при неизвестном устройстве,
но не добавляет availability evidence и не обновляет существующий inventory.
Log projection и archive фиксируются атомарно. Дедупликация строки использует
Agent/device, session scope для Simulator, source/source_id и record_id;
fingerprint сравнивает исходные business fields до маскирования секретов,
исключая меняющееся collection observed_at. Новый message_id при перечитывании
той же строки не дублирует её и не меняет исходное время приёма.

Audit строки одного события сохраняются отдельно, затем группируются по
Agent/device/scope/source/file/host/event_id. Это корреляция наблюдений Linux,
не журнал исполненных backend-команд. Полнота группы неизвестна. Отдельный
backfill читает только старые logs envelopes, небольшими порциями и отдельными
транзакциями; не применяет startup reset и остальные domain effects.

Telephony targets идентифицируются `(agent_id,target_id)` и не создают devices.
Текущий Agent не передаёт связь target → device или свой probe interval.
Мы не выводим эту связь по совпадению IP и не считаем SIP timeout доказательством
сетевого отказа. Samples, batch и inbox сохраняются в одной транзакции.
Последняя квалифицированная проверка выбирается по observed_at/received_at/id;
будущая дата помещается в отдельный clock issue slot и остаётся в истории.
Нормальная дата после исправления часов не блокируется будущим sample.

SIP current cards и summary используют единый evaluator: reported state,
freshness по ожидаемому SIP interval и состояние связи с collector. `healthy`
поддерживается как совместимый Agent alias `up`, сохраняя исходную state.
Утеря связи даёт `stale`, не выдуманный `down`. Policy сохраняется только
в backend и возвращается как manual/unverified. Старые архивные envelopes
не backfill-ятся автоматически.

## REST-аутентификация и права

Этот слой относится к пользователям платформы, а не к gRPC identity Agent.
По умолчанию все маршруты с данными проверяют сессию. Health/readiness,
OpenAPI и login/token остаются публичными. Явный development bypass допускается
лишь на числовом loopback HTTP listener; без конфигурации auth API не становится
анонимно доступным.

`internal/access` хранит правила viewer/operator/admin и создаёт случайные
256-битные токены. Пароли хешируются Argon2id (32 MiB, t=2, p=1) со случайной
солью; исходный пароль не сохраняется. В БД токен сессии представлен SHA-256,
права проверяются по текущей записи пользователя. JWT, refresh tokens, SSO,
MFA и разделение прав по Agent в этом этапе отсутствуют.

Cookie-сессия имеет HttpOnly/SameSite Strict и configurable Secure. Mutating
cookie-запрос проверяет Origin и привязанный к сессии `X-CSRF-Token`. Bearer
использует тот же серверный session store без автоматической браузерной cookie;
неоднозначная одновременная передача обоих способов отклоняется. Ответы
защищённых маршрутов не разрешают кэширование. HTTPS для REST обеспечивает
общий reverse proxy; TLS environment variables Controller относятся к gRPC.

Миграция 008 добавляет три таблицы. Создание первого admin — отдельный CLI
`cmd/access` после startup migration. CLI не вызывает `Migrate`, поскольку
startup reset connected-флагов относится к запуску Controller, а не к
администрированию работающей системы. Нет default-пароля и HTTP bootstrap.

Роль/disabled/password changes отзывают все сессии изменённого пользователя.
Guard последнего активного admin и изменения пользователей сериализуются
транзакционной advisory lock. Привилегия администратора при мутации проверяется
внутри транзакции, отдельно от HTTP middleware. Изменение monitoring policy
также проверяет актуальное право operator/admin при сохранении. Соответствующая
успешная audit-запись входит в ту же транзакцию; HTTP 200 не подтверждает
изменение без COMMIT.

`access_audit` — журнал действий пользователей backend. `audit_records` —
полученные Linux audit observations. Это разные источники; они не объединяются
по совпадению UID пользователя хоста и ID пользователя платформы. Журнал backend
пока не является неизменяемым и не фиксирует каждый HTTP request. Подробный
контракт и границы — [access-stage5.md](access-stage5.md).

## Очередь команд (этап 6)

Команда сначала фиксируется с actor/session/preconditions и глобальным ID.
Ключ HTTP scoped actor; Agent wire key равен command ID. Перед Send persisted
claim переводит queued в dispatching и повторно проверяет права/наблюдения.
Сериализованный Controller sender общий для HelloAck, MessageACK и Command.
Ready registry публикуется после HelloAck и привязывается к точной Agent session.

Один active command на Agent включает uncertainty; SQL partial unique index
исключает параллельное переполнение его worker. FinishSend делает CAS по
dispatching, не стирая быстрый result. CommandResult и event входят в транзакцию
inbox до ACK. Orphan/wrong-Agent/terminal conflicts архивируются диагностически.
Для success требуется квалифицированная verification. Восстановление после
рестарта не повторяет мутацию; неопределённые исходы не становятся failed по
догадке. Подробности и границы — [commands-stage6.md](commands-stage6.md).

## Agent snapshot references (этап 7)

Миграция010 добавляет command_snapshots и rollback links. Это observed reference
registry, а не копия содержимого Agent store. expires_at остаётся null с явной
expiry_basis; исходное backend измерение не объявляется Agent snapshot baseline.
Rollback создаётся по подтверждённой исходной команде: Actor/idem/freshness/
ownership/protection проверяются, reference резервируется в той же транзакции.
Wire содержит именно RollbackAction(snapshot_id). Agent окончательно разрешает
срок, версии, aliases и current expected state своего snapshot.

Команда и snapshot lifecycle меняются атомарно до ACK. Success verification
rollback оценивается по Agent-owned ожидаемому восстановленному состоянию,
а не по придуманному противоположному значению. Consumed/superseded references
не отправляются повторно; при неопределённости нет автоматического resend.
Agent создаёт новый snapshot перед откатом — эта новая ссылка также сохраняется.
Границы и API — [rollback-stage7.md](rollback-stage7.md).

## PKI enrollment и registry

При PKI_ENABLED отдельный HTTPS listener обслуживает enroll/renew в существующем
JSON контракте Agent. Issuer проверяет CSR/UUID и подписывает ClientAuth leaf.
Миграция011 хранит hashes bootstrap tokens, certificate metadata и PKI audit.
Одноразовый token погашается атомарно с registry/audit, per-Agent advisory lock
исключает конкурентное повторное использование и превышение лимитов.

Controller использует реальный TLS peer: URI/UUID + expiry + fingerprint registry.
Каждое incoming сообщение и dispatch проходят авторизацию. REST admin revoke
после COMMIT закрывает готовый stream, reconnect отдельно проверяет registry.
CA/key подключаются как private files/secret mount, в source/image не входят.
Enrollment listener принимает первый запрос без client certificate с token;
renewal требует verified client certificate. Детали — [stage8-pki.md](stage8-pki.md).

## Health/incidents

Миграция012 добавляет latest health observations, incidents, transitions и
evidence. CPU/RAM/trend вычисляются по source-isolated окну до120samples при
telemetry Ingest, до COMMIT/ACK. Health failure возвращает всю транзакцию,
включая inbox/telemetry. Fingerprint отделяет source и simulated session;
частичный unique index сохраняет один active incident на fingerprint.

Acknowledge — отдельная авторизованная транзакция operator/admin с audit.
Stable recovery создаёт resolved; повторная проблема — новую occurrence.
Freshness вычисляется на read и не объявляет отсутствие данных восстановлением.
Rule policy/version сохраняются в evidence, raw payload в него не копируется.
Подробности — [stage9a-health-incidents.md](stage9a-health-incidents.md).

Миграция013 расширяет incidents resource identity и добавляет
resource_health_observations. Interface projection использует существующую
telemetry; SIP projection — telephony_samples, без fake devices.
Endpoint address/transport задают hashed context; старый endpoint не закрывается
новым. Старые device fingerprints сохранены. Все projections остаются в Ingest
transaction. Read freshness учитывает соответствующий monitoring interval,
Agent state, simulated session и SIP context. Unit/value разделяют ms, rates,
percent и state grades. Детали — stage9b-network-sip-incidents.md.

Миграция014 добавляет persistent interface continuity/state changes и collector
trackers. Flap projection остаётся в telemetry Ingest transaction. Collector
worker — отдельная bounded transaction над100agents/tick, с row locks и cursor.
Он добавляет только collector incident, не меняет reported device offline.
Startup grace и sustained recovery отделяют restart/Hello от подтверждённой
потери и восстановления транспорта. Данные и thresholds — stage9c-flap-collector-incidents.md.

Reports summary читает incidents/raw sample quality в одной Repeatable Read
transaction. Миграция015 добавляет report_snapshots и indexes периодов/received_at.
Snapshot содержит frozen JSON report с методикой, после отдельной actor/idempotency/
audit write transaction. Это on-demand JSON export, не scheduler/email/AI.
Incident times clipped к period не интерпретируются как измеренный network uptime.

Diagnostic context использует одну read-only Repeatable Read transaction для
seed/candidates/freshness/current inventory/discovery. Shared collector/resource
и fresh high-confidence adjacency являются контекстными основаниями; causal
claim=false. Simulation scopes/expired links/weak resolution исключаются,
SIP service routing и historical topology не придумываются. Bounded selection
и ограничения публикуются в DTO. Details — stage9e-diagnostic-context.md.

Migration016 хранит frozen AI previews и async analyses. Allowlist экспортирует
псевдонимизированные numeric facts/relations и fixed local log pattern counts,
без свободного log/audit text/addresses/real IDs. Exact provider body/hash виден
до подтверждения. Actor/expiry/config повторно проверяются перед claim; provider
Responses требует strict JSON+valid evidence refs, не имеет tools/device actions.
Unknown delivery/restart дают uncertain без autoretry. Details — stage9f-ai-analysis.md.

## Контракт и generated code

Source of truth — `InfraPulseAgent/api/proto/infrapulse/v1/agent.proto` (P3).
Backend copy отличается только `go_package` под собственный module path.
Protobuf package, service names, прежние field numbers/types и enum values
сохранены. Сам Agent не редактировался. Обычной сборке `protoc` не нужен.

`scripts/generate-proto.ps1` и `.sh` требуют protoc 33.5, protoc-gen-go
v1.36.12 и protoc-gen-go-grpc v1.6.0. Режим `-Check` / `--check` сверяет canonical
Agent proto и проверяет reproducible generation обоих generated files.
`api/proto/contract_test.go` дополнительно сравнивает все прежние wire fields
и enums с сохранённым descriptor P2, проверяет P3 fields и archive roundtrip.
Инструкции воспроизведения — [reliable-delivery.md](reliable-delivery.md).
