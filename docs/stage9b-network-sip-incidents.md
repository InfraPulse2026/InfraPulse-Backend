# Этап9B — инциденты интерфейсов и SIP

Продолжение уже реализовано в [этапе9C](stage9c-flap-collector-incidents.md):
interface flap и collector-loss. Перечень дальнейшей работы в этом документе
описывает состояние этапа9B.

Этап продолжает health/incidents из9A. Agent и protobuf не менялись, frontend
не создавался. Backend использует уже передаваемые InterfaceRates, oper_state,
CounterAvailability и TelephonyHealth. GitHub на этом шаге не обновляется.

## Что добавлено

| Правило | Warning | Critical | Recovery |
|---|---|---|---|
| interface.oper_down | — | устойчивый down/lowerlayerdown при известном admin_up=true | устойчивый up при admin_up=true |
| interface.utilization | >=80% | >=95% | <=70% |
| interface.errors | RX+TX >=1 ошибка/сек | >=10/сек | <=0.1/сек |
| interface.drops | RX+TX >=10 drops/сек | >=100/сек | <=1/сек |
| sip.availability | устойчивый degraded | устойчивый down | устойчивый up/healthy с reachable и ответом2xx |
| sip.latency | >=250мс | >=1000мс | <=150мс |

Все sustain/recovery требуют минимум3 разных observed_at и длительность
max(6секунд,2ожидаемых интервала). При более частой фактической передаче нужны
дополнительные точки до этой длительности. Одиночный timeout/spike не является
подтверждённым инцидентом. ConsecutiveFailures от Agent сохраняется в исходной
телефонии, но не заменяет несколько собственных наблюдений Backend.

Новые resource thresholds фиксированы в версии1 правил, отдельно от настраиваемых
CPU/RAM ENV. Точные thresholds возвращаются GET /api/health/rules и сохраняются
в resource_policy evidence. API/ENV изменения этих новых thresholds пока нет.
Ожидаемая cadence берётся из уже существующих monitoring/telephony policies:
это поддерживает интервалы3секунды..24часа и не меняет scheduler Agent.

## Почему нет ложного «всё нормально»

Rates используются только когда InterfaceRates реально присутствует и его
availability разрешает обе нужные стороны. Отсутствующая маска сохраняет
прежнюю семантику P0. Накопительные InterfaceCounters не используются как
errors/second или bytes/second, и Backend не вычисляет их delta самостоятельно.
Известные0rates означают измеренный0, отсутствующие/invalid — unknown.

Utilization = max(RX bytes/sec, TX bytes/sec) ×8 / speed bits/sec ×100.
Направления full duplex не складываются:80%RX+80%TX не превращается в160%.
Speed отсутствует/0, rates отрицательные/NaN/Infinity или utilization>100%
дают unknown, а не придуманную загрузку. Нужны rates и корректная скорость порта.

Известное административное выключение или неизвестная доступность admin state
не выдаются за oper outage. Прежний открытый incident не закрывается только
потому, что порт отключили административно. Unknown oper state также не означает
up. Пропажа интерфейса из частичной telemetry не создаёт событие «кабель вынули»;
последнее наблюдение со временем станет stale.

SIP timeout без latency не становится0мс. Ответ401 обычно означает доступность
SIP endpoint с проблемой аутентификации и классифицируется Agent как degraded,
а не healthy. Availability и latency — разные правила: быстрый401 может иметь
нормальную latency, но не подтверждает исправную SIP availability. Здоровое
availability recovery требует reachable, state up/healthy и response code2xx.
OPTIONS не проверяет RTP, качество голоса или возможность реального звонка.

## Идентичность и хранение

Миграция013 расширяет incidents полями resource_kind/resource_id/resource_context.
Старые CPU/RAM incidents имеют resource_kind=device, прежние ID/fingerprint
и occurrence сохраняются. Interface incident принадлежит конкретному Agent,
device, interface ID и source. Один port ID на разных устройствах не объединяется.

SIP incident принадлежит telephony target, а не Device. Для него device_id в
JSON пустой, resource_kind=sip, resource_id=target ID. В SQL device_id=NULL,
target_id имеет настоящий FK на telephony_targets. Фиктивное устройство для
SIP не создаётся.

Resource_context SIP — SHA-256(address,transport). При смене endpoint история
версий не объединяется; здоровый новый endpoint не закрывает incident старого.
При возвращении к старому endpoint нужны новые непрерывные наблюдения его
собственного контекста. Старые contexts помечаются current=false/stale.
Source=simulated дополнительно изолируется по Agent session.

Resource projection выполняется в той же Ingest transaction, что inbox,
telemetry/telephony, incident, transitions и evidence. SQL failure откатывает
всё до application ACK. Replay не создаёт второй incident/evidence. Сохраняется
прежний lifecycle open → acknowledged → resolved и новая occurrence после recovery.

## Evidence и единицы

Для CPU/RAM сохраняется прежний evidence/telemetry_id. Новые правила используют
resource_evidence: record_id, record_kind (telemetry/telephony), observed_at,
value и, для SIP availability, reported state.

Для ресурсов используйте assessment.value + assessment.unit:
percent, per_second, milliseconds, boolean или state_grade. State_grade0=up,
1=degraded,2=down. Value_percent заполняется только для utilization; latency и
ошибки не маскируются под проценты. resource_policy и rule_version задают
порог/алгоритм решения. Исходный payload доступен в прежнем архиве/истории, а
ссылки на записи позволяют проверить измерения. В evidence не копируются
raw credentials, адрес с паролем или весь inventory.

## REST API для разработчика frontend

| Маршрут | Результат |
|---|---|
| GET /api/devices/{id}/health?agent_id=... | CPU/RAM/trend плюс interface components |
| GET /api/devices/{id}/interfaces/health?agent_id=... | оценки наблюдённых портов |
| GET /api/devices/{id}/interfaces/{interface_id}/health?agent_id=... | оценки конкретного порта |
| GET /api/telephony/targets/{id}/health?agent_id=... | availability/latency по SIP contexts |
| GET /api/incidents?resource_kind=interface&resource_id=... | фильтр по типу/ID ресурса |
| GET /api/incidents?resource_kind=sip&resource_id=... | инциденты SIP target |
| GET /api/health/rules | все9 rule IDs и resource thresholds |

Чтения требуют viewer/operator/admin. Acknowledge operator/admin остаётся прежним,
с CSRF при cookie. У новых read routes нет возможности отправить command.
OpenAPI обновлён до0.12.0; добавлены generic numeric units и resource DTO fields.

Health cards возвращают coverage=observed_components_only: это оценки тех
компонентов, по которым есть наблюдения, не гарантия здоровья всей сети.
Каждая projection ограничена600records; truncated сигнализирует о достижении
лимита либо возможной неполной сводке. Используйте endpoint конкретного порта,
если общий список слишком велик. Нет наблюдений — status unknown, а не healthy.
Current=false history не входит в текущую общую категорию. Incident signal —
последняя сохранённая оценка; evidence_status отдельно учитывает freshness,
подключение Agent и актуальный SIP endpoint context.

## Что реально проверено

Domain tests: full duplex utilization, rates vs counters, availability masks,
известный/неизвестный admin state, down, missing speed, SIP down/degraded/latency,
recovery, single timeout и future timestamps, смена endpoint.

PostgreSQL tests: interface incident/replay/recovery/card, device health с портами,
SIP target без fake device, endpoint isolation и последующее recovery исходного
endpoint, SQL failure с откатом inbox/evidence и успешным единственным retry.
Эти storage scenarios используют deterministic fixture clock и синтетические
protobuf samples, не объявляются физическим отказом железа.

Отдельный process scenario запускает настоящий неизменённый Agent, локальный
UDP SIP fixture, Controller и изолированный PostgreSQL. Fixture начинает с401,
переходит к200, затем перестаёт отвечать. Agent присылает свои результаты;
Backend открывает один incident, повышает warning до critical после устойчивого
down и закрывает ту же occurrence после устойчивых200. При escalation допустимо
больше двух evidence records: opened, severity_changed, recovered.
Это реальный transport/collector loop на тестовом endpoint, не промышленная PBX.

Также выполняются предыдущие tests CPU/RAM, auth, команды/rollback и PKI.
Docker/native Linux runtime/реальное Cisco-MikroTik/PBX deployment не заявляются
проверенными в этой среде.

Пока остаются interface flap, отдельный collector-loss incident, correlation
с topology/service dependencies, reports и AI. Наличие port down рядом с SIP
ошибкой само по себе не объявляется доказанной причиной. Автоматический ремонт
не запускается; guarded typed commands/rollback остаются отдельными API.
Пункт10 ещё не закрыт целиком. Полностью готовыми остаются8из10 исходных пунктов.
