# Этап9E — связанный контекст инцидента

Backend теперь возвращает связанные симптомы и основания их связи. Это
детерминированная contextual diagnosis, ещё не AI-анализ и не доказанная причина
аварии. Agent/protobuf не менялись, frontend не создавался, GitHub не обновлялся.

## Что делает новый endpoint

GET /api/incidents/{id}/context?window_seconds=300&limit=20
требует обычной viewer/operator/admin авторизации. Window60..3600секунд,
limit1..50. Ответ содержит исходный incident, related incidents, relation,
strength, basis, topology_links, limitations, timestamp/version и truncated.
causal_claim всегдаfalse. Запрос не отправляет команду или данные внешнему AI.

Типы связи:

| Relation | Основание |
|---|---|
| same_collector_temporal_context | тот же Agent и пересечение/близость episodes |
| collector_observability_context | Collector входит в путь получения наблюдений |
| same_device_context | тот же agent-scoped device ID |
| same_resource_context | тот же kind/ID/context/source ресурса |
| fresh_discovery_adjacency | актуальная однозначно resolved discovery связь устройств |
| port_and_neighbor_symptom_candidate | проблемный порт совпал с port ID LLDP/CDP наблюдения к соседу |

Strength weak/moderate/strong_context отражает силу контекстного основания,
не вероятность причины. Например, две проблемы SIP/CPU у одного Collector
получают слабую временную связь, но это не доказывает, что CPU вызвал SIP timeout.
Collector context объясняет ограничение наблюдаемости, не физический отказ узлов.

## Время и идентичность

Контекст строится внутри одной Repeatable Read transaction: seed, candidates,
их live evidence freshness, inventory и current topology согласованы между собой.
Пересечение времени использует backend opened/resolved episode timestamps;
дополнительное окно допускает близкие соседние episodes. Оngoing incident
может пересекаться с более новым событием; одинаковое время открытия не требуется.

Связи ограничены одним Agent. Real и simulated classification не объединяются,
а simulated sessions должны совпасть. Context/resource IDs SIP версий сохраняются;
один target ID с разными endpoints не объявляется одним ресурсом.
SQL предварительно отсеивает несовместимые scopes, чтобы они не занимали
всю bounded candidate страницу.

## Как учитывается топология

Для topology relation обе проблемы должны быть ongoing, иметь fresh evidence
и текущий signal warning/critical. Device IDs должны быть известны. Берётся
current Agent discovery: наблюдение не из будущего, TTL ещё действует,
resolution однозначный/high confidence. Hostname-only medium, ambiguous,
unresolved и expired связи не служат основанием topology relation.

Link содержит edge ID, endpoints, port IDs, protocol/source/scope и времена,
а не raw credentials/весь neighbor payload. Stronger port candidate требует
точного соответствия interface ID и LLDP/CDP. MNDP discovery может показать
соседство в общем segment, но не усиливает связь до port candidate.

Current discovery не восстанавливает карту на время давно закрытого incident.
Для resolved/stale/unknown seed topology relation не строится. Temporal/resource
контекст истории остаётся возможным и явно отличается от текущего соседства.
SIP target не связывается с устройством только по похожему address/hostname:
operator-declared service/routing dependencies в этом модуле не настроены.

## Ограничения выборки и состояние данных

До200candidate incidents,500topology observations,2000inventory устройств,
не более10links на один related candidate и до50related items в ответе.
Превышение любого применённого лимита отражается truncated. Результат является
контекстом доступной выборки, не исчерпывающим dependency graph всей сети.
При прочих равных более сильные основания идут раньше, затем время/ID.

EvidenceStatus каждой incident отдельно показывает fresh/stale/collector
unavailable/historical. Incident state=open не означает, что его старый symptom
подтверждён свежим измерением. Missing данные не превращаются в recovery.
Context не изменяет state, severity, acknowledge или исходный evidence.

## Проверки

Domain tests покрывают exact port/high-confidence fresh adjacency без causal
claim, expiry/weak resolution, разные Agent/simulation scopes, SIP без service
mapping, collector observability, historical seed, temporal bounds и truncation.
PostgreSQL test поднимает два agent-scoped устройства и реальную projection
LLDP с chassis MAC resolution, проверяет relation, expiry и resolved seed.
Это synthetic Agent envelopes/SQL incident fixtures, не физическая network outage.
API test проверяет protected viewer read и query limits.

OpenAPI0.15.0 описывает DiagnosticContext/Related/TopologyLink. Endpoint не
требует новой схемы БД: он использует уже сохранённые facts. Общий прогон сохраняет
прежние реальные неизменённые Agent SIP/commands/rollback/PKI process fixtures.

## Следующий шаг

Этот context станет одним из источников ограниченного AI export DTO.
Для внешнего provider нужны отдельные preview, редактирование/псевдонимизация
данных, явная policy отправки и проверенный structured result. На этом шаге
ни provider, ни внешний запрос, ни автоматическое remediation не включались.
ConfigUpdate требует согласования с владельцем Agent. Пункт10 ещё не полностью
закрыт, готовых исходных пунктов8из10.
