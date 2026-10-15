# Проверки B12.3b — 14 октября 2026

go fmt ./..., go vet ./... и общий go test ./... успешны. Общий прогон выполнен без opt-in TEST_DATABASE_URL/Agent binary; он не заменяет DB/device acceptance. Backend собран для Windows amd64 и Linux amd64.

На отдельной PostgreSQL 18, 127.0.0.1:55432, прошли LogIncident/LogSeverity/LogReview/OpenAPI, AssetLogs, AssetRegistryUpgrade, Logbook, HardwareIncident и Correlation tests: основной DB storage прогон 69.286s. После уточнения source hashes, offline event semantics и receipt-time qualification отдельно повторены актуальные LogIncident/LogSeverity/LogReview/OpenAPI tests — успешны, storage 11.273s, health 1.481s, API 3.211s.

Проверено:

- группа нескольких severity records, сохранение критического нуля, escalation без снижения severity;
- replay без второго счёта; отсутствие привязки по чужому hostname/placeholder; разделение Collector sources и simulation key;
- наличие normalized log signal и совместного case с CPU problem; log review не закрывает CPU;
- count conflict и pending-event conflict; новая occurrence после review; review retry без второго audit;
- запрет viewer и роль, отозванная после формирования principal; закрытые JSON тела и required confirmation/count/reason;
- immutable source-record references, masked bounded process label, source/process hashes; отсутствие raw/message/secret в incident proof;
- старый original timestamp не обновляется перечитыванием; unknown/highly uncertain clock не используется; задержка worker не теряет принятый event;
- отсутствие silent recovery от тишины; уже принятый event сохраняется после disconnect с source_unavailable;
- rollback после принудительного SQL failure оставляет исходный log и pending, повторная обработка создаёт правильный incident;
- старые log ingestion, поиск/raw ownership, B12 hardware/correlation и populated legacy schema upgrade сохраняют работоспособность.

Первый DB прогон выявил неверное выведение типа SQL timestamp в freshness query. Добавлен явный timestamptz cast; повторные прогоны успешны. Это исправленная проверками ошибка, а не оставленное ограничение.

Проверки использовали реальные PostgreSQL transactions и фикстуры неизменённого protobuf-контракта. Отдельного реального syslog/Linux/Agent стенда команды в этом этапе нет. Agent reference 215d33a4a5fbc2a12c57d31e516b0ad33f7ae574 остаётся без локальных изменений. Commit/push не выполнялись.

Пакет cumulative: прежние B0–B11, B12.1, Prometheus/Alertmanager, B12.3a hardware и B12.3b logs. Упаковщик проверяет paths и SHA256 каждого распакованного файла; исключает Git metadata, tools, test DB, .env, private keys и runtime. Старые ZIP не заменяются. B12 без-severity audit/pattern rules, drift/topology producers, B13–B16 и финальная командная приёмка остаются отдельной работой.
