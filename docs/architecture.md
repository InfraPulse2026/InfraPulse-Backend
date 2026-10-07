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
Он не доказывает доступность удалённых устройств. REST отдельно возвращает
последнее `online` и текущий `agent_connected`, вычисляемый по server receipt
time и AGENT_TIMEOUT. Устройства не удаляются при завершении stream.

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
