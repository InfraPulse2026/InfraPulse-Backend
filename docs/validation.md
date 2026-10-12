# Результаты проверок MVP

Актуальный Backend v2 через B8: [проверки от 12 октября](backend-v2-b8-validation.md).

Актуальные P3 ACK/durable integration результаты от 6 октября 2026 года:
[reliable-delivery.md](reliable-delivery.md). Ниже сохранена история предыдущих
проверок MVP от 5 октября; она не описывает состояние P3 delivery после исправления.

Проверено локально 5 октября 2026 года на Windows с Go 1.27.1.
Для проверки базы запущен отдельный PostgreSQL 18 на `127.0.0.1:55432`,
с собственным каталогом данных. Существующий PostgreSQL пользователя
и его учебные данные не изменялись.

## Выполнено успешно

- `go fmt ./...` — форматирование;
- `go vet ./...` — статические проверки;
- `go test ./... -count=1` с `TEST_DATABASE_URL` — все пакеты прошли;
- `go build -o bin/server.exe ./cmd/server` — сборка Windows;
- `GOOS=linux CGO_ENABLED=0 go build -trimpath -o bin/server-linux ./cmd/server`
  — статическая сборка Linux;
- настоящий Simulator из неизменённого Agent → gRPC backend → PostgreSQL → REST;
- настоящий Agent в collector-only режиме → регистрация и heartbeat;
- доступность `/readyz`, `/api/agents`, `/api/devices`, `/telemetry`,
  `/metrics/history`, `/openapi.json` на запущенном backend;
- сверка SHA-256 всех 170 файлов исходного Agent с архивом — без изменений;
- отдельная сверка proto и двух generated Go files — точные копии.

## Проверенные сценарии

gRPC: Hello/HelloAck, правильная версия протокола, обязательные данные,
совпадение identity, растущий sequence, повторный активный stream одного Agent,
невалидные даты/NaN, отказ хранилища до HelloAck, чистое завершение stream.

PostgreSQL: атомарность batch при ошибке второго sample; повтор одинакового
message_id; конфликт содержимого под тем же ID; метрики вне порядка времени;
нулевой CPU против отсутствующего; полный диапазон uint64 для RAM; inventory
сохраняется при telemetry; одинаковые device_id у разных agents; история
сессий; heartbeat восстанавливает local online; timeout; старый disconnect
не закрывает новую сессию; повторное применение миграции.

HTTP: формы ответов, пустые массивы, 400/404/409/503, latest/history,
inventory, telemetry alias, OpenAPI. Тест жизненного цикла запускает настоящий
backend с временной схемой, ждёт `/readyz`, отменяет контекст и проверяет
завершение серверов без ошибки.

## Сквозное демо

Сценарий Simulator `all`, 10 секунд, 4 устройства, interval 1s:

| Тип | Сохранено envelopes |
|---|---:|
| hello | 1 |
| inventory | 10 |
| telemetry | 37 |
| topology | 40 |
| logs | 10 |
| events | 8 |
| telephony | 10 |

В `telemetry` сохранено 37 samples. В REST получены `source=simulated`,
CPU и RAM, история и последнее измерение. Количество samples меньше 40:
сценарий содержит период недоступности первого устройства, когда оно не
отправляет online telemetry.

Обычный Agent на Windows проверен с `local.enabled: false`: регистрация,
HelloAck, heartbeat с интервалом 10 секунд, изменение last_seen и
`connected=true` в API. Это проверка реального транспорта/жизненного цикла
Agent, а не доказательство сбора Windows CPU/RAM.

Последняя версия дополнительно проверена коротким `healthy` demo через
поставляемую `configs/agent.simulator.yaml`: REST отдаёт UTC timestamps,
`received_at`, `source=simulated` и CPU. Примеры Agent используют `127.0.0.1`,
поскольку `localhost` в этой среде не дал успешного gRPC-подключения к IPv4 listener.

## Дополнение: настраиваемые интервалы

Дополнение: проверены миграции 002/003, сохраняемые per-device интервалы 3s..24h,
изолированность collectors и независимость telemetry freshness от heartbeat.
Исходный Agent validate-config принимает 10h/24h и отклоняет 24h1s;
суточный runtime-цикл не запускался. Удалённое применение настроек в Agent
не реализовано. Подробности — [monitoring-intervals.md](monitoring-intervals.md).

На запущенном backend через PUT сохранён интервал telemetry 86400s;
GET вернул ту же политику, manual/unverified и корректную freshness.
Миграции 002 и 003 применены поверх существующей схемы 001 с сохранением старых
измерений. Проверены fresh/stale для суточного профиля; обе границы API, 3s/24h, проходят проверку. В отдельной тестовой базе сохранились все 41 прежнее измерение.

## Не проверено в первоначальной среде 5 октября

- Docker Compose runtime и сборка Dockerfile: Docker здесь не установлен.
  Предусмотрены проверки `docker compose config --quiet` и `docker compose build`
  в CI; CI ещё не запускался, так как код не отправлялся в GitHub.
- Реальный Linux Agent → backend с CPU/RAM/interfaces: Linux/WSL здесь нет.
  Готовая конфигурация `configs/agent.linux.yaml` и команды находятся в README.
- Локальный `go test -race`: компилятор C для Windows не найден; команда
  предусмотрена в Linux CI, но её прохождение здесь не заявляется.
- mTLS handshake, настоящие сетевые устройства, AI и frontend.

Это рабочий проверенный backend MVP в описанной локальной конфигурации,
с явно указанными границами проверок. Готовность всей платформы или
производственного развёртывания не заявляется.

## Локальный этап 1: topology, 7 октября 2026

Этап находится в ZIP и не опубликован на GitHub. База реализации — backend
main после слияния Agent P3 compatibility (a960eb6).

Успешно выполнено:
- go fmt ./..., go vet ./..., go test ./... -count=1 с отдельным PostgreSQL 18;
- дополнительные тесты после последнего уточнения unknown/reported state;
- сборки server для Windows и Linux; Linux integration tests скомпилированы;
- существующие проверки сохранения wire-контракта и application ACK прошли;
- TTL boundary, clock skew, исправление времени Agent, late delivery;
- replay, out-of-order snapshots, partial/failed/empty polls;
- неоднозначный hostname/chassis, конфликт идентичности, бинарные port IDs;
- изоляция collectors и topology до inventory без выдуманного online/offline;
- запись 300 observations через несколько пакетов и пагинация 256+44;
- искусственная ошибка PostgreSQL на второй связи откатывает inbox, snapshot и первую связь;
- все 227 файлов присланного Agent ZIP совпали с Git reference, checkout Agent чистый.

Живой запуск неизменённого Agent Simulator P3:
1. Backend запущен на HTTP 127.0.0.1:18081, gRPC 127.0.0.1:19443.
2. Отдельная схема topology_demo_* в собственной тестовой БД.
3. Healthy scenario: 4 устройства, 3 секунды, interval=1s, текущая UTC дата.
4. PostgreSQL содержит 12 snapshots и 4 последних observations.
5. GET /api/topology вернул 4 active edges и 5 известных nodes:
   4 тестовых устройства и отдельный узел самого Simulator/collector.
6. Соседи сопоставлены по unique_hostname, confidence=medium; source=simulated.
7. После TTL обычная карта вернула 0 edges; include_expired=true вернул 4 expired edges.
8. Примеры ответов сохранены в docs/examples/*.json. Тестовая схема удалена,
   временные процессы остановлены. База пользователя не затрагивалась.

Локальные проверки PostgreSQL выполнялись с PostgreSQL 18. Docker runtime
и настоящий Linux collector в этом этапе не запускались. Linux binary и
integration executable скомпилированы, запуск Linux integration не заявляется.
Локальный race detector не запускался: C toolchain отсутствует.
Это проверка discovery graph на Simulator и тестовых fixture; настоящие
LLDP/CDP/MNDP устройства должен проверить ответственный за Agent.

Старые архивные topology envelopes до миграции 004 автоматически не
backfill-ятся. Карта наполняется новыми сообщениями. Глобальное reconciliation
разных collectors, device online expiry, commands/config/PKI/auth/AI —
следующие этапы и не объявляются реализованными этим ZIP.
## Локальный этап 2: availability, 7 октября 2026

Этапы topology и availability остаются только в локальной копии и ZIP.
GitHub main содержит лишь ранее согласованную совместимость Agent P3.
Agent и protobuf-контракт этого этапа не изменяются.

Выполнены go fmt ./..., go vet ./..., go test ./... с отдельным PostgreSQL 18.
17 availability PostgreSQL tests проверяют 3s/24h cadence, offline/recovery,
историю/пагинацию, dedup событий, конфликт ID с rollback всего пакета,
heartbeat только локального устройства, placeholders, источники/ошибки,
старые и будущие даты, миграцию legacy, повторную ошибку часов,
ограниченный размер текущих проекций и согласованность card/map/summary.
Существующие ACK/replay, topology, API и wire-contract tests также прошли.
Windows и Linux server собраны; Linux integration executable скомпилирован.
Нативный запуск Linux integration и race detector в этой среде не заявляются.

Живой проверочный запуск неизменённого P3 Simulator:
- HTTP 127.0.0.1:18082, gRPC 127.0.0.1:19444, отдельная availability_demo_* схема.
- Device-recovery, четыре устройства, семь секунд, interval=1s, текущая UTC дата.
- Карточка устройства прошла online → offline → online.
- Сообщённая история содержит три перехода, без строки на каждый sample.
- После завершения Simulator все пять записей (включая collector) получили
  stale/agent_disconnected: ни одна не объявлена автоматически offline.
- Ответы, сводка и timeline сохранены в docs/examples/availability.*.simulated.json.
- Тестовая схема удалена, запущенные нами процессы остановлены; база пользователя
  не использовалась.

Дополнительно проверена совместимость API: online теперь вычисляемый флаг;
reported_online и availability.last_reported_online сохраняют последнее
квалифицированное сообщение. Отсутствие данных отображается unknown,
истёкшие данные и потеря collector — stale.

Оставшиеся этапы платформы: logs/audit, telephony, command dispatch/state machine,
rollback orchestration, согласованный live ConfigUpdate, production enrollment
и identity binding, REST auth/roles, incidents/reports/analysis.

## Локальный этап 3: logs/audit, 7 октября 2026

Версия подготовлена локально на той же основе main a960eb6. GitHub не обновлён;
сохраняются topology и availability из предыдущих этапов. Agent и wire-контракт
не редактировались. OpenAPI расширен до 0.5.0.

Успешно выполнены go fmt ./..., go vet ./..., go test ./... -count=1 с отдельным
PostgreSQL 18 на loopback 55432. Девять новых PostgreSQL tests проверили:
- полнотекстовый и буквальный поиск, фильтры, сохранение null/0/false;
- маскирование типовых секретов только в выдаваемой/поисковой проекции;
- replay, reread с новым collection timestamp и конфликт исходного business payload;
- несколько audit record types одного event ID без удаления строк;
- независимость Agent, source и Simulator session;
- cursor страницы и межперемешанные строки разных audit groups;
- 260 строк через границу порций 128 и страницы 128+128+4;
- SQL ошибка на второй audit строке откатывает inbox, batch, logs и первую audit строку;
- bounded/idempotent backfill, malformed archive и сохранение активной Agent session.

API/unit tests, существующие ACK/replay, wire-contract, topology и availability
tests также прошли. Сборки server и backfill-logs для Windows и Linux выполнены;
Linux integration executable скомпилирован. Нативный Linux запуск, race detector
и Docker runtime здесь не выполнялись и не объявляются проверенными.

Живой запуск неизменённого P3 Simulator:
1. Отдельная схема logs_demo_*, HTTP 127.0.0.1:18083, gRPC 127.0.0.1:19445.
2. Audit-activity, 5s, interval=1s, четыре устройства, текущий UTC start.
3. PostgreSQL: 5 log_batches, 8 log_records, 3 audit_records.
4. REST summary: total=8, audit_failures=3, info=5, unknown severity=3.
5. Поиск q=denied и audit фильтр success=false/uid=1000 вернули по три строки.
6. API вернул три audit groups, две страницы по две строки, строки одной группы.
7. Примеры сохранены в docs/examples/logs.*.simulated.json с source=simulated.
8. Повторный backfill CLI вернул 0/0/0, не дублируя принятые записи.
9. Временный Controller остановлен по проверенному executable/PID; собственная
   demo schema удалена. Пользовательская PostgreSQL на 5432 не использовалась.

Реальные syslog/auditd файлы должен проверить владелец Agent на Linux с
настроенными sources и правами чтения. Это проверка синтетических событий,
а не доказательство полноты группы audit или production безопасности.
Дополнительное маскирование не меняет исходный архив agent_messages.
Retention, auth/roles, operator command audit trail и AI export остаются будущими этапами.
В сумме закрыты пункты 1, 2 и 8 списка, а не все десять.

## Локальный этап 4: SIP/telephony, 8 октября 2026

Основой остаётся backend main a960eb6 с локальными этапами 1..3. Коммитов
и публикации этого этапа нет. Agent reference checkout чистый; его бинарник
собран с -mod=readonly в отдельный tools/bin, исходники не редактировались.
Wire-contract tests проходят. OpenAPI 0.6.0 проверен на ссылки и дубли параметров.

Успешно выполнены go fmt ./..., go vet ./..., go test ./... -count=1 с
отдельным PostgreSQL 18 и AGENT_BINARY обычного P3 Agent. В полном запуске
process-level SIP test действительно выполнялся, а не был пропущен.
Server и существующая logs backfill CLI собраны для Windows и Linux;
Linux integration executable скомпилирован. Docker runtime, нативный Linux
запуск и race detector здесь не выполнялись.

Восемь новых PostgreSQL tests проверили:
- TLS metadata, response 401, latency=0 и null при timeout;
- SIP down не меняет devices.online и не создаёт фиктивный device;
- envelope replay/conflict и out-of-order observations;
- future-only report, последнюю квалифицированную проверку и clock recovery;
- дневной policy, expiry/disconnect и согласованность card/summary;
- Agent isolation, target ambiguity и source/transport filters;
- time/cursor history, границы from/to и совместимый пустой update;
- SQL ошибка второй точки полностью откатывает inbox/batch/targets/samples;
- 256 targets в одном update и сохранение policy/samples после повторной миграции.

Обычный неизменённый Agent -> локальная UDP SIP fixture -> gRPC Controller -> PostgreSQL:
1. Отдельная schema infrapulse_sip_process_*, временный YAML/state, durable queue.
2. Согласованный ACK, реальный OPTIONS request и matching SIP response 401.
3. Backend показывает reachable=true, degraded/authentication_required.
4. Ответ 200 переводит точку в up с настоящим измерением latency.
5. Игнорирование ответов даёт down после failure_threshold=2, code/latency=null.
6. Возобновление 200 возвращает up и consecutive_failures=0; история сохранена.
7. Процессы, UDP socket и отдельная schema очищаются тестом.

Полный backend runtime -> неизменённый Simulator -> REST:
- HTTP 127.0.0.1:18084, gRPC 127.0.0.1:19446, собственная telephony_demo_* schema.
- Sip-recovery, 7s, interval=1s, четыре точки и текущий UTC start.
- В БД 7 batches, 4 targets, 28 samples; REST history первой точки содержит 7 samples.
- Timeline реально показывает up -> degraded -> down -> up.
- После выхода Simulator summary показывает 4 stale/agent_disconnected, latest остаётся up.
- Проверены history страницы по 3 samples, policy на 24h с manual/unverified.
- REST примеры сохранены в docs/examples/telephony.*.simulated.json.
- Временный backend остановлен по проверенному PID/executable; demo schema удалена.

Не объявляется проверенным реальное качество аудио/RTP, реальная АТС,
TCP/TLS live collector или Linux deployment. UDP fixture и Simulator различаются
в отчёте. Старые telephony archive envelopes автоматически не backfill-ятся;
API рассчитан на новые пакеты. Retention, device binding, incidents, auth/roles,
commands/rollback, live config и production PKI остаются следующими этапами.
В текущей версии закрыты пункты 1, 2, 3 и 8 списка — четыре из десяти.

## Локальный этап 5: REST auth/roles, 8 октября 2026

Подготовлено локально поверх этапов 1..4 на основе main a960eb6. GitHub не
обновлён; Agent reference checkout чистый. Пользовательская авторизация не
меняет gRPC, Agent/protobuf-контракт или формат outbox.

Успешно выполнены go fmt ./..., go vet ./..., go test ./... -count=1 с отдельным
PostgreSQL 18 и AGENT_BINARY неизменённого P3 Agent. Process-level SIP test
в полном запуске действительно выполнялся. Сервер, bootstrap access CLI и
logs backfill CLI собраны для Windows/Linux; Linux integration executable
скомпилирован. Нативный Linux запуск, race detector, Docker runtime и внешний
HTTPS reverse proxy в этой среде не запускались.

Пять новых PostgreSQL access tests проверили bootstrap без перезаписи admin,
login/logout, hash-only session storage, неверные credentials, expiry,
20-session limit, роль/disable/password revocation, privilege denial,
две concurrent demotions с сохранением последнего admin и monitoring audit
atomicity. Ошибка SQL при записи audit откатывает изменение policy;
отозванный actor не может сохранить policy после HTTP проверки.

Unit/API tests проверили Argon2 salt/фиксированные параметры/Unicode,
opaque token canonical format, CSRF binding, все объявленные data routes
без credentials (401), права viewer/operator/admin, header spoofing,
cookie/Bearer смешение и дубликаты, query token rejection, default deny,
CSRF/Origin, generic storage errors и границы development config.

Независимый read-only review нашёл два дефекта rate limiting до упаковки:
отклонённый peer расходовал global budget, смена собственного пароля не имела
счётчика перед hash. Оба исправлены и закрыты regression tests: запрещённые
попытки не тратят чужой бюджет и не запускают новую проверку хеша.
OpenAPI 0.7.0 проверен на ссылки/дубликаты параметров; реальная ошибка генерации
пустых request schema исправлена и проверка ссылок добавлена в test suite.
Это техническая проверка текущего MVP, не внешняя сертификация безопасности.

Полный backend runtime, локальная schema access_demo_*, HTTP 18085/gRPC 19447:
1. Авторизация required включена; anonymous GET /api/devices -> 401.
2. Первый admin создан настоящей access CLI через password-stdin без вызова Migrate.
3. Admin создал viewer/operator; viewer GET devices -> 200, PUT monitoring -> 403.
4. Operator PUT monitoring -> 200 с атомарной записью audit.
5. Cookie PUT без CSRF -> 403, с корректным X-CSRF-Token/Origin -> 200.
6. Disabled viewer token -> 401; попытка отключить последнего admin -> 409.
7. Неизменённый Simulator healthy 6s/interval1s продолжил gRPC передачу:
   1 Agent, 3 devices (включая collector), 12 telemetry samples.
8. В БД 3 users и 10 access audit entries. Статусы и безопасная audit-выдача
   сохранены в docs/examples/access.*.fixture.json; passwords/Bearer/cookie/CSRF
   в примеры не экспортируются.
9. Временный backend остановлен по проверенному PID/executable, schema удалена.
   Пользовательская PostgreSQL на 5432 не затрагивалась.

Область MVP: локальные роли на всю установку. SSO/MFA, per-agent/tenant ACL,
email recovery, неизменяемый audit/retention, commands/rollback, ConfigUpdate,
production PKI/enrollment и incidents/reports/AI остаются отдельными этапами.
Сейчас реализованы пункты 1, 2, 3, 8, 9 списка — пять из десяти.

## Локальный этап 6: typed commands, 8 октября 2026

Подготовлено локально поверх этапов 1..5 на основе main a960eb6; GitHub не
обновлён. Agent reference checkout чистый; его исходники/protobuf не менялись.
OpenAPI расширен до 0.8.0 и проверен тестом всех schema references, auth routes
и отдельной проверкой повторяющихся параметров. Frontend не реализовывался.

Успешно выполнены go fmt ./..., go vet ./..., go test ./... -count=1 с отдельным
PostgreSQL 18 и AGENT_BINARY неизменённого P3. Оба process tests (SIP и commands)
действительно выполнялись. Server/access CLI/logs backfill собраны для Windows
и Linux; Linux integration executable скомпилирован. Docker runtime, нативный
Linux deployment, race detector и настоящая RouterOS hardware здесь не запускались.

Домен/API tests проверили типизированный HUMAN wire, обязательные confirmation/
initial-state/idempotency key, false/null, роли и disabled-auth prohibition.
Восемь новых controller tests проверили HelloAck-ready registry/session match,
concurrent ACK/commands с единственным sender, cancelled waiter, bounded Send,
transport failure, освобождение Send/Recv после возврата RPC, blocked ACK5s,
CommandResult archive compatibility и обязательный command ID.

Девять PostgreSQL tests проверяют creation/audit atomicity, global wire key
при actor-scoped client key, replay/conflict/busy, fresh qualified observations,
capabilities/metadata/expected state/protection, claim-time access/session/state
recheck, fast result before FinishSend без terminal regression, орphans и
wrong-Agent/terminal conflicts, полный rollback inbox/result/event при SQL ошибке,
nullablefalse/неполные verification, future-clock recovery, deadline/restart
uncertainty без автоматической повторной отправки.

До упаковки проверка обнаружила и исправила порядок migration008→009 на чистой
схеме, защиту Interface.Alias у переименованного порта, выбор qualified telemetry
после future clock sample, полноту времён успешной verification и различие
nanosecond Go timestamp/microsecond PostgreSQL timestamp в wire/ledger.

TestUnmodifiedAgentRouterOSCommandsThroughAuthenticatedREST:
1. Новая отдельная schema, локальный HTTPS RouterOS fixture с временным CA,
   свежий YAML/state и обычный Agent durable queue; никакого реального железа.
2. Настоящие gRPC Controller/dispatch worker/PostgreSQL и REST required auth.
3. Operator/admin request disable ether2 → success, verified=true,
   observed_admin_up=false, времена и Agent snapshot_id.
4. Повтор HTTP с тем же ключом → 200/Idempotent-Replay и прежний ID, без второго PATCH.
5. После свежей telemetry false команда enable → verified true.
6. Protected ether1 → 403/protected_resource, без асинхронной записи.
7. Ровно два PATCH, две persisted successful commands. Пример сохранён в
   docs/examples/commands.fixture.json и явно помечен local_routeros_https_fixture.
8. Schema, процессы Agent, worker и test listeners очищаются самим тестом.
   Пользовательская PostgreSQL 5432 не использовалась.

MVP намеренно не имеет force-resend/cancel/manual unlock: uncertain блокирует
новые мутации этого Agent, definitive late result может разрешить состояние.
Snapshot ID сохранён как opaque Agent-owned reference; срок/содержимое по wire
не получены. Rollback API, live ConfigUpdate, production PKI и incidents/reports/AI
ещё не реализованы. Сейчас готовы пункты 1,2,3,4,8,9 — шесть из десяти.

## Локальный этап 7: snapshot/rollback integration, 8 октября 2026

Подготовлено локально поверх этапов1..6 на основе main a960eb6. Agent reference
checkout чистый, wire/protobuf не менялся, frontend не реализовывался,
GitHub не обновлён. OpenAPI0.9.0 прошёл reference/parameter checks.

Успешно выполнены go fmt ./..., go vet ./..., go test ./... -count=1 с отдельным
PostgreSQL18 и AGENT_BINARY обычного P3. SIP, interface command и rollback
process tests выполнялись, а не были пропущены. Server/access CLI/logs backfill
собраны для Windows/Linux; Linux integration executable скомпилирован.
Нативный Linux deployment, Docker runtime, race detector, настоящий MikroTik
и внешний HTTPS proxy не заявляются проверенными этой средой.

Семь новых PostgreSQL tests проверили typed RollbackAction ownership, snapshot
consumption/new-reference/replay, precondition drift и capability/flag changes,
reservation recheck, unknown delivery/no-repeat, never-sent release, атомарный
rollback inbox/result/command/event/snapshot при SQL ошибке, supersession/ABA,
Agent snapshot_expired evidence без выдуманного TTL и обновление схемы stage6
с сохранением команд, ссылок и пользователей. Unit/API tests проверили typed
wire, origin idempotency, confirmation/limits, роли и nullable expiry.

TestUnmodifiedAgentRouterOSRollbackThroughAuthenticatedREST:
1. Собственная schema, свежий temporary Agent state/YAML/CA, loopback HTTPS
   fixture; настоящий неизменённый Agent, Controller, worker, PostgreSQL и REST auth.
2. Сценарий interface-команд этапа6 по-прежнему проходит: disable/replay/enable/protected.
3. Новое disable вернуло verified success и Agent snapshot reference.
4. POST origin/rollback отправил настоящий Command.Rollback(snapshot_id).
5. Agent восстановил up из собственного снимка и подтвердил чтением; исходная
   reference стала consumed, новая Agent-owned reference сохранена.
6. Replay того же HTTP key вернул прежний ID без нового PATCH; новый key на
   consumed reference получил409. Во всём сценарии ровно четыре fixture PATCH.
7. Пример docs/examples/rollback.fixture.json содержит local_routeros_https_fixture,
   command/result/reference metadata; passwords/cookies/Bearer/CSRF не экспортированы.
8. Agent process, sockets, worker и schema очищаются тестом. Пользовательская БД5432
   не использовалась.

Не заявляется знание содержимого/точной expiry Agent snapshots. Backend initial
state — наблюдение, не доказанная копия Agent baseline. Окончательные rollback
checks остаются в Agent; ошибки expiry/conflict сохраняются как evidence.
MVP реализует пункты1,2,3,4,5,8,9 — семь из десяти. Остаются live ConfigUpdate,
production PKI и incidents/reports/AI; первый требует согласования расширения
контракта с владельцем Agent, поскольку controller message ещё отсутствует.

## 2026-10-08 — этап 8A: сертификатная идентичность

Добавлены internal/pki identity/issuer и обязательная проверка UUID сертификата
Controller при настройке TLS_CLIENT_CA_FILE. Hello проверяется до Ingest,
последующие сообщения до сохранения; команда проверяется перед транспортом.

go fmt, go vet и полный go test ./... -count=1 успешно выполнены. Полный тест
использовал отдельный PostgreSQL18 на loopback55432 и AGENT_BINARY неизменённого
P3 Agent: integration tests не пропущены. Добавлены проверки CSR/issuer/peer,
Controller rejection-before-registration, heartbeat и command authorization,
и настоящий gRPC mTLS тест с несовпадающим/совпадающим UUID. После небольшой
дополнительной проверки preamble CSR повторно выполнены pki tests и общий vet.

Это этап8A, а не завершённый production PKI: enroll/renew, persistent bootstrap
tokens, registry/revocation и сквозная автоматическая ротация ещё не реализованы.
Количество полностью завершённых исходных пунктов остаётся семь из десяти.
Agent source/protobuf не менялись. GitHub не обновлялся.

## 2026-10-08 — этап8: HTTPS enrollment/renewal и отзыв

Добавлены migration011, persistent hash-only bootstrap tokens, certificate
registry/renewed_from/revocation, PKI audit и admin REST. PKI_ENABLED подключает
отдельный HTTPS listener и требует gRPC mTLS/REST auth/trust issuing CA.
Обычный development запуск сохранён. CLI init-ca создаёт новую папку credentials
без перезаписи; необязательный Compose overlay подключает secret mount read-only.

go fmt, go vet и полный go test ./... -count=1 успешно выполнены с отдельным
PostgreSQL18 и обычным AGENT_BINARY P3. Storage suite74.5секунды, integration
suite73.8секунды. Сценарии Agent SIP/interface commands/rollback по-прежнему
выполнены. Отдельный PKI process scenario прошёл за62.7секунды:
HTTPS enroll → gRPC mTLS heartbeat → встроенный automatic renewal через минуту
→ наблюдение нового TLS fingerprint → restart без bootstrap token → admin revoke
→ закрытие stream и отклонение reconnect с revoked certificate.

Первый вариант теста ошибочно ожидал другой session_id после TLS reconnect.
Критерий исправлен на наблюдаемый сертификат реального TLS peer: Agent сохраняет
session_id внутри одного процесса. Исходники Agent для этого не менялись.

Четыре PostgreSQL PKI tests проверили concurrent one-winner enrollment,
hash-only projection, persistence/replay после Migrate, UUID ownership,
renewal/отзыв/logout administrator, expired/revoked tokens, certificate/token
limits16 и атомарность token/certificate/audit при SQL failure. Unit/API tests
проверили identity/issuer, HTTPS/renew auth requirements, новые настройки,
admin-only routes, cookie CSRF, no-store и непротекающий token hash.

OpenAPI0.10.0 содержит management routes; reference/parameter checks пройдены.
Docker runtime, native Linux deployment, настоящий HSM/CA rotation/CRL/OCSP
и внешний HTTPS reverse proxy не заявляются проверенными.

В объёме MVP реализованы8из10 пунктов:1,2,3,4,5,7,8,9. Остались live ConfigUpdate
(требует расширения контракта владельцем Agent) и incidents/reports/AI.
GitHub не обновлён; архив не содержит CA/server/Agent private keys или raw tokens.

Финальный повторный полный прогон после проверки name constraints issuing CA
также успешен: integration suite75.0секунд, storage71.4секунды. Дополнительный
test запрещает issuance под CA, URI restrictions которого исключают infrapulse.
Проверки API admin-only/CSRF/no-store включены в этот финальный прогон.
Server и pki CLI собираются для Windows amd64 и Linux amd64; Linux runtime
не запускался. Agent reference git status чистый, изолированные DB schemas
после тестов отсутствуют. Памятка проверки — agent-pki-handoff.md.

## 2026-10-08 — этап9A: CPU/RAM/trend и lifecycle incidents

Добавлена migration012 с health_observations/incidents/transitions/evidence.
Проекция работает внутри telemetry Ingest transaction до COMMIT/application ACK.
CPU/RAM sustain, conditional CPU forecast, source/session/boot/gap isolation,
snapshot policy/version, стабильное recovery и новая occurrence реализованы.
REST viewer read/operator acknowledge использует прежние auth/CSRF и DB audit.
Live freshness вычисляется отдельно от сохранённого signal; старые simulated
sessions current=false не входят в общий status текущего запуска.

Успешно выполнены go fmt, go vet и полный go test ./... -count=1 с отдельным
PostgreSQL18/AGENT_BINARY неизменённого P3. Финальный полный прогон:
integration76.2секунды, storage92.3секунды. SIP/interface command/rollback/PKI
process fixtures выполнены, не пропущены. Agent не модифицирован.

Новые domain tests проверили ранний сигнал75% до warning80%, воспроизводимый
fit/ETA, spike/dip, missing/stale/future/duplicate metrics, reboot/gap,
поддержку более частой cadence и стабильное recovery. Четыре PostgreSQL tests
проверили lifecycle, replay, idempotent acknowledgment, recurrence/source
isolation, recovery evidence, collector loss, early forecast и отсутствие
partial inbox/telemetry/incident/evidence COMMIT при искусственной SQL ошибке.
Исторические lifecycle сценарии имеют deterministic fixture clock и synthetic
protobuf samples; они не заявляются проблемой физического оборудования.

OpenAPI0.11.0 reference/parameter/auth-route checks успешны. Новый API-role test
проверил запрет viewer acknowledge, operator/admin доступ и confirmation.
Additional API/domain checks покрывают current-vs-historical simulation health,
RAM0 vs missing и сохранённую policy/version. После их добавления выполнен
дополнительный targeted прогон и vet. Server собран для Windows/Linux; Linux
runtime и Docker execution не запускались в этой среде.

Этот шаг не закрывает пункт10 целиком: interface/SIP/collector incidents,
correlation/service dependencies, reports и AI остаются дальнейшей работой.
Полностью готовыми в MVP остаются8из10 исходных пунктов. Agent/protobuf не менялись,
frontend не создавался и GitHub не обновлялся.

## 2026-10-08 — этап9B: resource incidents интерфейсов/SIP

Migration013 сохраняет прежние device incidents/fingerprints и вводит resource
identity/health observations. Добавлены interface oper_down/utilization/errors/
drops, SIP availability/latency, source/session/endpoint continuity, nullable
SIP device FK и отдельный target FK. Resource numeric units/evidence/policy
не смешивают rates/ms с percent и не создают fake devices.

go fmt, go vet и финальный полный go test ./... -count=1 прошли с отдельным
PostgreSQL18/AGENT_BINARY P3. Integration92.1секунды, storage93.7секунды.
Прежние SIP, interface command, rollback и PKI process tests выполнены.
Новый TestUnmodifiedAgentSIPSustainedIncidentAndRecovery отдельно прошёл15.4с:
401 → 200 → UDP fixture без ответов → sustained open warning/critical incident
→ stable200 → resolved той же occurrence с evidence. Agent source не менялся.

Тест сначала ошибочно требовал immediate critical и ровно2evidence. Реальный
Agent сначала присылает degraded на первый timeout, затем down. Проверка
исправлена на ожидание critical той же active occurrence и допустимый
severity_changed evidence. Поведение Backend не переделывалось ради теста.

Unit tests: duplex utilization, availability masks, counters vs rates,
admin disabled/unknown, nil speed/latency, sustained SIP states/latency,
single timeout vs advertised ConsecutiveFailures, future/stale/endpoint change.
Три PostgreSQL tests: interface lifecycle/replay/card, SIP target/context
isolation/recovery без fake device и atomic inbox/evidence failure+retry.
Storage scenarios имеют deterministic fixture clocks/synthetic protobuf samples.
API tests: authenticated viewer resource reads, route kind/ID и coverage DTO.
OpenAPI0.12.0 references/parameters/auth-route checks успешны.

Server собран Windows/Linux amd64. Docker runtime/native Linux/реальные
сетевые устройства/PBX не заявляются проверенными. Resource thresholds этой
версии фиксированы; ожидаемые интервалы берутся из прежних manual policies.
Flap/collector-loss/correlation, reports и AI остаются дальнейшей работой.
Полностью закрытых исходных пунктов8из10; пункт10 ещё не завершён.
Agent/protobuf не менялись, frontend не создавался, GitHub не обновлялся.

## 2026-10-08 — этап9C: persistent flap и collector loss

Migration014 сохраняет interface trackers/state changes и collector trackers,
добавляет agent resource identity. Flap:4/8переходов за300секунд, quiet recovery
после300секунд с подтверждённым up; admin/unknown/reboot/gap сбрасывают continuity.
Collector worker100agents/tick3с имеет startup grace и loss/recovery confirmation15с.
Его snapshot описывает connection/last_seen, не downstream offline.

Полный go test ./... -count=1 с PostgreSQL18/AGENT_BINARY P3 успешен:
integration113.7секунды, storage159.5секунды; прежние SIP/commands/rollback/PKI
process fixtures выполнены. go fmt/go vet успешны. После OpenAPI0.13.0 и read
route updates дополнительно выполнены targeted API/health/server tests и vet.
Server собран Windows/Linux amd64, native Linux/Docker runtime не заявляются.

Четыре новых PostgreSQL tests проверили4flips/303quiet samples/recovery за пределами
120point окна, persistence после Migrate, admin/unknown без fake physical flip,
transient disconnect/confirmed collector loss/recovery, отсутствие fabricated
devices.online=false, startup grace и atomic SQL failure/retry. Это synthetic
fixtures с controlled clocks/connection rows, не живое отключение сети.
API read test дополнен authenticated agent resource route; OpenAPI refs/parameters/
auth-route checks успешны. Agent reference остаётся чистым, DB schemas очищены.

Первые11health rules работают, но correlation/reports/AI ещё не реализованы.
Пункт10 не закрыт целиком, полностью завершённых пунктов8из10. Agent/protobuf
не менялись, frontend не создавался, GitHub не обновлён.

## Этап9D — отчёты, recurrence и frozen JSON snapshots

Migration015 добавляет report_snapshots и period/received indexes. Summary
читает одну read-only Repeatable Read transaction: [from,to), clipped backend
incident durations, event means, rule/resource groups, source/session/context
recurrence, raw received sample quality. Summed seconds не uptime; availability
остаётсяnull. Snapshot creation сохраняет coherent JSON и audit отдельной
actor/idempotency transaction; late data не переписывает original report.

Полный go fmt/go vet/go test ./... -count=1 с PostgreSQL18/AGENT_BINARY P3 успешен:
integration112.5с, storage170.0с. Прежние Agent SIP/commands/rollback/PKI process
fixtures выполнены. Domain/API/DB report tests проверили period boundaries,
timezone identity, carry-in/durations/events/null means, recurrence without
mixing simulated sessions, concurrent one-snapshot idempotency, immutable
payload, conflicts, audit SQL failure, revoked actor и metadata list.
API tests также проверили protected read, attachment JSON, роли и cookie CSRF.

OpenAPI0.14.0 references/parameters/auth-route checks успешны. После additions
проведены targeted report fixture/API/domain tests и vet; server собран
Windows/Linux amd64. docs/examples/report.fixture.json экспортирован из
проверенного synthetic lifecycle fixture; содержит scenario label и no credentials.
Agent reference чистый, изолированные schemas очищены. Docker/native Linux/
физическое оборудование не заявляются проверенными.

Reports on-demand/explicit JSON snapshots реализованы; periodic scheduler,
PDF/email/CSV, correlation и AI остаются дальнейшей работой. Пункт10 ещё не
завершён, готовых исходных пунктов8из10. Agent/protobuf не менялись, frontend
не создавался, GitHub не обновлялся.

## Этап9E — contextual diagnosis без causal proof

Добавлены diagnosis domain и GET incident/context с repeatable-read snapshot
seed/candidates/freshness/current topology. Relations same collector/device/
resource/discovery adjacency и matched LLDP/CDP port дают явные основания,
не вероятности причины. Source/simulation isolation, expiry/high-resolution
checks, historical-seed abstention и bounded selection/truncated реализованы.

go fmt/go vet/полный go test ./... -count=1 с PostgreSQL18/AGENT_BINARY P3 успешны:
integration106.1с, storage115.4с. Прежние реальные Agent SIP/commands/rollback/
PKI process fixtures выполнены. После небольшой корректировки topology gating
повторены diagnosis/API/targeted DB tests и vet; server собран Windows/Linux.

Domain tests: fresh mapped port without cause, expired/medium resolution,
Agent/simulator isolation, SIP no invented service dependency, collector/time
bounds, historical seed/truncation. PostgreSQL test: two known devices, exact
LLDP chassis MAC resolution, fresh context, expiry и resolved incident without
current-topology historical claim. Эти DB fixtures synthetic, не hardware outage.
API auth/limits и OpenAPI0.15.0 reference/parameter/auth-route checks успешны.

Внешнего AI/export/remediation нет. Следующая часть пункта10 — controlled preview/
provider analysis. ConfigUpdate требует владельца Agent; готовых пунктов8из10.
Agent/protobuf не менялись, frontend не создавался, GitHub не обновлён.

## Этап9F — AI preview/consent/provider/result storage

Migration016 и asynchronous worker реализуют frozen allowlisted export,
hash/expiry/actor-owned confirmation, idempotency/single-use preview, exact
provider request, JSON schema/evidence-reference validation и PostgreSQL result.
Model/key обязательны только при AI_ENABLED=true; defaultfalse не отправляет
внешние запросы. Fixed HTTPS endpoint, redirect/proxy-by-env prohibition,
response/timeout/output bounds, storefalse и no tools/device commands реализованы.

go fmt/go vet/финальный полный go test ./... -count=1 успешны с отдельным
PostgreSQL18/неизменённым AGENT_BINARY P3: integration90.6с, storage84.9с.
Прежние SIP/commands/rollback/PKI process fixtures выполнены. Additional
API/schema/export/config tests и vet повторены; server собран Windows/Linux.

Unit fixtures проверили sentinel privacy, exact reviewed JSON/header/hash,
reference/enum checks, redirect/refusal/incomplete и отсутствие duplicate call.
HTTP provider fixture локальный, не реальная OpenAI модель. PostgreSQL tests:
job replay, single-use preview, result, disabled/unconfirmed/expired requests,
logout до claim, restart uncertain/no resend, SQL audit rollback; worker fixture
подтвердил queued→running→succeeded с одной обработкой mock Provider. API roles/
CSRF и explicit config checks успешны. OpenAPI0.16.0 references/parameters/auth
route checks пройдены. Agent source/protobuf/frontend/GitHub не менялись.

Официальные документы OpenAI Structured Outputs/Responses/Data controls
использованы для provider wire. Реальные model/key/price/answer quality и целевая
deployment среда НЕ проверены. Эти acceptance проверки остаются обязательными;
не считаем storefalse гарантией полного отсутствия хранения у provider.
Функции пункта10 реализованы в MVP, live-provider acceptance пока открыт.
ConfigUpdate требует владельца Agent; весь проект не объявляется завершённым.
# Backend v2 B0 — 2026-10-10

## Backend v2 B1 — 2026-10-10

Canonical Collector d456fea синхронизирован additively, только backend
go_package отличается. Frozen P2 и Stage9F fixtures неизменны.
Полный go test -count=1 ./... с PostgreSQL, старым AGENT_BINARY и новым
COLLECTOR_V2_BINARY: PASS (integration 91.233s, storage 81.095s).
Новый process test проверяет Redfish HTTPS/verified CA, remote Syslog TCP,
durable replay при lost ACK после COMMIT, накопление при отключённом backend,
восстановление соединения и авторизованный REST.
Дополнительный populated Stage9F -> migration017 -> repeat migration test PASS.
gofmt, vet, Windows/Linux cross-build и reproducible proto generation check PASS.
OpenAPI 0.17.0, старые маршруты сохранены. Agent checkout чистый.
Race/Docker/Linux-only process suite/реальный BMC/AI provider не проверены.
Подробности: docs/backend-v2-b1.md. B2–B16 ещё не реализованы.

Утверждённый план B0–B16 принят. Сохранён Stage9F ZIP и создана локальная ветка
feature/backend-v2-b0-baseline, без commit/push. Добавлены frozen Stage9F
FileDescriptorSet и fixture 71 защищённого REST-маршрута с правами.
gofmt, vet, Windows/Linux build, полный uncached test с PostgreSQL 18 и
неизменённым Agent прошли. Новые contract tests после добавления прошли.
Race и Docker локально недоступны; Linux-only durable process E2E не запускался.
CI checks расширены, но ещё не запускались на GitHub.
Подробности: docs/backend-v2-b0.md. B1 ожидает актуальный Collector Phase 3–5.

## Backend v2 B4.1 + Collector Phase8 compatibility — 2026-10-11

ZABBIX production registry now contains real read-only test/sync operations.
Official JSON-RPC apiinfo.version/host.get are called through the existing
HTTPS origin/CIDR/DNS/TLS policy. Configured selected hosts are paged with an
atomic inventory/cursor/job commit. Credentials remain encrypted and private.
Zabbix target versions supplied by the team: 7.4.15 and 7.0.31; fixtures cover
both and the legacy 6.0 wire format. Prometheus target for B5: 3.15.0.

New Agent main pinned at 215d33a4a5fbc2a12c57d31e516b0ad33f7ae574, fetched to
an independent read-only reference checkout. Backend canonical proto synced
(go_package only differs), generated code reproducible. Agent checkout clean;
Agent code, old frozen descriptors, frontend and GitHub untouched.
Expanded identities include subject/origin/time/normalization; invalid/rejected
facts do not become match evidence. Identity clocks alone do not grow history.
ConfigApplyStatus is validated/archived/deduplicated; B9 is not negotiated.

Full go fmt / go vet / uncached go test ./... PASS with separate PostgreSQL18,
unmodified old P3 Agent and newly built unmodified Collector 215d33a.
Integration package: 125.974s; storage: 201.434s. Process fixtures exercised
new Collector Redfish/Syslog/gRPC/REST, loss of ACK after commit, retained replay,
outage recovery, prior SIP/commands/rollback/PKI and auth scenarios.
Zabbix tests exercised authentication/version/errors and real protected TLS
transport to a local JSON-RPC fixture, queue/encrypted token, PostgreSQL Assets,
provider-state history, unchanged-fact dedup, cursor, revision cancellation,
source isolation and no invented Agent. OpenAPI0.24.0 API/ref/auth checks PASS.
Canonical generation check PASS; backend Windows/Linux amd64 builds PASS.

No connection to the team's real Zabbix has been accepted yet. Fixture PASS is
not real-server/hardware acceptance. B4 metrics/problems/events and Backend B9
managed configuration remain subsequent work. Docker/race/Linux-only process
checks and real IPMI/BMC/AI-provider acceptance were not run in this stage.
Details: docs/backend-v2-b4-zabbix.md and
 docs/backend-collector-phase8-compatibility.md.

## Backend v2 B4.2 — 2026-10-11

Opt-in Zabbix monitoring adds selected numeric item values, paged active
problems/history, bounded reconciliation of previously active event IDs and
explicit recovery-event lookup. Source-instance/event identity is preserved;
missing data/API failures never infer zero values or recovery. Decimal/uint64
values retain numeric precision before explicit unit conversion.

Migration024 adds external metrics/signals/membership/version tables and extends
common Incidents with nullable Agent only for validated external resources.
Inventory/monitoring/cursor/job/incident effects commit atomically. Existing
Collector constraints, application ACK, commands, auth and PKI remain intact.
External signals use dynamic SourceBindings, so Asset merge/split does not freeze
old Asset IDs. Legacy AI preview rejects external incidents explicitly until
B12/B13 evidence export; local acknowledgment never mutates Zabbix.

Full go fmt / go vet / uncached go test ./... PASS with separate PostgreSQL18,
old unchanged P3 Agent and unchanged final Collector 215d33a. Integration package
120.484s, storage 207.403s. Source tree of Agent remains clean. Windows/Linux
amd64 backend builds PASS. Frozen/canonical contract and baseline REST protected
routes pass. No commit/push/GitHub mutation.

New real protected TLS + queue worker + PostgreSQL + authorized REST fixture
passes. It proves source token encryption/no REST echo, selected Assets/metrics/
signals, common Incident acknowledge and explicit recovery. Unit fixtures cover
API 6.0/7.0.31/7.4.15, missing/non-numeric values, units/precision, cross-host
rejection and safe API errors. DB tests prove dedup, active/acknowledged/resolved,
no disappearance recovery, error freshness, rollback after evidence-write failure,
dynamic merge links, distinct identical event IDs across sources and revision
fencing of an obsolete recovery. API tests prove all read roles, unauthorized
401, input bounds and OpenAPI0.25.0 references/parameters/route coverage.

The JSON-RPC servers are fixtures, not actual Zabbix installations. Endpoint and
host/item IDs for a live acceptance are unavailable. Scope is selected numeric
items and bounded source0/object0 trigger events; full Zabbix history, correlation
closure variants, automatic all-host discovery, external AI/cross-source root
cause and production scaling acceptance are not claimed. Docker/race/Linux-only
process checks are unavailable locally. Agent/Collector/frontend unchanged.
Next planned connector B5 targets team-specified Prometheus3.15.0.
Details: docs/backend-v2-b4-monitoring.md.

Final B4.2 hardening: publication decodes persisted source settings into a zero
value, preventing omitted fields from retaining an adapter request policy.
Focused source queue/Zabbix/external publisher/resolver regression PASS (12.716s),
TLS-worker-DB-REST regression PASS (3.785s), and explicit stored-policy override
rejection test PASS. Final source settings remain authoritative.

## Backend v2 B5 — 2026-10-11

PROMETHEUS registers read-only test/sync operations and scoped on-demand
instant/range/target metadata. Stable source-owned targets derive IDs from pool
and configured identity labels; no inferred physical identity from instance.
Node exporter starter mappings cover up/CPU/RAM/disk/network/errors; optional
scoped uname/DMI inventory remains medium evidence unless explicitly verified.
Official PromQL parser v0.315.0 injects exact target labels into EVERY selector.
No viewer raw queries/URLs, @/negative offset, huge range or unlimited result.
TLS/origin/CIDR/pinned-DNS/redirect/proxy policy is reused. Target URLs, discovered
secrets and raw errors are not persisted or followed. Major3 wire target:3.15.0.

Migration025 keeps only latest Prometheus snapshots; source history is queried
on demand and never copied to Zabbix snapshot history. Inventory/latest/cursor/
job completion are atomic. Scrape times remain separate from evaluation times;
stale scrape is not fresh from re-query, NaN/Inf/null and ambiguity are explicit.
Asset/source revision and binding ownership are checked before/after requests.
Endpoint/settings/binding/secret read a single repeatable-read snapshot, then
release DB transaction before HTTPS. Source edits cannot mix address/password.

Full go fmt/go vet/uncached go test ./... PASS with own PostgreSQL18, old unchanged
Agent P3 and unchanged Collector215d33a. Integration119.861s, storage215.060s.
Scope/freshness/identity/profile/query-bounds tests and API read-role/401/no raw
query substitution tests PASS. Real protected HTTPS transport to an API fixture
plus PostgreSQL proves target Asset/latest, range, no fake Agent/no copied history,
foreign Asset rejection before network and late settings edit response fencing.
Canonical/frozen contract and legacy source/Zabbix/commands/PKI regressions pass.
Focused regression rerun after snapshot hardening passes; Windows/Linux builds
verified. OpenAPI0.26.0 references/route/parameters tests PASS.

No actual Prometheus3.15.0 deployment/node_exporter acceptance yet; fixture is
synthetic. Docker/race/Linux-only process suite, hardware and enterprise scaling
not accepted. Prometheus alert ingestion/global correlation remains B12; B6-B16
are subsequent work. Agent code/frontend/GitHub unchanged. Details:
docs/backend-v2-b5-prometheus.md.

## 2026-10-12 — B6 unified inventory / physical locations

Added separate resolved-inventory API with current per-attribute claims, source
instance/binding/fact IDs, confidence, provenance, observation/receipt times and
explicit conflicts. Highest-confidence contradictions are not hidden by the
canonical binding. Root/component identities remain scoped; rejected identity
cannot become usable through its legacy field. IP union, all selected Zabbix
management interfaces and source-scoped components/hardware are covered. Missing
fields, disabled sources and unknown inventory do not imply healthy/live data.

Migration026 adds location catalog, reviewed placements, per-U occupancy,
placement/catalog audit and actor-scoped idempotency. Hierarchy/capacity guards,
stable placement revisions, multi-U/outside-rack, atomic imports/swaps, rollback,
concurrent overlap rejection and live admin revalidation are tested on own
PostgreSQL18 at 127.0.0.1:55432. Upgrade from populated B5 and repeat migrations
preserve prior assets and placements. Merge cannot abandon placed alias; split
does not duplicate occupancy. Inventory refresh never changes reviewed placement.

Final go fmt ./..., go vet ./... and uncached go test ./... PASS with disposable
PostgreSQL schemas, unchanged old Agent P3 and unchanged Collector215d33a.
Integration128.907s, storage247.009s. The first new process assertion incorrectly
selected only real scope in an explicitly simulated Redfish fixture; corrected
to preserve its source namespace. Final process test reads unified inventory and
placement through authenticated HTTP after real Collector gRPC ingestion.

Windows amd64 and Linux amd64 builds PASS (CGO disabled). Canonical Collector
proto and reproducible generated code PASS; frozen protocol/legacy API regressions,
Zabbix, Prometheus, commands, rollback and PKI tests PASS. OpenAPI0.27.0 references,
anonymous deny and read/admin permissions PASS; whitespace check PASS.

No Agent or frontend code edits, commits, pushes or GitHub writes. Docker,
Linux-only process/race suite, physical hardware and deployment acceptance remain
unverified. Ansible connector is B10; topology globalization is B7; B7-B16 remain
future work. Existing single-Controller ownership limitation remains until B16.
Details: docs/backend-v2-b6-inventory-locations.md.

## 2026-10-12 — B7 global Asset topology

Added global topology alongside unchanged default legacy topology. Existing
Asset mappings collapse source bindings into one node; typed ports group
directed evidence, preserving source/protocol/raw bytes/confidence and TTL.
Unique chassis match uses complete inventory; hostname/IP are tentative only,
ambiguous/conflicting/rejected identities remain unresolved. Real/simulated
source/session namespaces stay isolated. Unknowns never create Assets or health.
Management relations BMC_MANAGES_SYSTEM/CHASSIS are separate, with BMC target,
source type/protocol/instance/resource scoping and inverse normalization.

Migration027 preserves legacy snapshots and seeds current accepted observations
in bounded chunks with imported events. New latest/version/lifecycle projections
share the inbox transaction; ACK/replay/error rollback semantics preserved.
Updates batch by 256. Expiry worker batches500 each second; graph checks expiry
at read time independently. History supports observed/changed/revived/expired,
source filters and keyset pagination. Immutable evidence vs current Asset mapping
is explicitly distinguished; arbitrary-date graph reconstruction is not claimed.
Bounds/truncation are visible and never masquerade as unique entity resolution.

go fmt ./..., go vet ./... and uncached go test ./... PASS with own PostgreSQL18,
old unchanged Agent P3 and unchanged Collector215d33a. Integration141.248s,
storage370.358s. Multi-Collector fixture verifies two known physical Assets,
one grouped link and three directed evidence with reciprocal ports. Ambiguity,
simulation, port changes, out-of-order/future observations, clock/replay dedup,
business history, TTL, upgrade, migration repeat, rollback and role/query checks
PASS. Real Collector Redfish process -> gRPC -> PostgreSQL -> authenticated
global REST proves BMC/system relation resolves without inventing a cable.

After the full suite, expiry catch-up was hardened: a renewal arriving before
the expiry worker now records its missed expired transition before revived in
the same transaction. Final focused global+legacy topology PostgreSQL regression
PASS16.893s; go vet rerun PASS. Windows/Linux amd64 builds PASS (CGO disabled).
Canonical/frozen contract, reproducible generated code, OpenAPI0.28.0 references,
legacy routes, Zabbix/Prometheus/commands/PKI/inventory regressions PASS.
Whitespace check PASS; Agent checkout clean; Backend HEAD unchanged.

No GitHub writes, commits, pushes, frontend or Agent edits. Physical switches,
Docker, Linux-only process/race suite and enterprise scale remain unaccepted.
BMC relation changes retain inventory fact history; Asset merges/splits retain
their own audit. Existing single-Controller limitation remains until B16.
Details: docs/backend-v2-b7-global-topology.md. B8-B16 remain subsequent work.
