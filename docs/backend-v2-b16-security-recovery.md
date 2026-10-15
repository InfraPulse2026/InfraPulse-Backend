# B16 — безопасность, восстановление и итоговая проверка

Дата локальной проверки: 15 октября 2026. Изменён только backend. Agent reference `215d33a4a5fbc2a12c57d31e516b0ad33f7ae574` и frontend не редактировались. Git commit/push и публикация GitHub не выполнялись.

## Исправления B16

1. В server и Automation Worker установлен общий safe logging handler. Ошибки больше не сериализуются целиком: известные SQL/network/cancellation ошибки становятся закрытыми operational codes. DSN, password/token/private_key, payload, SQL/detail и другие явно секретные поля скрываются, включая вложенные группы и inherited attrs. Строковые error/cause/reason тоже скрываются. Идентификаторы заданий остаются для поиска. Ошибки configuration/database startup также имеют фиксированную phase, чтобы оставался безопасный диагностический ориентир. Тест включает PostgreSQL Detail, wrapped DSN, provider error, вложенный token и предварительно преобразованную в строку ошибку.
2. Мигратор отклоняет неизвестные версии схемы выше 045 и некорректные markers ниже 1. Старая сборка не должна молча запускаться с более новой схемой. Все pending migrations сохраняются прежней общей транзакцией; опубликованные SQL миграции не переписаны.
3. Проверен отказ миграции 045, отсутствие ложного marker, устранение конфликта и успешный повтор. Искусственный marker 046 блокирует текущую сборку. Повтор на поддерживаемой схеме проходит.
4. Server lifecycle test запускает два последовательных runtime против одной схемы и тех же портов: startup/migrations, readiness, worker shutdown, повторный старт. Это проверка настоящих listeners и функции запуска, а не тест crash/kill отдельного server executable.
5. Контрольный backend workflow Zabbix+SNMP+SIP продолжен до case preview, запрета job без consent, подтверждённого job, сохранённого validated result и immutable unified report. Три сигнала остаются одним эпизодом. Provider result контролируемый; это не вызов настоящего LLM.
6. Добавлен opt-in тест реального перезапуска PostgreSQL: pool теряет соединение, восстанавливается, миграции повторяются, исходный envelope воспроизводится без дубля, новый sample записывается, история сохраняется. Перезапуск выполняет внешний harness только на disposable instance; сам Go test сервисы не останавливает. Cleanup после restart использует новое соединение и удаляет только точную временную схему теста.

Safe logging не является универсальным фильтром любых сообщений: log message и произвольные новые attrs не анализируются на все возможные секреты. Production call sites передают фиксированные messages, ошибки как error attrs и технические IDs. Не следует добавлять raw connector bodies, credentials или operator text в сообщения. Payload в PostgreSQL и доступ к нему имеют отдельную политику; этот handler не шифрует базу и не очищает уже существующие журналы.

## Что проверено локально

| Проверка | Результат и границы |
| --- | --- |
| Go fmt/vet и полный Go test с PostgreSQL | Пройдено, packages выполнялись с `-p 1`, timeout 25m; storage 650.220 s. Optional tests без своего стенда пропущены явно. |
| RBAC/session/CSRF | API и storage tests пройдены, включая новые AI/correlation/report routes, admin-only schedules, отзыв текущего actor и cookie CSRF. |
| Secrets/TLS/SSRF | Пройдены vault, source network policy, private CA, запрет redirect/proxy/неразрешённого адреса, safe connector failures и bounded PromQL/query/result tests. |
| Command/remediation approval | Пройдены typed commands, snapshot ownership, admin approval, stale policy/evidence fences, uncertainty и запрет произвольных команд. |
| Python automation adapter | 4 contract tests пройдены с fake Runner. SSH host-key enforcement, fixed tasks/vendor module, extra fields rejection и filtered facts проверены. Linux Ansible/hardware execution не выполнено. |
| Replay/atomicity/restart | Пройдены rollback/commit failure, store pool restart, concurrent replay, case queues и uncertain AI/command/automation recovery. |
| Настоящий PostgreSQL restart | Пройден отдельным executable-тестом после stop/start disposable PostgreSQL 18 на 127.0.0.1:55432. Тест наблюдал disconnect и recovery; 2 записи истории после replay+новой записи. |
| Неизменённый Collector process | `TestUnmodifiedCollectorV2RedfishSyslogReplayAndREST` пройден с локальным executable reference 215d33a: 4 log delivery attempts, 2 unique records, offline admission/replay, Redfish inventory/telemetry и authorized REST. Источники контролируемые loopback fixtures. |
| Остальные opt-in process tests | Отдельный integration прогон 142.237 s пройден с неизменёнными локальными Agent P3 и Collector 215d33a: RouterOS typed commands/rollback на fixture, managed ConfigUpdate live apply/reject/restart/clear, HTTPS enrollment/UUID binding/renewal/revoke, SIP timeout/401/recovery и Zabbix TLS queue/REST. Оригинальные executable не правились; источники эмулированные, не hardware acceptance. |
| Protobuf | `scripts/generate-proto.ps1 -Check` пройден: canonical reference соответствует backend кроме go_package, generated files воспроизводимы. Проверен конкретный reference commit, а не неизвестный будущий GitHub main. |
| Multi-source → AI → report | Пройден контролируемый PostgreSQL workflow; источник/модель не объявлены реальными только из-за успешного fixture. |

Нативный диапазон MetricStore не добавляет нового публичного REST маршрута. Существующий metrics/history JSON сохранён. Секреты источников не попадают в source read API; конфигурационные baseline/raw snapshots требуют административного доступа, а summary drift ограничен очищенными данными.

## Повторяемый запуск проверок

Нужны Go и expendable TEST_DATABASE_URL. Полная команда: `go test -p 1 ./... -timeout 25m -count=1 -v`, перед ней `go fmt ./...` и `go vet ./...`. Результат без TEST_DATABASE_URL не считается проверкой PostgreSQL. `python -B scripts/test_automation_runner.py` проверяет adapter с подменённым Runner.

Для Collector process установить COLLECTOR_V2_BINARY в путь к исполняемому файлу согласованного reference, затем запустить указанный integration test. Исходники Collector не нужно править.

Для PostgreSQL restart-test использовать отдельную disposable instance, а не общую рабочую базу. Установить абсолютный INFRAPULSE_RESTART_BARRIER и запустить только `TestDatabaseProcessRestartPreservesReplayAndPoolRecovery` с timeout 4m. После появления barrier внешний harness делает stop/start именно этой instance. Тест ждёт disconnect/reconnect до трёх минут. В этой проверке directory и postmaster.opts были проверены: InfraPulse/tools/postgres-test-data, loopback 55432. Основной PostgreSQL 5432 не затрагивался.

## Deployment и HA readiness

Поддерживаемая схема MVP: один active backend, PostgreSQL с постоянным volume, отдельный Linux Automation Worker при его явном включении, HTTPS reverse proxy для REST, gRPC mTLS/PKI для управляемых Collectors. Demo insecure режим остаётся явным локальным режимом; он не доказывает production transport security. Ключи vault/CA/credentials хранятся отдельно от ZIP/Git и должны переживать восстановление базы.

Проект не объявляет многорепликовый backend уже реализованным HA. Active gRPC streams и readiness ownership находятся в памяти конкретного controller. Startup RecoverCommands/RecoverJobs также рассчитан на единственный active controller. Один load balancer и два экземпляра не решают маршрутизацию команд и могут конфликтовать с recovery.

Для будущей active/passive схемы требуется внешнее fencing прежнего active instance до запуска нового; сохранение DB/vault keys; reconnect Collectors; проверка durable queues и uncertain outcomes. Автоматический failover/fencing здесь не реализован. Для active/active дополнительно нужны durable session-owner leases с epochs, маршрутизация dispatch к владельцу stream и scoped startup recovery. Это reference architecture, не обещание работающего distributed controller.

PostgreSQL HA cluster и отдельная TSDB не устанавливались. Проверены восстановление соединения и сохранение фактов после обычного restart disposable database; failover репликации, backup restore и дисковая катастрофа этим не доказаны.

## Открытая приёмка команды

- Реальные Zabbix 7.0.31/7.4.15 и Prometheus команды: endpoint, TLS, разрешённая учётная запись, selected hosts/targets, inventory/history/problems и недоступность источника. Локальные HTTP/TLS fixtures проверяют контракт и отказ, но не экземпляры команды.
- Реальные/эмулированные устройства и матрица шести вендоров: фактически проверенные capabilities и модели. Generic SNMP, vendor integration и simulated support должны быть различимы. Сообщённый vendor не доказывает испытание устройства.
- Linux Ansible Runner, выбранное SSH-устройство и настоящий approved change → recollection → verified/rollback. Backend/adapter workflow проверен, физическое изменение не выполнено.
- Разрешённый реальный LLM, выбранная модель и reviewed export → confirmed job → ответ со ссылками. Provider, consent, privacy и result validation проверены без платного запроса.
- Финальный сетевой demo с актуальным согласованным Collector и frontend другого разработчика. Backend reference совпадает с указанным commit; дальнейшие обновления Agent требуют повторной сверки контракта.

Локальная реализация и контролируемая B16 проверка завершены после сборки/проверки итогового архива. Открытые испытания выше не отмечаются выполненными без фактических результатов.
