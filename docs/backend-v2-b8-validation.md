# Проверки B8 — 12 октября 2026

Проверена локальная рабочая версия Backend v2 через B8. Среда: Windows, Go 1.27.1, отдельный PostgreSQL 18 на loopback 127.0.0.1:55432. Пользовательская база на стандартном порту не использовалась. Тесты создают отдельные схемы и удаляют их после завершения.

## Успешные проверки исходной копии

- go fmt ./...: форматирование выполнено; повторный вызов не меняет файлы.
- go vet ./...: все пакеты прошли.
- go test ./... -count=1 с TEST_DATABASE_URL, AGENT_ROOT, AGENT_BINARY и COLLECTOR_V2_BINARY: все пакеты прошли. Пакет integration — 104.590 s, storage — 306.956 s.
- Сборка cmd/server для Windows/amd64 и Linux/amd64, CGO_ENABLED=0: успешна.
- scripts/generate-proto.ps1 -Check с protoc 33.5 / protoc-gen-go / protoc-gen-go-grpc: canonical Agent contract и reproducible generated Backend code — PASS.
- Read-only git status Collector 215d33a: пустой; HEAD 215d33a4a5fbc2a12c57d31e516b0ad33f7ae574. Backend HEAD a960eb676c3a036397d0c375ccfd0851fb09d9a9 не менялся: новых commit/push нет.

## Существенные сценарии B8

Hardware: source ownership, ноль и Power Off, отсутствие ложного Critical, late/future clock handling, SourceRef target guard, verified BMC relation, merge/split, идемпотентный seed, исходное received_at при upgrade, transactional rollback и просроченное connected/last_seen.

Logs/audit: точная source/device/session привязка, независимые host claim и transport metadata, unknown sender в общем журнале, replay/reread dedup, целая audit-группа при фильтре одной строки, group cursor, merge/split, retained typed simulation с прежним пустым scope, сохранение audit keys и fingerprint, scope filter. HTTP tests проверяют viewer/operator/admin, некорректные query/encoding, отсутствие raw bypass через legacy list/detail/audit-group routes. Store raw tests проверяют реальную текущую роль и сессию БД, revoke/expiry и отказ выдачи при ошибке audit.

VoIP: explicit reviewed mapping, observed endpoint identity, nullable response/latency и измеренный ноль, placement context, current revisions, concurrent update, idempotency, audit rollback/live admin, merge/split, address/transport change, real/typed-sim batch/target scope и новая сессия. Cross-source тест связывает реальную SIP-точку Collector с PBX inventory Zabbix без выдуманного legacy device.

Process fixtures: неизменённый финальный Collector запускается отдельным процессом против Redfish/syslog fixtures, подключается по gRPC, проходит повторную доставку после потери ACK и недоступности backend. Новые hardware/latest/history и B7/B6 контекст читаются через authenticated REST. Прежние process tests Agent SIP, commands/rollback и PKI также проходят общим прогоном. Это локальные fixtures, а не заявление о приёмке физического оборудования.

OpenAPI 0.29.0: ссылки на schemas проверены; заявленные маршруты требуют authentication; 71 маршрут Stage9F и его permission сохранены. Изменение обычного поля log.raw на пустое значение намеренное и описано: привилегированное чтение вынесено в audited admin route.

## Что этот прогон не подтверждает

Не запускались Docker Compose/CI GitHub, race detector и Agent native Linux collector на настоящей Linux-машине. Linux бинарник Backend собран, но production deployment здесь не выполнялся. Не предоставлены командные Zabbix/Prometheus endpoints, физические BMC/syslog/АТС; их acceptance остаётся у команды. B9 managed configuration, B10–B16 и frontend не входят в завершённый B8.

Архив B8 упакован по whitelist исходников/документации, без .git, runtime/state, executable tools, .env, сертификатов и секретных ключей. При распаковке сравниваются SHA-256 всех файлов с рабочей копией; отдельная проверка распакованной копии выполняется перед выдачей архива.
