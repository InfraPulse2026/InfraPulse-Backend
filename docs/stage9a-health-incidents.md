# Этап9A — health rules и история инцидентов

Это описание этапа9A. Сетевые/SIP правила уже добавлены в
[этапе9B](stage9b-network-sip-incidents.md); список ещё не реализованных network
rules ниже относится к состоянию9A, а не новому архиву9B.

Это первый шаг пункта10: CPU/RAM, условный CPU forecast и incident lifecycle.
Reports, AI, interface/SIP rules и correlation пока не реализованы. Agent/source
protobuf не менялись, frontend не создавался, GitHub не обновлялся.

## Как это работает по шагам

1. Agent отправляет обычный TelemetryBatch. Новый формат от Agent не требуется.
2. В прежней транзакции сохраняются inbox и telemetry.
3. Backend выбирает до120 последних измерений одного agent/device/source.
   Source=simulated дополнительно изолируется по Agent session, чтобы разные
   демонстрационные запуски не складывались в одну аварийную историю.
4. Смена boot_id, большой разрыв измерений, отсутствующие/некорректные значения
   прерывают непрерывность. Будущие timestamps не участвуют в правилах.
   Одинаковый observed_at считается одной точкой, независимо от числа записей.
5. Правила оценивают свежесть, устойчивость, пороги и при наличии данных тренд.
6. Новый подтверждённый сигнал создаёт incident; повторный обновляет его.
   Agent/device/rule/source/simulation session определяют fingerprint.
7. Inbox, metric, health, incident, transition и evidence COMMIT выполняются
   вместе. Только после успеха Controller отправляет прежний application ACK.
   Ошибка SQL на incident/evidence не приводит к частично сохранённой записи.
8. Пользователь читает результат через REST. Operator/admin может подтвердить,
   что заметил incident. Это acknowledge, а не исправление устройства.
9. Свежие устойчиво нормальные измерения закрывают incident. Пропажа Agent,
   пустая metric или устаревший sample этого не делают. Новая проблема после
   закрытия создаёт новую occurrence с новым incident ID.

Работа начинается при следующем новом telemetry envelope после обновления
Backend. Имеющаяся недавняя история используется для окна; автоматического
создания исторических incidents за весь прошлый архив нет. Нового scheduler
или опроса оборудования на Backend не добавлено.

## Первые правила

| Rule | Warning | Critical | Условие восстановления |
|---|---|---|---|
| cpu.sustained | CPU>=80% | CPU>=95% | CPU<=70%, устойчиво |
| ram.sustained | used/total>=80% | used/total>=95% | used/total<=75%, устойчиво |
| cpu.trend | достижение CPU critical ожидается в ближайшие300с при сохранении тренда | уже устойчиво достигнут CPU critical | подтверждённая нормализация/стабильный нерисковый тренд |

Sustain/recovery требуют минимум3 различных времени наблюдения и длительность
не меньше max(6секунд,2ожидаемых интервала). При более частой фактической передаче
используются дополнительные точки до нужной длительности, а не только3последних.
Выборка ограничена120points. Один spike или dip не подтверждает аварию/recovery.

RAM без total, с total=0 или used>total получает unknown, не0%. CPU вне0..100,
NaN/Infinity или отсутствующий optional value также не считается нормальным.
Известный0% является нормальным измерением.

Forecast требует известный стабильный boot_id, минимум6points, окно не меньше
max(30секунд,5ожидаемых интервалов), достаточно согласованный линейный тренд
(R²>=0.9), и положительный slope для прогнозируемого достижения порога.
Предупреждение ниже обычного warning возможно: синтетические50/55/60/65/70/75%
с шагом6секунд дают estimate24секунды до95% при сохранении этой линии.

Это условная экстраполяция, не гарантия будущей аварии и не доказанная причина
потери связи/SIP. R² показывает соответствие линии наблюдениям, не вероятность.
Нечёткий/шумный тренд и недостаточные данные дают unknown. Для Device без
boot_id CPU/RAM thresholds работают, но forecast не делается.

History window max(20минут,12ожидаемых telemetry интервалов), не больше120точек.
Ожидаемый интервал берётся из существующего monitoring policy устройства либо
backend default. Его нужно согласовать с фактическим YAML Agent, поскольку
live ConfigUpdate всё ещё отсутствует. Интервалы до24ч учитываются без жёсткого
предположения «каждый Agent обязан присылать sample каждые3секунды».

## Состояния и evidence

Health assessment: ok, pending, warning, critical, unknown, stale. Pending
означает замеченный, но ещё не устойчивый spike. Severity alone не заменяет
status/freshness. Device health возвращает status unknown, если компонент
неизвестен/устарел/ожидает подтверждения и нет warning/critical компонента.
Это не вычисленный процент здоровья сети; общего score на этом этапе нет.

Incident: open → acknowledged → resolved. Acknowledge не меняет metric и
не отправляет Agent команду. Повторный acknowledge не создаёт второй transition.
Closed incident acknowledge возвращает409. После resolved новая авария получает
новый ID и occurrence+1, а не переписывает прошлое событие.

Incident signal — последняя сохранённая оценка; evidence_status отдельно сообщает:
fresh, stale, collector_unavailable или historical для закрытого incident.
Старый warning не объявляется текущим исправным состоянием при потере данных.
GET device/health переоценивает свежесть по времени чтения и подключению Agent;
старые simulated sessions тоже отмечаются stale. Это read projection, не ложное
событие «device offline» и не автоматическое закрытие incident.
У observations есть current: старые simulated sessions возвращаются как история
с current=false и не портят общую категорию здоровья текущего запуска.

Evidence snapshots сохраняются при открытии, смене severity и восстановлении.
Они содержат telemetry IDs, observed_at, процентные значения, window/slope/R²/ETA,
применённую policy и rule_version. Смена thresholds позже не стирает основания
старого решения. Raw telemetry payload, credentials и полный inventory в
evidence не копируются. Для последних неподтверждённых сигналов достаточно
health_observations и текущего incident.signal; отдельного evidence каждый3сек
не создаётся. Evidence/transitions API отдаёт до200 последних записей.

## REST API

| Метод | Маршрут | Назначение |
|---|---|---|
| GET | /api/health/rules | policy, rule IDs и минимальные требования к данным |
| GET | /api/devices/{id}/health?agent_id=... | оценки по source/правилам и общая категория |
| GET | /api/incidents | список с фильтрами и пагинацией |
| GET | /api/incidents/{id} | карточка, последний signal и evidence_status |
| GET | /api/incidents/{id}/evidence | сохранённые основания решения |
| GET | /api/incidents/{id}/transitions | история lifecycle |
| POST | /api/incidents/{id}/acknowledge | body {"confirmed":true}, operator/admin |

Список фильтруется agent_id, device_id, source, rule_id, state
(open/acknowledged/resolved). Limit1..200 default100, offset0..100000.
Все чтения требуют viewer/operator/admin. Acknowledge требует incidents.write
(operator/admin), текущую сессию и CSRF при cookie. Транзакция повторно проверяет
роль/сессию; событие и access audit пишутся вместе. Development bypass не даёт
неавторизованному пользователю возможность acknowledge через storage.
OpenAPI версия0.11.0 содержит эти routes и Incident/HealthAssessment DTO.

## Конфигурация и запуск

Запуск Backend прежний. Миграция012 добавляется автоматически и не переписывает
старые таблицы telemetry. Новые настройки можно задать через окружение для
go run либо .env для Docker Compose. Требуется recovery < warning < critical,
в диапазоне0..100, иначе server не запускается с ошибочной policy.

```text
HEALTH_CPU_WARNING=80
HEALTH_CPU_CRITICAL=95
HEALTH_CPU_RECOVERY=70
HEALTH_RAM_WARNING=80
HEALTH_RAM_CRITICAL=95
HEALTH_RAM_RECOVERY=75
HEALTH_FORECAST_HORIZON_SECONDS=300
```

Horizon60..3600секунд. Изменение этих ENV требует перезапуска Backend; оно не
меняет scheduler Agent. Новая policy применяется к следующему telemetry update.
API изменения правил и per-device overrides на этом шаге не реализованы.

## Проверки и границы

Domain tests: high CPU, critical, spike, stable recovery, раннее предупреждение
ниже80%, отрицательный/шумный тренд, неизвестные metrics, future/stale/duplicate
observations, reboot/gap, более частая cadence и одиночное падение значения.

PostgreSQL tests: одна открытая occurrence на повторный сигнал, replay,
idempotent acknowledge, поздние/future данные без ложного recovery, stable
recovery с evidence, новая occurrence, source isolation, collector loss,
early forecast projection и атомарный rollback inbox/telemetry/incident/evidence
при искусственной SQL ошибке. Для детерминированного lifecycle storage tests
используют внутренние fixture clocks и синтетические protobuf samples;
это не выдаётся за аварию реального железа или стенд с физической нагрузкой.

API tests проверяют роли и confirmation; existing cookie CSRF middleware
сохраняется. Общий прогон также запускает неизменённый Agent для прежних
SIP/interface-command/rollback/PKI integration сценариев.

Пока нет правил для interface rates/utilization/flap, SIP incidents,
отдельных collector-loss incidents, topology correlation, service dependencies,
reports, AI context/export и action proposals. Пункт10 остаётся незавершённым.
Нет автоматического remediation: существующие typed commands/rollback можно
выполнять только по их прежним guarded API, отдельно от acknowledge.
