# Этап 6: типизированные команды Agent

Этот этап закрывает пункт 4 списка: таблицы команд, журнал состояний, создание
через REST, отправка по существующему gRPC stream и обработка CommandResult.
Исходники Agent, protobuf и frontend не меняются. Изменения подготовлены
локально; GitHub пока не обновлён. ZIP включает все предыдущие этапы.

## Что реально можно выполнить

В текущем Agent исполняются interface enable/disable для RouterOS REST.
Backend этого этапа принимает именно эти действия. Произвольного shell/CLI,
argv или тела HTTP-запроса оператор передать не может. Service start/stop/restart
имеет поля в protobuf, но у Agent пока нет исполнителя; API их не выдаёт за
работающие действия. Rollback API относится к следующему этапу.

Мутации по умолчанию выключены. Для отдельного подготовленного стенда нужны:

```text
COMMANDS_ENABLED=true
COMMAND_ALLOW_DISABLE=true
```

Второй флаг нужен только для disable. Backend по умолчанию защищает lo, eth0,
ether1; COMMAND_PROTECTED_INTERFACES и COMMAND_PROTECTED_DEVICES добавляют
защищённые ресурсы. Они не снимают обязательную защиту по умолчанию.
В существующем YAML Agent отдельно должны быть commands.enabled=true и
enable_mutations=true у нужного RouterOS device. Источники Agent не редактируются.
Дополнительную защиту зависимых bridge/bonding/VLAN ресурсов проверяет сам Agent.

## Проверки перед созданием и перед отправкой

Команду создаёт вошедший operator/admin с правом commands.write. Viewer может
читать команды и результаты, но не создавать их. Cookie требует CSRF как
другие мутации. В режиме AUTH_MODE=disabled командное управление запрещено:
у действия должен быть конкретный пользователь.

Backend требует действующий Agent, его текущую session, квалифицированное
online устройства, RouterOS driver/source и supported capability
interfaces.enable или interfaces.disable. Inventory и Device.LastSeen не
старше пяти минут; последнее корректное измерение интерфейсов — не старше 30s.
Будущие ошибочные samples остаются в архиве и не блокируют новые корректные.
InterfaceMetadata.admin_status должен подтверждать, что admin_up действительно
измерен. Отсутствие измерения не заменяется false.

По reference ID/name/default-name/MAC должен находиться ровно один интерфейс.
Все эти идентификаторы проверяются против защиты: переименование ether1 не
разрешает отключить его по каноническому ID. Также защищается интерфейс,
несущий известный management IP устройства. Agent перед записью снова
разрешает текущее имя/ID и проверяет собственную policy.

Требуемое исходное admin_up сверяется с telemetry. Эта precondition не является
полем protobuf Command, поэтому backend не придумывает новое wire-поле.
Короткое окно между наблюдением и физической записью остаётся: перед записью
Agent самостоятельно читает ресурс; сквозной атомарной CAS на оборудовании
текущий контракт не обещает.

## Запрос и защита от двойного нажатия

POST /api/commands, Content-Type application/json, один заголовок
Idempotency-Key длиной 1..128 printable ASCII bytes:

```json
{
  "agent_id": "collector-uuid",
  "device_id": "router-1",
  "interface_id": "ether2",
  "operation": "disable",
  "expected_admin_up": true,
  "timeout_seconds": 30,
  "confirmed": true
}
```

Timeout — целое число 1..60s, confirmed обязательно true, initial state
обязательно true или false. ID устройства/интерфейса — максимум 128 UTF-8 bytes
без control characters. Backend задаёт command ID, requested_at и HUMAN initiator.

Ключ уникален внутри пользователя. Тот же ключ и те же параметры возвращают
тот же command, HTTP 200 и Idempotent-Replay:true. Другие параметры под тем же
ключом дают 409. Новый запрос возвращает 202: это принятие в очередь, не успех.
На wire idempotency_key равен глобально уникальному command ID: одинаковые
браузерные ключи разных пользователей не смешиваются в Agent journal.

## Сохранение, отправка и состояния

Миграция 009 добавляет controller_commands, command_events, command_results.
Создание и запись пользователя/action в access_audit атомарны. До сетевой
отправки состояние dispatching уже фиксируется в БД. Права, срок, session,
policy и preconditions проверяются ещё раз при claim.

```text
queued → dispatching → sent → accepted/running → terminal result
                  ↘ uncertain при неизвестной доставке
```

Обычный Agent обычно присылает сразу terminal result; accepted/running
поддержаны для совместимости. Terminal состояния: success, failed, rejected,
unsupported, timeout. Неотправленная просроченная команда получает expired.
Unspecified/неподдержанный status сохраняется диагностически и не подтверждает успех.

HelloAck, MessageACK и Command используют один последовательный sender.
Команда получает только ready stream той session, на которой она была создана.
Начатая отправка имеет ограничение времени; после её неопределённого сбоя RPC
закрывается. Ожидание sender можно отменить до отправки без чужой мутации.

Sent означает лишь успешную локальную отправку. Success требует результата
именно этого Agent и matching verification с verified, наблюдённым admin_up,
ожидаемым итоговым состоянием, каноническим интерфейсом и временами проверки.
Неполное success становится uncertain/success_not_verified, не снимая блокировку.
Поле result.status сохраняет, что сообщил Agent; state — оценка backend.

На Agent допускается одна активная команда, включая uncertain. Это соответствует
его последовательному worker и защищает маленькую очередь. Быстрый результат
может прийти ещё во время Send; поздний FinishSend не возвращает success в sent.
Result, event и входящий envelope сохраняются вместе до ACK. После reconnect
допускается replay того же результата в новой session без повторного эффекта.

О неизвестной команде/другом Agent, повторном или конфликтующем terminal result
сохраняется диагностическая запись; известный terminal не перезаписывается.
Дополнительная фильтрация типовых секретов применяется к выдаваемым error/evidence;
исходный архив остаётся как прислан Agent, не обещается полностью обезличенным.

## Потеря связи и перезапуск

После попытки доставки backend не отправляет команду повторно автоматически.
Если результат не пришёл в течение deadline+5s, появляется uncertain.
При рестарте queued отклоняется, dispatching/sent/accepted/running становятся
uncertain. Повторить HTTP-запрос с прежним ключом означает прочитать старую
команду, а не запустить её заново.

Uncertain блокирует новые мутации этого Agent. Definitive поздний результат
может разрешить состояние; автоматически трактовать timeout как «ничего не
изменилось» нельзя. В этом этапе нет force-resend, cancel или ручного unlock
API. Если Agent сообщил конечную неопределённость и definitive результата
уже не будет, требуется ручной разбор; будущий механизм reconciliation должен
явно описать наблюдения и ответственность оператора. Это ограничение MVP.

## API и отображение frontend

| Метод и путь | Назначение |
|---|---|
| POST /api/commands | Создать подтверждённое действие operator/admin |
| GET /api/commands | Список, filters agent_id/device_id/state, limit/offset |
| GET /api/commands/{id} | Текущее состояние, причина и result |
| GET /api/commands/{id}/events | Журнал переходов, oldest first, максимум 1000 |
| GET /api/commands/policy | Флаги backend и защищённые ресурсы |

Limit 1..500, default100; offset0..1000000. Command ID кодируется в URL,
формат cmd_ +32hex. API/schema — OpenAPI0.8.0. В result имеются changed,
uncertain, nullable observed_admin_up, времена, verification и snapshot_id.
Snapshot ID — непрозрачный идентификатор Agent. Содержимое и TTL по этому wire
не передаются, поэтому backend не выдумывает «снимок точно живёт 24 часа».
Отправка RollbackAction и реестр наблюдённых snapshot references реализованы
в [этапе 7](rollback-stage7.md); Agent content/TTL по-прежнему неизвестны.

Frontend делает другой разработчик. Для него важны separate «принято» и
«выполнено», отдельное отображение uncertainty и хранение прежнего
Idempotency-Key при повторе одного намерения. Флаг changed=false не равен
ошибке; read-after-write verification описывает наблюдённый результат.

Ошибки: 400 invalid_request; 401 без действующей сессии; 403 forbidden/
commands_disabled/protected_resource; 409 command_precondition/agent_command_busy/
idempotency_conflict; 404 not_found; 503 при недоступном storage/dispatch.

## Что проверено

Домен, API, gRPC dispatch, PostgreSQL lifecycle и обычный P3 Agent проверены.
Process test TestUnmodifiedAgentRouterOSCommandsThroughAuthenticatedREST запускает loopback HTTPS
RouterOS fixture, реальный Agent, gRPC Controller, worker, PostgreSQL и REST auth.
Команда disable ether2 дала verified success+snapshot; replay не создал PATCH;
enable вернул исходное состояние; protected ether1 получил 403 без записи.
Fixture содержит ровно два PATCH. Пример ответа — docs/examples/commands.fixture.json.
Для process test обязательны AGENT_BINARY и TEST_DATABASE_URL.

Это тест протокола и работы системы на локальном fixture, не проверка реального
MikroTik. Docker runtime, нативный Linux deployment и race detector в текущей
среде не заявляются проверенными. Подробные результаты — validation.md.
В объёме MVP готовы пункты 1,2,3,4,8,9; остаются rollback, live config, production
PKI и incidents/reports/AI. GitHub не обновлён, Agent исходники не менялись.
