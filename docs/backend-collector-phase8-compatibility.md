> Обновление B9: разделы о выключенном управлении ниже описывают исходное состояние B1. В текущем backend управление реализовано и включается явно; см. [B9](backend-v2-b9-collector-configuration.md). При выключенном флаге прежнее поведение сохранено.

# Совместимость Backend с финальным Collector Phase 8A

Дата проверки: 11 октября 2026. В GitHub main Agent обнаружен commit
`215d33a4a5fbc2a12c57d31e516b0ad33f7ae574`:
`fix(agent): устранены дефекты Collector и завершена Phase 8A`.
Он получен в отдельный reference checkout; source code и git status Agent
не изменены. Это обновление backend поверх B4.1, не реализация всего B9/B7.

## Почему потребовалась правка

Предыдущий backend был синхронизирован с Collector `d456fea`. В новом canonical
proto добавлены identity subject/origin/time/normalization, extended identity
lists, topology metadata/advertised addresses, metadata SIP/events/services/
networks/firewall и managed-config messages. Backend отклоняет неподдерживаемые
protobuf fields, поэтому просто запускать новую версию без sync было нельзя.

Backend agent.proto теперь совпадает с canonical proto `215d33a`, кроме
backend `go_package`. Go-код перегенерирован закреплёнными protoc 33.5,
protoc-gen-go 1.36.12, protoc-gen-go-grpc 1.6.0. Старые field numbers/types,
oneof members и RPC не переписаны: frozen P2 и Stage9F tests сохранены.

## Что работает в этой версии

- Новые hello/heartbeat, inventory/telemetry и metadata принимаются прежним
  AgentService.Connect. Полные snapshots сохраняются в PostgreSQL в прежней
  транзакции; ACK отправляется после commit.
- Обе identity sections сохраняются в inventory/telemetry JSONB. Origin,
  subject_kind, original_value, normalization и fact-specific time не теряются.
  В Asset identity projection обе секции рассматриваются вместе. Invalid/
  rejected diagnostics остаются evidence с `usable=false`, а не кандидатом.
- Явная typed identity, называющая собственный Device/resource и имеющая
  соответствующий subject kind, корректно распознаётся как root identity.
  Interface/component/neighbor subjects не поднимаются на всё устройство.
  В evidence JSON сохраняется исходный subject, даже если индексированная
  root projection использует пустой subject_id.
- Fingerprint игнорирует identity observation clocks, но сохраняет subject,
  origin и normalization. Повторный poll не создаёт версию лишь из-за времени.
  Automatic resolution дополнительно проверяет возраст/clock quality времени
  high root serial/UUID. По умолчанию автоматический режим всё ещё выключен.
- ConfigApplyStatus проверяется на matching Collector identity, revision,
  digest и допустимый status, архивируется и дедуплицируется. Это пассивный приём
  сообщения, не подтверждение достижения desired configuration.
- Topology/telephony/events и local service/network/firewall snapshots сохраняют
  новые поля в protobuf JSONB; существующие маршруты и legacy projections
  продолжают работу. Превращение полного нового Neighbor evidence в глобальный
  граф относится к B7; текущий REST граф показывает прежнюю нормализованную форму.

## Что сознательно ещё не включено

`HelloAck.managed_config_v1=false`: backend пока не реализует B9 negotiation,
desired snapshot/revision/digest reconciliation и REST управления targets.
Backend не отправляет DesiredConfigSnapshot. Remote management Collector
не включается молча, независимо от его capability и local opt-in.
Прежние typed interface commands и rollback остаются в своём механизме.

Наличие protobuf messages или archived config status не означает, что интервалы
Collector можно уже менять из UI. Новый контракт закрывает прежнее отсутствие
возможности со стороны Agent; реализация Backend B9 теперь возможна без правок
Agent. Политика local credential_ref/ca_ref, mTLS и UUID/session checks обязательна.

## Проверки

- Canonical proto comparison и воспроизводимая генерация backend.
- Frozen P2/Stage9F wire compatibility, новые metadata/subject/status validations.
- PostgreSQL: extended diagnostics, root subject, inventory сохранение,
  ConfigApplyStatus commit/replay без выдуманного состояния APPLIED.
- Process E2E с бинарником, собранным из неизменённого `215d33a`: Redfish verified
  HTTPS CA, Syslog, gRPC, commit → lost ACK → replay, outage recovery и REST auth.
  Этот тест использует локальные synthetic protocol endpoints, не реальное железо.
- Регрессия старого P3 Agent, commands/rollback/PKI/SIP и baseline REST.

Полная проверка live managed configuration остаётся в B9. IPMI UDP/реальный BMC,
production deployment, Docker и Linux-only process checks этой проверкой
не подтверждены. Agent Phase8 report с mock Controller не заменяет эти проверки.
