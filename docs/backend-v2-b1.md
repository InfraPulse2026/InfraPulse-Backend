# Backend v2 B1 — совместимость Collector Syslog и Redfish

Дата: 2026-10-10. Проверяемый Collector:
`d456fea4ac583641ec6fc65e5570e687174f8d40` из InfraPulse2026/InfraPulse-Agent main.
Исходники Collector не менялись. Backend синхронизирован с его canonical proto;
отличается только `go_package`. IPMI в этом commit отсутствует.

## Что реализовано

1. Additive protobuf sync: SourceRef, ObservationTime/Metadata, SyslogMetadata,
   HardwareInventory/Status, ComponentHealth, ResourceRelationship и
   IdentityEvidence. Inventory completeness принимается как метаданные снимка.
2. Валидация metadata в controller и перед storage: collector_id не может
   подменять stream identity; source/provenance/clock quality проверяются;
   duration/timestamps, списки и текст ограничены. Старые сообщения без metadata
   принимаются. Невалидные данные не получают ACK.
3. Полные аппаратные поля сохраняются в существующих inventory/telemetry JSONB
   внутри транзакции inbox. История сохраняет hardware metrics, component status,
   relation resource IDs, identity evidence и время источника. Никакой Asset
   resolution по этим evidence в B1 не выполняется.
4. Миграция 017 добавляет nullable syslog/observation metadata к log_records;
   старые строки остаются. Доступны через прежние log list/detail API.
5. Метаданные Syslog отделены от заявленного hostname. Authenticated/managed —
   сведения, сообщённые Collector, а не самостоятельная проверка syslog TLS
   сервером backend. Полная transport authentication выполняется на Collector.
6. Текст structured_data и дополнительных Syslog полей проходит существующую
   дополнительную фильтрацию секретов. Binary raw_payload не выдаётся поисковым
   REST: raw_payload_omitted отмечает наличие пропущенных октетов. Оригинальный
   sanitized-by-Collector payload остаётся в inbox, который не имеет публичного
   REST endpoint. Это не обещание полной очистки любых произвольных секретов.
7. Business dedup учитывает source identity и Syslog evidence, но не изменившееся
   время повторного чтения файла. Legacy file record может получить новые metadata
   без дубликата, только если его прежний business fingerprint совпадает.
8. ACK после COMMIT и envelope replay остаются прежними. Обновлены OpenAPI 0.17.0
   и optional схемы новых полей; 71 прежний защищённый маршрут сохранён.

## Как читать данные через API

```text
GET /api/devices/{id}/inventory?agent_id=...
  hardware, observation, identity_evidence
  components[].hardware, components[].observation

GET /api/devices/{id}/telemetry?agent_id=...
  sample.hardware_health[]
  sample.metrics[].resource_id/component_id/status/observation
  sample.observation, sample.identity_evidence

GET /api/logs?agent_id=...
GET /api/logs/{id}?agent_id=...
  syslog, observation, raw_payload_omitted
```

Все эти endpoints требуют data.read. Вложенные inventory/sample используют
protobuf JSON: snake_case, uint64/int64 как строки, duration как `0s`/`1.5s`.
Присутствующий optional ноль сохраняется. Отсутствующий offset/uncertainty
означает неизвестное значение, а не нулевую погрешность. ReceivedAt задаёт backend.
SourceTimestamp сохраняется отдельно; часы источника автоматически не исправляются.

PowerState=Off не превращается в Health=Critical. ResourceRelationship не
становится сетевым кабелем. Partial/empty snapshot не удаляет прежние устройства.
Hardware aggregate health/sensor rules и global Assets относятся к B8/B3.

Redfish по умолчанию Collector опрашивает медленнее Linux metrics. До B9
ожидаемые интервалы backend нужно отдельно выставлять через device monitoring
API в соответствии с YAML Collector; эта API пока не меняет его scheduler.

## Проверки

Frozen P2/Stage9F descriptor tests проходят; fixtures не регенерировались.
Generator check подтверждает canonical source и воспроизводимые generated Go.
Использованы protoc 33.5, protoc-gen-go 1.36.12, protoc-gen-go-grpc 1.6.0.

Новые PostgreSQL tests проверяют аппаратные данные, replay, Syslog redaction,
конфликт business ID и полный откат inbox при ошибке проекции, rejected spoof,
сохранение legacy file row при enrichment и upgrade populated Stage9F через 017.
Тест upgrade выполняется только в отдельной disposable schema; повтор миграции
не теряет старые строки.

Процессный test `TestUnmodifiedCollectorV2RedfishSyslogReplayAndREST` запускает
неизменённый настоящий Collector с durable queue. Он получает данные от
синтетического HTTPS BMC с проверкой CA и Basic auth ресурсов, принимает remote
Syslog по loopback TCP, доставляет в настоящий PostgreSQL и читает через
авторизованный REST. После COMMIT искусственно теряется первый ACK: повтор
доставки не дублирует лог. При остановленном controller listener второй лог
накапливается у Collector и приходит после восстановления соединения.
Syslog authenticated=false в этом TCP fixture честно сохраняется; mTLS sender
attribution проверяется unit validation, но не этим процессным TCP сценарием.

```powershell
$env:TEST_DATABASE_URL = 'postgres://...@127.0.0.1:55432/test?sslmode=disable'
$env:AGENT_ROOT = 'путь к неизменённому Collector d456fea'
$env:AGENT_BINARY = 'путь к старому P3 Agent executable'
$env:COLLECTOR_V2_BINARY = 'путь к новому Collector executable'
go test -count=1 ./...
```

Без env process/database tests пропускаются. Сборка Windows/Linux — отдельная
проверка и не заменяет Linux runtime/race. Docker, Linux-only durable E2E,
реальный BMC и реальный AI provider локально не проверены.

Полный uncached regression 2026-10-10: PASS; process integration 91.233s,
storage 81.095s. После добавления populated upgrade проверки её отдельный
запуск PASS, как и затронутые API/controller/metadata tests и vet.

## Дальше

B2 добавляет DataSource framework; B3 — Asset/SourceBinding/provenance и
сопоставление конфликтующих идентификаторов. B1 не создаёт Zabbix/Prometheus
connectors, Ansible, IPMI, централизованную конфигурацию Collector или UI.
