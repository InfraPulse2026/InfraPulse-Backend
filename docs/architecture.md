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
рост sequence, валидность дат и конечность чисел. Пропуски sequence допустимы:
текущая RAM-очередь Agent может терять сообщения.

Backend сначала фиксирует регистрацию и устройство в транзакции, затем
отправляет `HelloAck{accepted:true, protocol_version:1}`. В существующем контракте
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
запись одинакового envelope. Повтор ID с изменёнными данными отклоняется.
Hello при reconnect восстанавливает регистрацию, сохраняя историю измерений.

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

Файлы `agent.proto`, `agent.pb.go`, `agent_grpc.pb.go` скопированы из Agent ZIP
без изменения. Protobuf package, service names и field numbers совпадают.
Сам Agent не редактировался. Обычной сборке `protoc` не нужен.

`go_package` в proto и descriptor сохраняет имя модуля Agent; backend импортирует
локальную копию generated package под собственным module path. Для будущей
генерации из общего proto используй mapping, не меняя wire contract:

```sh
protoc -I api/proto \
  --go_out=api/gen --go_opt=paths=source_relative \
  --go_opt=Minfrapulse/v1/agent.proto=github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1 \
  --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
  --go-grpc_opt=Minfrapulse/v1/agent.proto=github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1 \
  api/proto/infrapulse/v1/agent.proto
```

Оба проекта должны согласовывать изменения proto. Не добавляй поля HelloAck
только на стороне backend и не меняй номера существующих полей.
