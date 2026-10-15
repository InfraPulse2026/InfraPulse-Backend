# Backend InfraPulse: следующие этапы после сквозного MVP
Локально реализованы topology (пункт 1 списка), availability expiry (пункт 8)
и logs/audit (пункт 2), SIP/telephony (пункт 3), REST auth/roles (пункт 9), команды (пункт 4), snapshot/rollback (пункт5), PKI enrollment/renewal (пункт7).
Подробности: [этап 1](topology-stage1.md), [этап 2](availability-stage2.md),
[этап 3](logs-audit-stage3.md), [этап 4](telephony-stage4.md), [этап 5](access-stage5.md), [этап 6](commands-stage6.md).
Эти изменения пока не опубликованы на GitHub. В объёме MVP реализованы 8 из
10 пунктов: 1, 2, 3, 4, 5, 7, 8, 9. Результаты проверок записываются отдельно в
[validation.md](validation.md). Остаются
live config (6) и incidents/reports/AI (10). Детали PKI — [этап8](stage8-pki.md).

Начат пункт10: CPU/RAM/trend, evidence и lifecycle incidents реализованы
в [этапе9A](stage9a-health-incidents.md), interface/SIP rules —
в [этапе9B](stage9b-network-sip-incidents.md), flap/collector-loss —
в [этапе9C](stage9c-flap-collector-incidents.md), reports —
в [этапе9D](stage9d-reports.md), contextual diagnosis —
в [этапе9E](stage9e-diagnostic-context.md), AI workflow —
в [этапе9F](stage9f-ai-analysis.md). Реальный provider acceptance и регулярный
report scheduler ещё остаются. До acceptance подтверждённых пунктов8из10.



Ниже описаны целевые расширения; конкретный реализованный объём каждого модуля
зафиксирован в документах этапов выше. Исходный Agent не меняется. Frontend разрабатывает другая
команда; здесь определяются данные, расчёты и API, которые backend даст ей.

## Что уточнили организаторы

Фиксированного списка производителей и моделей нет; демонстрацию можно строить
на одном устройстве и виртуальной сети. Интересны популярные зарубежные и
российские вендоры. В письменном кейсе указаны шесть вендоров, в устном уточнении
допускается гибкость. Практический выбор — сохранить шесть имеющихся profiles
Agent, а стенд проверять на доступном зарубежном и российском устройстве или
явно обозначенной эмуляции. Generic SNMP profile не доказывает поддержку всех
моделей производителя: возможности и уровень проверки показываются отдельно.

Минимум — карта сети и контроль здоровья, предпочтительно с ранним обнаружением
проблем. Обновление оперативных метрик около 3 секунд, остальные интервалы
можно менять. Разрешены open source, искусственные сценарии и синтетические
логи. Безопасность передаваемых данных должна быть объяснима. Управляемое
исправление по предложению ИИ — желательная демонстрационная возможность.

## Продуктовая идея, которую должен обеспечить backend

Система выдаёт связанное объяснение:

> На uplink наблюдаются рост загрузки и ошибки. Одновременно ухудшается SIP.
> Возможная причина — деградация этого соединения. Вот измерения, события,
> затронутые по известным зависимостям узлы и проверяемый план действий.

Это пример целевого ответа, а не уже найденный реальный инцидент. Backend
вычисляет измеримые факты и кандидатов причин. ИИ объясняет подготовленные
данные и предлагает действия. Он не придумывает показания приборов.

Цепочка: наблюдения → ранний сигнал → incident → evidence → вероятная причина
→ оценка влияния → предложение → контролируемое исполнение → новая telemetry.

## Краткий ресерч

| Система | Подтверждённая функция | Что берём в наш backend |
|---|---|---|
| [Zabbix](https://www.zabbix.com/documentation/current/en/manual/appendix/functions/prediction) | forecast и timeleft для оценки будущего значения/времени до порога | устойчивые правила и объяснение прогноза; прогноз сам по себе не объявляем уникальностью |
| [Prometheus](https://prometheus.io/docs/introduction/overview/) | временные ряды, labels, правила alerting | структурированные метрики, стабильные идентификаторы правил, разделение наблюдения и предупреждения |
| [NetBox](https://netbox.readthedocs.io/en/stable/) | документирование устройств, местоположений и соединений | отделение справочных данных о сети от наблюдаемого состояния |
| [LibreNMS](https://docs.librenms.org/Extensions/Network-Map/) | карта из discovery, включая LLDP/CDP | исходные наблюдения и явное основание каждой связи |

Решение для хакатона — развивать существующий Go + PostgreSQL backend. Полная
интеграция всех четырёх систем сейчас добавит настройку, а не готовый сценарий
InfraPulse. Позже можно добавить импорт NetBox и export собственных метрик
backend в Prometheus. Это рекомендация по текущему проекту, не утверждение,
что эти системы не умеют диагностику или автоматизацию.

## B1. Наблюдаемая сеть: первый следующий этап

В MVP все envelopes уже сохраняются. Следующий этап добавляет индексируемые
проекции и API поверх тех же сообщений, не второй транспорт и не новые drivers.

### Топология и инвентаризация

Новые сущности: `topology_snapshots`, `topology_observations`, `topology_links`,
`device_metadata`. Существующий `devices.inventory` остаётся полной исходной
моделью. Metadata backend хранит операторские site/room/rack/unit и назначение
устройства отдельно от автоматически полученных значений.

Для topology сохраняются device/interface IDs, raw chassis/port IDs с subtype,
protocol/source, observed/received/expiry times, confidence, evidence и
completeness. Один линк может иметь несколько подтверждающих observations.
Если remote узел не сопоставлен уверенно, он остаётся отдельным unresolved
узлом. Совпадение hostname не основание автоматически объединять оборудование.

Полный свежий snapshot может обновить набор наблюдений. Partial/failed snapshot
не удаляет старые связи. Истечение срока делает связь stale/expired; это не
доказательство, что кабель выдернули. Наблюдение MNDP на общем segment тоже не
доказывает прямое соединение. Source=simulated сохраняется у узлов и связей;
реальные и синтетические графы не объединяются незаметно.

Планируемые маршруты:

- `GET /api/topology` → nodes, links, evidence, freshness, unresolved nodes;
- `GET /api/devices/{id}/interfaces?agent_id=...` → нормализованные интерфейсы;
- `GET/PATCH /api/devices/{id}/metadata?agent_id=...` → физическое расположение;
- `GET /api/topology/changes` → история наблюдаемых изменений.

Важно: topology обнаруживается по отдельному медленному интервалу, не каждые
3 секунды. Фактическое oper_state интерфейса обновляется fast telemetry и может
изменить статус уже известного линка, не выполняя полный LLDP walk.

Готовность B1: Simulator → graph API; один линк с двумя observations не
дублируется; partial snapshot не стирает карту; неизвестный сосед виден;
stale topology и потеря Agent показаны отдельно от подтверждённого отказа.

### Logs, events и telephony

Индексируемые `logs`, `events`, `telephony_observations` извлекаются из envelope
в той же транзакции. Дедупликация не только по message_id: устойчивый record/event
ID с agent/source предотвращает повтор при новом envelope. Telephony target
принадлежит наблюдающему Agent; отсутствующая latency не превращается в ноль.

Планируемые маршруты:

- `GET /api/logs?agent_id=...&device_id=...&severity=...&from=...&to=...&q=...`;
- `GET /api/events?device_id=...&agent_id=...`;
- `GET /api/telephony` и `GET /api/telephony/{id}/history`.

Запросы ограничены диапазоном, размером страницы и сортировкой; поиск использует
параметры SQL. Severity protobuf LogRecord — числовая syslog severity, а не
произвольный строковый enum. Нормализация должна учитывать эту шкалу.

## B2. Health, ранние сигналы и incidents

Сначала правила над данными, затем язык объяснения. Каждое правило возвращает
`rule_id`, severity, reason, evidence references, время наблюдения и качество
данных. Если метрика отсутствует или устарела, результат `unknown`/`stale`,
а не healthy. Общий score сопровождается проблемными составляющими.

Первые правила:

1. Устойчивая нагрузка CPU и RAM; короткий всплеск не создаёт новый incident.
2. Рост нагрузки: slope и оценка достижения порога при сохранении тренда.
3. Ошибки/drop rates интерфейса и oper_state; счётчики нельзя трактовать как rates.
4. Utilization интерфейса только при наличии и скорости порта, и traffic rates.
5. Interface flap: несколько изменений состояния за временное окно.
6. SIP: рост latency, timeout, consecutive failures, восстановление.
7. Collector unavailable: отдельно от offline удалённых устройств.

Тренд считается по observed_at, на одном agent/device/metric/source. Нужны
достаточное число samples, временная длительность, свежесть, положительный
устойчивый slope и приемлемый разброс. Window, warning horizon, пороги и
cooldown конфигурируются. Отрицательный slope, нехватка данных или смена boot_id
не дают уверенного прогноза. Не публикуем вероятность в процентах без
калиброванной модели. Пишем: «при сохранении текущего тренда» и показываем
количество точек/длительность окна.

CPU полезен для раннего предупреждения, но сам по себе не доказывает причину
SIP-ошибок. Для краткого демо используем заранее подготовленные и явно
синтетические samples с согласованными timestamp. Не уменьшаем требования к
доказательствам незаметно ради эффектного прогноза за две точки.

Новые сущности: `health_observations`, `incidents`, `incident_evidence`,
`incident_transitions`, `service_dependencies`.

Incident имеет fingerprint agent/device/rule/resource/source. Повторяющийся
сигнал обновляет один incident, а не создаёт новый каждые 3 секунды. Lifecycle:
open → acknowledged → resolved; recovered signal фиксирует восстановление
после заданного стабильного периода. Новая авария после закрытия — новая
occurrence с отдельным временем.

Планируемые маршруты:

- `GET /api/devices/{id}/health?agent_id=...`;
- `GET /api/incidents` и `GET /api/incidents/{id}`;
- `POST /api/incidents/{id}/acknowledge`;
- `GET /api/incidents/{id}/evidence`.

Корреляция объединяет близкие по времени симптомы при наличии связей/зависимостей.
Например, oper_down uplink + offline downstream дают кандидата причины.
Несколько деградаций SIP/traffic без oper_down дают гипотезу, а не доказанный
отказ. Если Agent сам пропал, downstream device-offline нельзя считать
независимыми доказательствами физического отказа.

Оценка влияния сервисов требует `service_dependencies`: физическая adjacency
сама не показывает маршрутизацию, резервирование и прохождение SIP traffic.
Без этих данных backend возвращает «потенциально связанные узлы», а не
«точно отказавшие сервисы». What-if допускается как отдельный модельный
расчёт с перечислением предположений, не как факт наблюдения.

Готовность B2: предупреждение до заданного critical threshold; единичный spike
не создаёт поток incidents; evidence воспроизводимо; отсутствие данных видно;
восстановление закрывает incident после стабильного периода.

## B3. AI analysis и контроль данных

Backend формирует ограниченный пакет контекста: incident, подтверждённые
измерения, несколько релевантных logs, соседние узлы и capability matrix.
Источник каждого факта сохранён. История всей сети и credentials не уходят
в модель. Исходные logs считаются данными, а не инструкциями для ИИ.

Перед внешним запросом `POST /api/incidents/{id}/analysis/preview` возвращает
точный export DTO: что отправится, какие поля скрыты, какие IDs псевдонимизированы,
сколько строк выбрано, выбранный provider и режим. Это позволяет на защите
показать ответ на вопрос «что уходит из нашей сети».

После настройки provider `POST /api/incidents/{id}/analysis` сохраняет
`ai_analyses` с reference IDs, structured explanation, гипотезами, рекомендациями,
версией prompt/model и временем. Внешний AI по умолчанию выключен. Если provider
недоступен, incident и его evidence остаются доступны; deterministic explanation
не выдаётся за ответ LLM.

Agent уже скрывает известные secrets, но не гарантирует полную анонимизацию
текста. Поэтому нужен backend export allowlist и повторная фильтрация. IP,
hostname, serial, audit UID могут раскрывать инфраструктуру даже без паролей.
Псевдонимизация должна быть стабильной внутри анализа, чтобы сохранить связи.

## B4. Управляемые действия

AI предлагает только типизированное действие из реально объявленных capabilities.
Первый конкретный кандидат — `interface.enable` на изолированном тестовом порту
RouterOS с заранее известным состоянием. CPU warning не оправдывает произвольный
restart или отключение management-порта.

До исполнения нужны пользовательская авторизация, операторское подтверждение,
policy check, защита management interfaces, stale-state check, audit trail,
idempotency key и timeout. REST auth/roles и журнал действий доступа добавлены
в этапе 5. Этап 6 реализует подтверждённые interface enable/disable для поддерживаемого
RouterOS через Agent; остальные действия и AI proposals остаются дальнейшим планом.

Сущности controller_commands/command_results и журнал переходов уже добавлены;
action_proposals и полный AI approval workflow остаются планом. Целевая цепочка:
создать предложение → одобрить → проверить policy →
отправить существующий protobuf Command → принять CommandResult → прочитать
новую telemetry и verification.

`Send` не означает успех. `uncertain=true`, timeout и потеря stream сохраняются
как неопределённый/неподтверждённый результат. Повторное нажатие не создаёт вторую
мутацию. Agent P3 уже имеет snapshot_id и typed RollbackAction. Backend пока
реализует выдачу typed rollback и отслеживание snapshot references в этапе7.
Универсальный rollback не обещается; обратная interface-команда тоже требует проверки.

Simulator сейчас отвечает на команды unsupported. Нельзя показывать его
самостоятельное recovery как результат исполненной AI-команды. Для проверки
управления нужен настоящий изолированный RouterOS либо явно тестовый executor,
а его демонстрация отдельно помечается.

## B5. Повторяемость и отчёты

`GET /api/reports/summary?from=...&to=...` объединяет recurring incidents,
самые проблемные устройства, длительности деградации, качество/полноту данных,
число предупреждений до critical threshold. Затем внутренний scheduler
сохраняет регулярные snapshots с уникальным ключом периода. Процент доступности
публикуется только при известном покрытии наблюдений и явной методике.

Минимум для демо — JSON отчёт по завершённому сценарию. PDF/почтовая рассылка
не входят в первую итерацию backend.

## Порядок выполнения и ограничения объёма

| Порядок | Готовый результат | Почему в этом порядке |
|---|---|---|
| 0 | проверить deployment MVP на выбранной среде | стабильная база всех дальнейших сценариев |
| 1 | topology + logs/events/telephony projections и read API | получить контекст, на который опирается диагностика |
| 2 | health rules + incidents + recovery | доказать раннее предупреждение и объяснимость |
| 3 | ограниченный AI context, preview и analysis | ИИ получает структурированные факты |
| 4 | один guarded action поверх auth/audit этапа 5 | исполнение только на поддерживаемом стенде |
| 5 | сводный отчёт и фиксация demo acceptance | закрыть требования и сделать результат повторяемым |

Следующий независимый модуль — health rules/incidents/reports/AI. PKI/enrollment, snapshot/rollback,
commands/auth, SIP и локальные пользователи добавлены
в текущую копию; UI разрабатывается отдельно.
Live ConfigUpdate требует согласованного расширения контракта с владельцем Agent:
в текущем agent.proto соответствующее controller message отсутствует.
Миграции 001..003 опубликованной базы не переписываем. Новые таблицы этапов
идут в 004..011. Не добавляем Kafka, микросервисы и несколько СУБД без необходимости.

Обязательное ядро защиты: работающая цепочка Agent → база → API, наблюдаемая
карта, объяснимый health/incident, logs и SIP context. Главная демонстрационная
связка — раннее предупреждение с evidence и последующим восстановлением.
Управление по кнопке — следующий слой при готовом безопасном стенде.
Победа не гарантируется; качество оцениваем по воспроизводимой демонстрации,
покрытию кейса и честным границам возможностей.


## Backend v2 B9 — managed Collector configuration

Реализованы admin REST desired/applied, полные snapshots и canonical digest финального Collector, monotonic revisions, durable stream delivery/status history, runtime proof и наследование применённого polling в freshness. Включение требует explicit backend flag, mTLS и local Collector opt-in. Подробно: [B9](backend-v2-b9-collector-configuration.md). Следующие стадии окончательного плана — B10–B16; фронтенд и code Agent вне текущей работы.

## Backend v2 B10 — Ansible read-only automation

Добавлены отдельный Linux Worker, реестр фиксированных read-only шаблонов Cisco IOS/Arista EOS/VyOS, admin Asset/SSH bindings, durable jobs/claims/cancel/expiry/audit, безопасные события и encrypted configuration/facts snapshots. Backend-реализация проверяется локально, настоящая Ansible/device приёмка требует Linux-стенда. Подробно: [B10](backend-v2-b10-ansible.md), [валидация](backend-v2-b10-validation.md). Далее — B11 drift, B12 multisource incidents, B13 AI, B14 chronic/reports, B15 TSDB/Grafana, B16 итоговая безопасность/HA/acceptance. Исторические описания старых контрактов выше не заменяют актуальные B9/B10.

## Backend v2 B11.1 — expected/actual и drift

Добавлены immutable admin-approved snapshot baseline, versioned conservative normalizer, automatic redacted diff, history/freshness/source guards и related reviewed VoIP context. [Инструкция](backend-v2-b11-1-configuration-drift.md), [валидация](backend-v2-b11-1-validation.md).

## Backend v2 B11.2 — безопасное remediation

Добавлены versioned per-Asset policy, разрешённые VLAN, protected interfaces, idempotent request, отдельное live-admin approval, повторные preflight-проверки во время исполнения, один закрытый action `restore_access_vlan` для Cisco IOS/Arista EOS и автоматическая recollection/verification. Произвольные команды и playbooks не принимаются. [Инструкция](backend-v2-b11-2-remediation.md), [валидация](backend-v2-b11-2-validation.md). B11 завершён на уровне backend; реальная Linux/SSH/device приёмка остаётся обязательной перед демонстрацией. Далее B12–B16.

## Backend v2 B12.1 — durable multi-source incident foundation

Добавлены migration 035, durable projection queue, normalized signals поверх прежних incidents, Asset/service/site/topology scoring, timeline, повторная обработка и explicit all-signals recovery. [Описание](backend-v2-b12-1-correlation.md). B12 целиком ещё открыт: Prometheus alerts, log/hardware/drift/topology producers и полная multi-source приёмка — следующие шаги.

## Backend v2 B12.2a — Prometheus firing alerts

Добавлены opt-in alerts_enabled, bounded /api/v1/alerts, однозначный selected-target mapping, устойчивые events/incident IDs и передача в B12.1 queue. Отсутствие alert не считается recovery. [Инструкция](backend-v2-b12-2a-prometheus-alerts.md). Explicit resolved delivery, hardware/log/drift/topology producers и полная B12 приёмка остаются.

Добавлен [B12.2b — explicit resolved delivery Alertmanager](backend-v2-b12-2b-alertmanager.md). Историческое ограничение отсутствия resolved выше относится к первоначальному B12.2a.

## Backend v2 B12.3a — hardware vendor health

Migration 038, durable sample queue, explicit Warning/Critical, stable three-sample OK recovery, typed hardware evidence и correlation producer. Agent не изменён. [Описание](backend-v2-b12-3a-hardware-incidents.md). Следом — log/drift/topology producers; B12 целиком пока открыт.

## Backend v2 B12.3b — reported log event producer

Migration 039, durable queue, source-owned five-minute grouping по явной severity, source-record evidence, correlation и защищённый operator review без утверждения device recovery. Agent не изменён. [Описание](backend-v2-b12-3b-log-incidents.md). B12 целиком открыт: audit/pattern rules без severity, drift/topology producers и общая приёмка.

## Backend v2 B12.3c — explicit audit failure

Добавлено audit.reported_failure по явному success=false, nullable исходный severity, общая durable очередь и защищённый review. [Описание](backend-v2-b12-3c-audit-incidents.md). Agent не изменён. Pattern rules, drift/topology и общая приёмка B12 остаются.

## Backend v2 B12.4 — configuration drift producer

Добавлены Asset-only configuration incidents по последнему свежему B11 сравнению, durable queue/cursor, explicit same-baseline compliance recovery, correlation и API/OpenAPI 0.40.0. [Описание](backend-v2-b12-4-configuration-incidents.md). Исторические comparisons/unknown/смена baseline не закрывают drift. Agent не изменён. Topology, оставшиеся pattern rules и общая B12 приёмка впереди.

## Backend v2 B12.5a — reported neighboring endpoint changes

Добавлены durable topology snapshot queue/baseline, conservative COMPLETE LLDP/CDP peer change rule, immutable before/after references, guarded operator review и correlation. [Описание](backend-v2-b12-5a-topology-incidents.md). TTL expiry/absence/partial не означают отказ. Agent не изменён. Общая B12 приёмка и оставшиеся pattern/дополнительные topology rules впереди.

## Backend v2 B12 — завершён локальный функциональный объём

[Заключение и границы B12](backend-v2-b12-final.md): полный набор MVP producers, pattern catalog, normalized signals, evidence/timeline, structural correlation и all-members recovery проверены локально. Живая командная приёмка остаётся B16. Исторические пометки «B12 открыт» выше относятся к промежуточным стадиям. Следующий этап B13 — multi-source AI context.

## Backend v2 B13 — multi-source AI workflow

[Контекст и API B13](backend-v2-b13-ai.md): case export, pseudonymization, local ref mapping, case confirmation/history, bounded rate-limit retry и uncertainty готовы локально. Реальный вызов модели команды ещё нужен для acceptance. Старый workflow сохранён. Следующий этап B14 — asset/site chronic reports и scheduler.
