# Проверки B9 — 12 октября 2026

Локальный backend v2 B0–B9, Windows, Go 1.27.1. Отдельная тестовая PostgreSQL 18 на `127.0.0.1:55432`; тесты используют временные схемы. База пользователя на порту 5432 не затрагивается. Agent/Collector только читается и запускается как неизменённый бинарник.

## Полный прогон и последующие изменения

- `go fmt ./...` выполнен; итоговый повторный вызов не меняет файлы.
- `go vet ./...` прошёл для всех пакетов; повторён после последнего уточнения B9.
- `go test ./... -count=1` с PostgreSQL, AGENT_ROOT, прежним AGENT_BINARY и финальным COLLECTOR_V2_BINARY прошёл. Integration — 135.073 s; storage — 334.181 s. В прогон входят прежние Agent SIP/commands/rollback/PKI и Collector Redfish/syslog/replay/REST fixtures.
- После обновления OpenAPI до 0.30.0 отдельно повторены API/collectorconfig/monitoring/telephony тесты. Все прошли; 945 ссылок OpenAPI разрешены, четыре B9-маршрута имеют admin permission.
- После дополнительной защиты от heartbeat, созданного до перезапуска Collector, повторены все B9 PostgreSQL scenarios и B9 real Collector process scenario. Этот целевой повтор проверяет последнюю версию защиты; полный длительный прогон выше выполнен до этого уточнения.
- Backend `cmd/server` собирается для Windows/amd64 и Linux/amd64 с CGO_ENABLED=0.
- `scripts/generate-proto.ps1 -Check`: canonical Agent contract и reproducible generated Backend code — PASS. Номера полей и proto Collector не изменялись в B9.
- Collector Git status пустой, HEAD `215d33a4a5fbc2a12c57d31e516b0ad33f7ae574`; backend HEAD остаётся `a960eb676c3a036397d0c375ccfd0851fb09d9a9`. Commit/push не выполнялись.

## Проверенные сценарии B9

Canonical encoding: независимый Python golden vector из финального Agent совпадает побайтно; transport/session/revision metadata не входит в digest; normalization не меняет входную структуру. Проверены defaults всех пяти протоколов, duplicate/unknown field rejection, границы 24h и hardware minimum, пригодность опубликованного JSON-примера.

Storage: immutable revisions/history, idempotency replay/conflict, current revision conflict, одновременные записи без потери изменения, audit rollback в одной транзакции, действующие admin role/session, demotion, закрытые secret-bearing поля, подтверждённая очистка, terminal state monotonicity, неизвестный unsolicited result, session reconnect и matching Hello LKG, highest rejected revision и uint64/bigint overflow. Старые сообщения не создают повторных domain effects.

Runtime proof: standalone APPLIED остаётся `applied_reported`; current Hello/Heartbeat подтверждает `applied`. Timestamp heartbeat проверяется относительно Agent session started_at. Retained heartbeat старого процесса сохраняется как uncorrelated и не подтверждает применение. Повтор одинакового heartbeat не увеличивает config events. Потеря connection даёт `in_sync=false`.

Freshness: только matching real Collector/Target/protocol получает applied polling. Desired и report-only state не меняют период. Hardware 24h не даёт ложного stale по прежнему 300s default; explicit max_age_seconds остаётся приоритетным. Ручная freshness policy и local target с похожим raw ID не перезаписываются.

Security/REST: viewer/operator/anonymous не получают admin config routes; неизвестный/слишком большой body и лишние параметры отвергаются. Backend flag требует mTLS+REST auth. Даже permissive authorizer вместе с plaintext не включает negotiation и не позволяет отправить desired snapshot. Existing command/ACK sender используется без одновременных Send.

## Реальный процесс Collector

`TestUnmodifiedCollectorManagedConfigLiveApplyRejectRestartAndClear` использует финальный бинарник Collector 215d33a, настоящую PostgreSQL, gRPC с client mTLS, HTTPS enrollment и локальный UDP SIP responder. Управление выполняется через authenticated REST.

Проверяет:

1. Matching Collector UUID, локальный remote-management opt-in и negotiation.
2. Добавление managed SIP endpoint и реальный OPTIONS probe.
3. Изменение 3s → 6s: измерены фактические интервалы проб, PID и gRPC session остаются прежними.
4. Правильное отражение applied периода 6s в backend freshness; raw SIP health ID и remote SourceRef учитываются отдельно.
5. Отклонение дополнительного SNMP target с неразрешённым local credential_ref; предыдущая SIP конфигурация продолжает опрос.
6. Перезапуск без bootstrap token и восстановление последней успешной managed конфигурации.
7. Пустой snapshot очищает managed targets, при этом local SIP target из YAML продолжает опрос.
8. История applied/rejected доступна через REST.

Это проверка совместимости с реальным неизменённым Collector на локальных protocol fixtures. Она не является приёмкой физического Cisco/Eltex/BMC или реальной корпоративной АТС.

## Границы проверки

На этой машине не запускаются Docker Compose/GitHub CI, race detector и native Linux collector на реальной Linux-машине. Linux backend бинарник собирается, но production deployment не выполнялся. Адреса командных Zabbix/Prometheus и физического оборудования пока не предоставлены. Managed SNMP/RouterOS/Redfish/IPMI definitions проверены по canonical contract/validation; live protocol acceptance именно их B9-настроек остаётся за стендом команды.

Пользовательские секреты и сертификаты не входят в ZIP. Архив проверяется через whitelist, SHA-256 всех распакованных файлов и отдельную сборку/целевые тесты распакованной копии. Старые архивы B0–B8 сохраняются.

B9 готов в указанном объёме. B10–B16, frontend, HA/tenant deployment и финальная приёмка всей платформы остаются последующими этапами.
